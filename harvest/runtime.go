package harvest

import (
	"fmt"
	"sort"
)

// Runtime is a per-vendor implementation of session parsing + identity.
// Implementations live in claude.go / codex.go and self-register via init(),
// so generic code never names a vendor (ADR 0020). This file grows one method
// at a time as clusters migrate; today it owns session identity.
type Runtime interface {
	Name() string
	// CanonicalID is the stable identity of a session — the key the governor
	// index and enrichment DB are keyed under. It must be derivable from the
	// summary alone.
	CanonicalID(s SessionSummary) string
	// MatchID reports whether a user-supplied id refers to this session
	// (exact, thread-uuid, or vendor-specific suffix).
	MatchID(s SessionSummary, id string) bool
	// Collect discovers this vendor's session files (no parsing).
	Collect() []fileJob
	// Summarize builds a full summary for one file; ok=false skips it (empty).
	// Usage lives on the summary's rail fields (Model, Turns, Context); totals
	// come from recorded calls (token-usage-analytics plan §3.4).
	Summarize(j fileJob) (SessionSummary, bool)
	// Normalize reads a session file into canonical events.
	Normalize(path string) ([]CanonicalEvent, int, *SessionUsage, error)
	// ThreadTitle returns a display-title override for a summary, or "" for
	// none (codex prefers the user-curated thread name; claude has none).
	ThreadTitle(s SessionSummary) string
}

// SessionSource is the optional logical-session capability for runtimes whose native
// store is not one file per session. Runtime deliberately remains unchanged.
type SessionSource interface {
	ListSessionRecords() ([]SessionRecord, error)
	NormalizeSession(SessionRef, bool) ([]CanonicalEvent, int, *SessionUsage, error)
}

type SessionRef struct {
	Runtime      string `json:"runtime"`
	ID           string `json:"id"`
	Source       string `json:"source"`
	Segment      string `json:"segment"`
	UpdateMarker string `json:"update_marker"`
}

type SessionRecord struct {
	Ref     SessionRef
	Summary SessionSummary
}

func sessionSource(runtime string) (SessionSource, bool) {
	source, ok := runtimeFor(runtime).(SessionSource)
	return source, ok
}

// runtimeFor returns the runtime for a vendor name, or nil.
func runtimeFor(name string) Runtime { return runtimes[name] }

var runtimes = map[string]Runtime{}

func register(r Runtime) { runtimes[r.Name()] = r }

// Runtimes returns the registered runtimes (order unspecified) — for callers
// that must iterate vendors instead of hardcoding a list.
func Runtimes() []Runtime {
	out := make([]Runtime, 0, len(runtimes))
	for _, r := range runtimes {
		out = append(out, r)
	}
	return out
}

// RuntimeNames returns registered runtime names in stable (sorted) order. This
// replaces every hardcoded []string{"claude","codex"} — a new vendor joins by
// registering, not by an edit to a literal a caller might forget.
func RuntimeNames() []string {
	out := make([]string, 0, len(runtimes))
	for name := range runtimes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CanonicalID returns a session's stable identity via its runtime, falling back
// to the raw ID for an unregistered runtime.
func CanonicalID(s SessionSummary) string {
	if rt := runtimes[s.Runtime]; rt != nil {
		return rt.CanonicalID(s)
	}
	return s.ID
}

// MatchID reports whether id refers to s, via s's runtime.
func MatchID(s SessionSummary, id string) bool {
	if rt := runtimes[s.Runtime]; rt != nil {
		return rt.MatchID(s, id)
	}
	return s.ID == id || s.ThreadID == id
}

// DeepNormalizer is an OPTIONAL capability: a runtime that can re-read its own
// session file at full fidelity implements it.
//
// It is a separate interface rather than a method on Runtime deliberately. Any
// vendor that cannot do this still works — the console simply cannot expand a
// clipped payload for it and says so — and adding the capability is ADDING an
// interface, never modifying the one every vendor must satisfy (ADR 0020,
// open/closed).
type DeepNormalizer interface {
	// NormalizeFull parses one session file keeping whole payloads. Callers use
	// it for a single event, never for a whole transcript.
	NormalizeFull(path string) ([]CanonicalEvent, int, *SessionUsage, error)
}

// IDShape is an optional adapter optimization. It may reject only identifiers that
// cannot name a session for that runtime; unknown adapters remain conservative.
type IDShape interface{ CouldMatchID(id string) bool }

// RepositoryGrouper is the optional repository-grouping capability: a runtime
// with vendor-specific grouping rules (claude folds worktree checkouts into the
// parent repository) rewrites the derived key. Previously an anonymous
// assertion inside RepositoryGroupKey; named so CapabilityMatrix can report a
// runtime that silently lacks it.
type RepositoryGrouper interface {
	RepositoryGroupKey(s SessionSummary, key string) string
}

// SessionLineage is the optional native-lineage capability: a runtime whose
// store records parent/child session relationships reports them here, observed
// provenance only: the vendor's own files state the relationship.
// An adapter that cannot prove
// a relationship reports none; absence of lineage is never proof of
// independence.
type SessionLineage interface {
	// Lineage returns the observed lineage facts for one session summary.
	// ok=false means this runtime has nothing to report for this session —
	// no parent and no enumerable children.
	Lineage(s SessionSummary) (LineageFacts, bool)
}

// Lineage dispatches SessionLineage; ok=false also covers a runtime without
// the capability, so generic callers never branch on vendor names.
func Lineage(s SessionSummary) (LineageFacts, bool) {
	if lin, ok := runtimeFor(s.Runtime).(SessionLineage); ok {
		return lin.Lineage(s)
	}
	return LineageFacts{}, false
}

// EdgeSource is the optional observed-edge capability: a runtime that records
// cross-session interactions (spawns, messages, waits, context reads) in its
// own files derives them lazily, per session. A full-transcript parse is
// allowed — callers invoke this on demand, never during the summary scan.
type EdgeSource interface {
	Edges(s SessionSummary) ([]Edge, error)
}

// Edges dispatches EdgeSource; ok=false means the runtime has no edge
// capability (distinct from "has the capability, found no edges").
func Edges(s SessionSummary) ([]Edge, bool, error) {
	source, ok := runtimeFor(s.Runtime).(EdgeSource)
	if !ok {
		return nil, false, nil
	}
	edges, err := source.Edges(s)
	return edges, true, err
}

// TurnAnchorer is the optional turn-identity capability: a runtime that can
// name "the turn this event belongs to" composes an opaque anchor string for
// it. Generic code compares anchors only for equality — the composition
// (codex turn_id, claude record uuid) never leaks past the adapter. ok=false
// means this vendor cannot anchor that event; the caller must fall back to a
// coarser boundary, never guess identity from timestamps.
type TurnAnchorer interface {
	TurnAnchor(s SessionSummary, e CanonicalEvent) (anchor string, ok bool)
}

// TurnAnchor resolves the opaque turn anchor for ONE event of a session,
// dispatched like EventText. found=false with no error means the runtime
// cannot anchor turns.
func TurnAnchor(runtime, id string, seq int) (anchor string, found bool, err error) {
	s, ok := Find(runtime, id)
	if !ok {
		return "", false, fmt.Errorf("no %s session %q", runtime, id)
	}
	anchorer, ok := runtimeFor(runtime).(TurnAnchorer)
	if !ok {
		return "", false, nil
	}
	events, _, _, err := NormalizeSummary(s, false)
	if err != nil {
		return "", false, err
	}
	for _, e := range events {
		if e.Seq == seq {
			anchor, ok = anchorer.TurnAnchor(s, e)
			return anchor, ok, nil
		}
	}
	return "", false, fmt.Errorf("session %q has no event %d", id, seq)
}

// ResumeHandle is the optional native-resume capability: a runtime where the
// resumable handle differs from a session's canonical identity (a codex
// subagent rollout is its OWN artifact but only the parent thread is
// resumable) reports the handle here. Empty string keeps the canonical id.
type ResumeHandle interface {
	ResumeID(s SessionSummary) string
}

// NativeOpener is the optional desktop-open capability: a runtime whose vendor
// ships a desktop app with a URL scheme reports the link that opens this
// session there. ok=false means this session has none (a subagent would open
// its parent; an id the app would refuse opens nothing). The adapter composes
// the URL from a fixed template and an id it has shape-checked, so scheme and
// path are never data (native-session-open-links plan §2.1).
type NativeOpener interface {
	NativeOpen(s SessionSummary) (NativeOpenLink, bool)
}

// NativeOpenLink is one session's desktop-app link. App is the desktop app's
// own name, for the control's label.
type NativeOpenLink struct {
	URL string `json:"url"`
	App string `json:"app"`
}

// ActivityEvidenceReporter is the optional session-activity evidence
// capability (natural-session plan, red-team M9/B3): a runtime reports which
// lifecycle signal kinds its natural sessions serve and through which
// evidence class — "hook-exact" (an installed lifecycle hook wrote the durable
// fact) or "presence-derived" (inferred from store presence). An absent kind
// is an absent fact, never a guess: a runtime with no end hook reports no
// "session.ended" entry at all.
type ActivityEvidenceReporter interface {
	ActivityEvidence() map[string]string
}

// ActivityEvidence returns one runtime's signal-kind → evidence-class facts,
// or nil when the adapter does not report the capability.
func ActivityEvidence(runtime string) map[string]string {
	if rt := runtimeFor(runtime); rt != nil {
		if reporter, ok := rt.(ActivityEvidenceReporter); ok {
			return reporter.ActivityEvidence()
		}
	}
	return nil
}

// CLIRevisioner is the optional interface-revision capability: a runtime
// whose adapter was verified against a specific installed provider CLI
// reports that measured revision here (natural-session plan, Slice A). The
// revision is compile-time data recorded with the adapter — it names what
// the interface was BUILT AGAINST, not what is installed right now;
// installed-version evidence belongs to the runtime-discovery owner. A
// runtime without the capability reports no revision, never a guess.
type CLIRevisioner interface {
	CLIRevision() string
}

// CLIRevision returns the interface revision one runtime's adapter was
// verified against, or "" when the adapter does not publish one.
func CLIRevision(runtime string) string {
	if rt := runtimeFor(runtime); rt != nil {
		if rev, ok := rt.(CLIRevisioner); ok {
			return rev.CLIRevision()
		}
	}
	return ""
}

// CLICapability is the revision-bearing row the capability matrix publishes
// per runtime: which interface the adapter was verified against, and which
// optional capabilities it implements.
type CLICapability struct {
	InterfaceRevision string          `json:"interface_revision"`
	Capabilities      map[string]bool `json:"capabilities"`
}

// CapabilityMatrix reports which optional capabilities every registered
// runtime implements — pure data for doctor-style surfaces. A runtime silently
// half-implemented shows up as an explicit false, never as an absent row. The
// interface_revision row names the provider CLI each adapter was measured
// against ("" when the adapter publishes none).
func CapabilityMatrix() map[string]CLICapability {
	matrix := make(map[string]CLICapability, len(runtimes))
	for name, rt := range runtimes {
		revision := ""
		if rev, ok := rt.(CLIRevisioner); ok {
			revision = rev.CLIRevision()
		}
		matrix[name] = CLICapability{InterfaceRevision: revision,
			Capabilities: map[string]bool{
				"session_source":                   capImplemented[SessionSource](rt),
				"deep_normalizer":                  capImplemented[DeepNormalizer](rt),
				"id_shape":                         capImplemented[IDShape](rt),
				"repository_grouper":               capImplemented[RepositoryGrouper](rt),
				"session_lineage":                  capImplemented[SessionLineage](rt),
				"edge_source":                      capImplemented[EdgeSource](rt),
				"turn_anchorer":                    capImplemented[TurnAnchorer](rt),
				"resume_handle":                    capImplemented[ResumeHandle](rt),
				"native_open":                      capImplemented[NativeOpener](rt),
				"activity_evidence":                capImplemented[ActivityEvidenceReporter](rt),
				"lifecycle_normalizer":             capImplemented[LifecycleNormalizer](rt),
				"incremental_lifecycle_normalizer": capImplemented[IncrementalLifecycleNormalizer](rt),
				"logical_lifecycle_source":         capImplemented[LogicalLifecycleSource](rt),
				"transcript_projection_source":     capImplemented[TranscriptProjectionSource](rt),
			}}
	}
	return matrix
}

func capImplemented[T any](rt Runtime) bool { _, ok := any(rt).(T); return ok }

func CouldMatchID(runtime, id string) bool {
	rt := runtimeFor(runtime)
	if shape, ok := rt.(IDShape); ok {
		return shape.CouldMatchID(id)
	}
	return true
}

func looksLikeUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// EventText returns the untruncated text of ONE event.
//
// Why this exists: a transcript clips tool payloads so a 1,300-event session
// stays openable (transcriptCaps — a couple of KB for a tool call), which for a
// large edit is the head of the file and not the body. The content a reviewer
// actually needs was never in the response. It is still on disk, so this
// re-reads the one event someone asked to see, at deepCaps.
//
// found=false with no error means the runtime cannot do deep reads; the caller
// must say so rather than presenting the clipped text as complete.
func EventText(runtime, id string, seq int) (text string, found bool, err error) {
	s, ok := Find(runtime, id)
	if !ok {
		return "", false, fmt.Errorf("no %s session %q", runtime, id)
	}
	_, logical := sessionSource(runtime)
	deep, deepOK := runtimeFor(runtime).(DeepNormalizer)
	if !logical && !deepOK {
		return "", false, nil
	}
	var events []CanonicalEvent
	if logical {
		events, _, _, err = NormalizeSummary(s, true)
	} else {
		events, _, _, err = deep.NormalizeFull(s.Path)
	}
	if err != nil {
		return "", false, err
	}
	for _, e := range events {
		if e.Seq == seq {
			return e.Text, true, nil
		}
	}
	return "", false, fmt.Errorf("session %q has no event %d", id, seq)
}
