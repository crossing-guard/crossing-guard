package engine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The wire form of an event (schemas/event.schema.json 1.0; team plan §5.13).
//
// This is the one record the client can already produce end to end. The encoder is
// portable on purpose: the server decodes the same structs. Three rules it enforces:
//
//   - an ABSOLUTE PATH NEVER SHIPS. Inside the session's checkout a file target becomes a
//     repo-relative path; outside it the target is {kind:file, outside_repository:true}
//     with no path and no digest, because a digest of a low-entropy path is
//     dictionary-reversible;
//   - the chain's tags_digest is computed from the frozen bytes BEFORE redaction, so what
//     ships can be redacted without breaking verification (invariant 13);
//   - redaction is a last pass with the role-agnostic secret patterns over reason, tag
//     values, and evidence. Today's frozen tags carry no raw command at all.

type WireSession struct {
	ID        string `json:"id"`
	Runtime   string `json:"runtime"`
	NativeID  string `json:"native_id"`
	CatalogID string `json:"catalog_id,omitempty"`
	ResumeID  string `json:"resume_id,omitempty"`
}

type WireActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type WireWorkspace struct {
	RepositoryID *string `json:"repository_id"`
}

type WireProvenance struct {
	Kind           string `json:"kind"`
	ContentHash    string `json:"content_hash"`
	RedactionStage string `json:"redaction_stage"`
}

type WireChain struct {
	Seq      int64  `json:"seq"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

type WireTarget struct {
	Kind              string  `json:"kind"`
	RepositoryID      *string `json:"repository_id,omitempty"`
	Path              string  `json:"path,omitempty"`
	OutsideRepository bool    `json:"outside_repository,omitempty"`
	Name              string  `json:"name,omitempty"`
}

type WireTag struct {
	Key        string `json:"key"`
	Value      string `json:"value"`
	Detector   string `json:"detector"`
	Provenance string `json:"provenance"`
	Evidence   string `json:"evidence,omitempty"`
}

type WirePayload struct {
	Verb       string      `json:"verb"`
	Tool       string      `json:"tool"`
	Target     *WireTarget `json:"target"`
	Tags       []WireTag   `json:"tags"`
	TagsDigest string      `json:"tags_digest"`
	Decision   string      `json:"decision"`
	Reason     string      `json:"reason"`
	Origin     string      `json:"origin"`
	Rule       *string     `json:"rule"`
	Layer      *string     `json:"layer"`
}

type WireEvent struct {
	Schema        string         `json:"schema"`
	SchemaVersion string         `json:"schema_version"`
	ID            string         `json:"id"`
	OccurredAt    string         `json:"occurred_at"`
	RecordedAt    string         `json:"recorded_at"`
	Sequence      int64          `json:"sequence"`
	Session       WireSession    `json:"session"`
	Actor         WireActor      `json:"actor"`
	Workspace     WireWorkspace  `json:"workspace"`
	Type          string         `json:"type"`
	Visibility    string         `json:"visibility"`
	Sensitivity   []string       `json:"sensitivity"`
	Provenance    WireProvenance `json:"provenance"`
	Chain         *WireChain     `json:"chain"`
	Payload       WirePayload    `json:"payload"`
}

// WireEventInput is everything the encoder needs, as the store holds it.
type WireEventInput struct {
	DeviceID       string
	GlobalID       string
	TS             int64  // Unix seconds, as event.ts
	ReceivedAt     int64  // Unix seconds; 0 = unknown, falls back to TS
	Session        string // "<vendor>/<native id>"
	Runtime        string
	CatalogID      string
	ResumeID       string
	Verb           string
	Tool           string
	TargetEntityID string
	FrozenTags     string // the exact stored tags JSON
	Decision       string
	Reason         string
	Origin         string
	Rule           string // the rule that produced or asked for the decision; "" = unknown
	Layer          Layer  // the distribution tier that rule arrived by; "" = unknown
	ChainSeq       int64
	PrevHash       string
	Hash           string
	RepositoryID   string // "" when the session's repository is unknown
	CheckoutRoot   string // absolute; used only to relativize, never shipped
}

// WireSessionID is stable for a (device, session) pair so a re-push dedupes and the server
// has one key for a session. Callers pass the NORMALIZED "<runtime>/<native id>": live
// hooks record a bare native id with the runtime in its own column while imports record
// "<vendor>/<id>", and both must land on one wire session (postwork W2).
func WireSessionID(deviceID, session string) string {
	return DeterministicTypedID("ses", deviceID+":"+session)
}

// WireSessionParts is the one normalization of a stored session to its wire runtime and
// native id: a prefixed "<vendor>/<id>" yields the id, the runtime column wins over the
// prefix when set, and an empty part reads "unknown". WireSessionID keys on
// runtime+"/"+native, so every producer of a session's wire id calls this.
func WireSessionParts(runtimeColumn, session string) (runtime, native string) {
	runtime, native = runtimeColumn, session
	if i := strings.Index(session, "/"); i > 0 {
		if runtime == "" {
			runtime = session[:i]
		}
		native = session[i+1:]
	}
	if runtime == "" {
		runtime = "unknown"
	}
	if native == "" {
		native = "unknown"
	}
	return runtime, native
}

// sensitivityClasses is the wire sensitivity vocabulary: the water-mark ladder.
var sensitivityClasses = func() map[string]bool {
	out := make(map[string]bool, len(waterOrder))
	for _, v := range waterOrder {
		out[v] = true
	}
	return out
}()

var verbShape = regexp.MustCompile(`[^a-z0-9_]+`)

// EncodeWireEvent renders one stored event as schema-valid wire JSON.
func EncodeWireEvent(in WireEventInput, dets []Detector) ([]byte, error) {
	if !IsTypedID(in.GlobalID) {
		return nil, fmt.Errorf("event global id %q is not a typed id", in.GlobalID)
	}
	runtime, native := WireSessionParts(in.Runtime, in.Session)
	var frozen []WireTag
	if strings.TrimSpace(in.FrozenTags) != "" {
		if err := json.Unmarshal([]byte(in.FrozenTags), &frozen); err != nil {
			return nil, fmt.Errorf("frozen tags are not the expected shape: %w", err)
		}
	}
	sensitivity := []string{}
	seen := map[string]bool{}
	tags := make([]WireTag, 0, len(frozen))
	for _, t := range frozen {
		// The recorded tag value stays as stored; only the sensitivity class is canonical,
		// and dedupe keys on it (the schema makes sensitivity uniqueItems).
		if class := CanonicalDataClass(t.Value); t.Key == DataClassKey && sensitivityClasses[class] && !seen[class] {
			seen[class] = true
			sensitivity = append(sensitivity, class)
		}
		t.Value, _ = RedactText(PortableText(t.Value, in.CheckoutRoot), dets)
		t.Evidence, _ = RedactText(PortableText(t.Evidence, in.CheckoutRoot), dets)
		tags = append(tags, t)
	}
	if len(sensitivity) == 0 {
		sensitivity = []string{"unknown"}
	}
	verb := verbShape.ReplaceAllString(strings.ToLower(in.Verb), "_")
	if verb == "" || verb[0] < 'a' || verb[0] > 'z' {
		verb = "unknown"
	}
	origin := in.Origin
	if origin == "" {
		origin = "live"
	}
	// sandbox-hook is reserved (team plan §7.1 posture B): observations a sandboxed static
	// tier spooled and the daemon later ingested travel under their own provenance.
	kind := map[string]string{"live": "native-runtime-event", "transcript": "derived-record", "imported": "imported",
		"sandbox-hook": "sandbox-hook"}[origin]
	if kind == "" {
		return nil, fmt.Errorf("event origin %q has no wire provenance kind", origin)
	}
	digest := TagsDigest(in.FrozenTags)
	contentHash := digest
	var chain *WireChain
	if in.Hash != "" {
		contentHash = "sha256:" + in.Hash
		chain = &WireChain{Seq: in.ChainSeq, PrevHash: in.PrevHash, Hash: in.Hash}
	}
	received := in.ReceivedAt
	if received == 0 {
		received = in.TS
	}
	reason, _ := RedactText(PortableText(in.Reason, in.CheckoutRoot), dets)
	var rule *string
	if in.Rule != "" {
		r := in.Rule
		rule = &r
	}
	var layer *string
	if in.Layer != "" {
		l := string(in.Layer)
		layer = &l
	}
	var repo *string
	if in.RepositoryID != "" {
		r := in.RepositoryID
		repo = &r
	}
	ev := WireEvent{
		Schema: "crossing-guard.event", SchemaVersion: "1.0", ID: in.GlobalID,
		OccurredAt: time.Unix(in.TS, 0).UTC().Format(time.RFC3339),
		RecordedAt: time.Unix(received, 0).UTC().Format(time.RFC3339),
		Sequence:   in.ChainSeq,
		Session: WireSession{ID: WireSessionID(in.DeviceID, runtime+"/"+native), Runtime: runtime, NativeID: native,
			CatalogID: in.CatalogID, ResumeID: in.ResumeID},
		Actor:       WireActor{Type: "agent", ID: "runtime:" + runtime},
		Workspace:   WireWorkspace{RepositoryID: repo},
		Type:        "action." + verb,
		Visibility:  "organization",
		Sensitivity: sensitivity,
		Provenance:  WireProvenance{Kind: kind, ContentHash: contentHash, RedactionStage: "after-hash"},
		Chain:       chain,
		Payload: WirePayload{Verb: in.Verb, Tool: in.Tool,
			Target: PortableTarget(in.TargetEntityID, in.CheckoutRoot, in.RepositoryID),
			Tags:   tags, TagsDigest: digest, Decision: in.Decision, Reason: reason, Origin: origin, Rule: rule, Layer: layer},
	}
	return json.Marshal(ev)
}

// PortableTarget turns a local entity id into something that means the same on another
// machine. It never returns an absolute path.
func PortableTarget(entityID, checkoutRoot, repositoryID string) *WireTarget {
	if entityID == "" {
		return nil
	}
	kind, rest, ok := strings.Cut(entityID, ":")
	if !ok || rest == "" {
		return &WireTarget{Kind: "other", Name: "unparsed"}
	}
	switch kind {
	case "file":
		if checkoutRoot != "" && repositoryID != "" {
			if rel, err := filepath.Rel(checkoutRoot, rest); err == nil {
				rel = filepath.ToSlash(rel)
				if rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") && !strings.HasPrefix(rel, "/") && !strings.HasPrefix(rel, "~") {
					r := repositoryID
					return &WireTarget{Kind: "file", RepositoryID: &r, Path: rel}
				}
			}
		}
		return &WireTarget{Kind: "file", OutsideRepository: true}
	case "url", "mcp", "db", "session", "memory":
		return &WireTarget{Kind: kind, Name: rest}
	}
	return &WireTarget{Kind: "other", Name: kind}
}

// homePath matches an absolute path under a user's home or a temporary/private tree — the
// shapes that name a person or a machine — on POSIX and Windows.
var homePath = regexp.MustCompile(`(?:/(?:Users|home|root|private|tmp|var/folders)/[^\s'"\x60,;)]*)|(?:[A-Za-z]:\\[^\s'"\x60,;)]*)`)

// PortableText makes free text safe to leave the device with respect to paths (team plan
// invariant 8; item 4 postwork C1): a path under the session's checkout becomes
// repository-relative, and any other absolute path of a home/temporary shape becomes
// "[absolute path]". Frozen tag evidence ("path=/Users/…/x.md") is the case that
// proved it necessary; reasons and tag values pass through it too.
func PortableText(s, checkoutRoot string) string {
	if s == "" {
		return s
	}
	if root := strings.TrimRight(checkoutRoot, "/"); root != "" && root != "/" {
		s = strings.ReplaceAll(s, root+"/", "")
		s = strings.ReplaceAll(s, root, ".")
	}
	return homePath.ReplaceAllString(s, "[absolute path]")
}

// RedactText replaces every match of a role-agnostic secret pattern with a marker naming
// the detector, and reports which detectors fired. These are the only detectors that can
// fire on role-less text (team plan invariant 7): it is a bounded, named check, never a
// claim that no secret remains.
func RedactText(text string, dets []Detector) (string, []string) {
	if text == "" {
		return text, nil
	}
	var fired []string
	for i := range dets {
		d := &dets[i]
		if d.Disabled || d.Kind != "pattern" || len(d.Roles) > 0 || d.Tag.Key != "secret" || d.re == nil {
			continue
		}
		if d.re.MatchString(text) {
			text = d.re.ReplaceAllString(text, "[redacted:"+d.ID+"]")
			fired = append(fired, d.ID)
		}
	}
	return text, fired
}
