package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/internal/teamlink"
	"crossing-guard/schemas"
	"crossing-guard/teamwire"
)

// Building the handoff document (team rest-of-release plan §6.1, §6.6). One function
// builds it for the preview and for the send, from the same inputs, so the bytes the
// sheet showed are the bytes that leave: the send must name the previewed wire hash and
// is refused when the document it builds hashes differently (criterion 62).
//
// The checks before it leaves run over every free-text field — the title, the body,
// each remaining item, each excerpt turn, each governance tag value: engine.PortableText
// (the checkout root becomes relative; home, temporary and system path shapes become
// "[absolute path]"), then engine.RedactText (the named secret patterns). The wire hash
// is taken after. It is a bounded, named check, never a claim that no secret remains;
// the preview shows the result so a shape the check does not cover is visible first.

// absolutePathMarker is what engine.PortableText leaves in place of an absolute path.
const absolutePathMarker = "[absolute path]"

// handoffLocalRecipient stands in the recipient field of a local, same-device handoff,
// which names no member and never reaches a server.
const handoffLocalRecipient = ""

// handoffChecks counts what the checks changed, by marker name. Counts only.
type handoffChecks struct {
	// Redactions is secret pattern name → how many matches became markers.
	Redactions map[string]int `json:"redactions"`
	// RedactionCount is their total.
	RedactionCount int `json:"redaction_count"`
	// AbsolutePaths is how many paths became "[absolute path]".
	AbsolutePaths int `json:"absolute_paths"`
}

// handoffChecker applies the two checks to one text at a time and counts.
type handoffChecker struct {
	root      string
	detectors []engine.Detector
	checks    handoffChecks
}

func (c *handoffChecker) text(in string) string {
	portable := engine.PortableText(in, c.root)
	if moved := strings.Count(portable, absolutePathMarker) - strings.Count(in, absolutePathMarker); moved > 0 {
		c.checks.AbsolutePaths += moved
	}
	out, fired := engine.RedactText(portable, c.detectors)
	for _, name := range fired {
		marker := "[redacted:" + name + "]"
		if n := strings.Count(out, marker) - strings.Count(portable, marker); n > 0 {
			c.checks.Redactions[name] += n
			c.checks.RedactionCount += n
		}
	}
	return out
}

// handoffCheckoutFacts is the sender session's checkout: the root paths are made
// relative to (never shipped), and the repository id — remote-derived only, else nil.
type handoffCheckoutFacts struct {
	root         string
	repositoryID *string
}

// remoteRepositoryIdentity is the identity kind of a repository id derived from the
// repository's remote: the only kind that means the same repository on another device.
const remoteRepositoryIdentity = "remote-sha256"

// handoffCheckout resolves the session's folder through the repository-identity owner
// (changeenv, behind resolveCheckout). A folder that is not a checkout, or does not
// resolve in time, yields the folder itself as the root and no repository id.
func handoffCheckout(d *SessionDetail) handoffCheckoutFacts {
	if d.Cwd == "" {
		return handoffCheckoutFacts{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), handoffResolveTimeout())
	defer cancel()
	repo, err := resolveCheckout(ctx, d.Cwd)
	if err != nil || ctx.Err() != nil || repo.Root == "" {
		return handoffCheckoutFacts{root: d.Cwd}
	}
	facts := handoffCheckoutFacts{root: repo.Root}
	if repo.IdentityKind == remoteRepositoryIdentity && repo.ID != "" {
		id := repo.ID
		facts.repositoryID = &id
	}
	return facts
}

// handoffResolveTimeout is team.json's identity_resolve_timeout — the link's bound on
// one repository resolution, whose default loads unlinked too.
func handoffResolveTimeout() time.Duration {
	if team != nil {
		team.mu.Lock()
		defer team.mu.Unlock()
		if team.doc.IdentityResolveTimeout.Duration > 0 {
			return team.doc.IdentityResolveTimeout.Duration
		}
	}
	def, err := teamlink.Default()
	if err != nil {
		return time.Second
	}
	return def.IdentityResolveTimeout.Duration
}

// handoffAgentsOf lists the shared agents a handoff names, as references: the adopted
// agents turned on for the repository the sender's session worked in. The reader is
// sharedAgentsForSession, installed by wireTeamRestOfRelease once the hosts exist; a
// daemon without them names none. It is a variable so that wiring is one assignment.
var handoffAgentsOf = func(*SessionDetail) []teamwire.HandoffAgent { return []teamwire.HandoffAgent{} }

// handoffSessionIdentity is the sender's session as the wire session object: the wire
// id (derived from the device and runtime/native id by the one owner), the runtime, and
// the three identities as three values — native, catalog and resume — never one joined
// on another.
func handoffSessionIdentity(deviceID string, s harvest.SessionSummary) teamwire.SessionIdentity {
	native := targetNativeIDOf(s)
	runtime, nativeID := engine.WireSessionParts(s.Runtime, native)
	return teamwire.SessionIdentity{ID: engine.WireSessionID(deviceID, runtime+"/"+nativeID), Runtime: runtime,
		NativeID: nativeID, CatalogID: targetCatalogIDOf(s), ResumeID: s.ResumeID}
}

// handoffGovernance reads the session's declared governance state: the folded tags and
// the water mark over them. Tag values pass the same checks as the text.
func handoffGovernance(s harvest.SessionSummary, c *handoffChecker) teamwire.HandoffGovernance {
	out := teamwire.HandoffGovernance{Tags: []teamwire.HandoffTag{}}
	if governor == nil {
		return out
	}
	rows, err := governor.SessionState(harvest.CanonicalID(s))
	if err != nil {
		return out
	}
	var tags []engine.Tag
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.Key+"\x00"+row.Value] {
			continue
		}
		seen[row.Key+"\x00"+row.Value] = true
		tags = append(tags, engine.Tag{Key: row.Key, Value: row.Value})
		out.Tags = append(out.Tags, teamwire.HandoffTag{Key: row.Key, Value: c.text(row.Value)})
	}
	out.WaterMark = engine.WaterMark(tags)
	return out
}

// handoffExcerpt builds the optional conversation excerpt: the session's last turns
// user and assistant text, at most turns of them and at most maxBytes of text in total,
// each turn checked. Older turns are dropped first to fit; a single turn larger than
// the bound is cut at a character boundary. truncated says the byte bound, or the
// harvest's own display bound on a turn, cut something.
func handoffExcerpt(d *SessionDetail, turns, maxBytes int, c *handoffChecker) *teamwire.HandoffConversation {
	var all []teamwire.HandoffTurn
	truncated := false
	for _, ev := range d.Events {
		if (ev.Kind != "user" && ev.Kind != "assistant") || strings.TrimSpace(ev.Text) == "" {
			continue
		}
		all = append(all, teamwire.HandoffTurn{Seq: max(ev.Seq, 1), Role: ev.Kind, Text: ev.Text})
	}
	if len(all) > turns {
		all = all[len(all)-turns:]
	}
	for i := range all {
		all[i].Text = c.text(all[i].Text)
	}
	total := 0
	for _, turn := range all {
		total += len(turn.Text)
	}
	for len(all) > 1 && total > maxBytes {
		total -= len(all[0].Text)
		all = all[1:]
		truncated = true
	}
	if len(all) == 1 && len(all[0].Text) > maxBytes {
		all[0].Text = cutAtRune(all[0].Text, maxBytes)
		truncated = true
	}
	for _, ev := range d.Events {
		if ev.FullLen > len(ev.Text) && len(all) > 0 && ev.Seq >= all[0].Seq && (ev.Kind == "user" || ev.Kind == "assistant") {
			truncated = true
		}
	}
	if all == nil {
		all = []teamwire.HandoffTurn{}
	}
	return &teamwire.HandoffConversation{Turns: all, Truncated: truncated}
}

// cutAtRune cuts s to at most limit bytes without splitting a character.
func cutAtRune(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// handoffDocumentInput is everything a handoff document is built from. The person's
// final text arrives as Title, Body and Remaining; the rest is read from the session.
type handoffDocumentInput struct {
	ID        string
	CreatedAt string
	DeviceID  string
	Session   *SessionDetail
	Checkout  handoffCheckoutFacts
	Recipient string // a member's user id; handoffLocalRecipient for a local handoff
	Title     string
	Body      string
	Remaining []string
	// IncludeConversation is the sender's tick; without it no excerpt is built.
	IncludeConversation bool
	Limits              ConsoleHandoffDefaults
}

// handoffInvalid is a document the person must change before it can leave. Its text
// names a field and a rule and never quotes what was submitted.
type handoffInvalid struct{ reason string }

func (e *handoffInvalid) Error() string { return e.reason }

// buildHandoffDocument builds the record and its frozen wire body. The checks run
// here; the wire hash is taken after them. A team document is validated against the
// handoff wire schema, so a document the server would refuse never reaches the outbox.
func buildHandoffDocument(in handoffDocumentInput, detectors []engine.Detector) (teamwire.HandoffRecord, []byte, handoffChecks, error) {
	c := &handoffChecker{root: in.Checkout.root, detectors: detectors, checks: handoffChecks{Redactions: map[string]int{}}}
	title, body := strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if title == "" {
		return teamwire.HandoffRecord{}, nil, c.checks, &handoffInvalid{"title is required"}
	}
	if body == "" {
		return teamwire.HandoffRecord{}, nil, c.checks, &handoffInvalid{"body_markdown is required"}
	}
	rec := teamwire.HandoffRecord{SchemaVersion: teamwire.HandoffSchemaVersion, ID: in.ID,
		Session:      handoffSessionIdentity(in.DeviceID, in.Session.SessionSummary),
		RepositoryID: in.Checkout.repositoryID, CreatedAt: in.CreatedAt,
		CreatedBy: teamwire.Actor{Type: "user", ID: "member"}, Recipient: teamwire.HandoffRecipient{UserID: in.Recipient},
		Title: c.text(title), BodyMarkdown: c.text(body) + "\n", Remaining: []string{},
		Agents: handoffAgentsOf(in.Session), AnchorsScope: teamwire.HandoffAnchorsScope}
	for _, item := range in.Remaining {
		if item = strings.TrimSpace(item); item != "" {
			rec.Remaining = append(rec.Remaining, c.text(item))
		}
	}
	rec.GovernanceState = handoffGovernance(in.Session.SessionSummary, c)
	if in.IncludeConversation {
		rec.Conversation = handoffExcerpt(in.Session, in.Limits.ConversationTurns, in.Limits.ConversationMaxBytes, c)
	}
	if rec.Agents == nil {
		rec.Agents = []teamwire.HandoffAgent{}
	}
	rec.ContentHash = teamwire.HandoffWireHash(rec)
	wire, err := json.Marshal(rec)
	if err != nil {
		return teamwire.HandoffRecord{}, nil, c.checks, err
	}
	if in.Recipient != handoffLocalRecipient {
		violations, err := schemas.Violations(teamwire.SchemaForKind(teamwire.KindHandoff), wire)
		if err != nil {
			return teamwire.HandoffRecord{}, nil, c.checks, err
		}
		if len(violations) > 0 {
			v := violations[0]
			return teamwire.HandoffRecord{}, nil, c.checks, &handoffInvalid{fmt.Sprintf("the handoff does not fit the wire schema at %s (%s)", v.Path, v.Keyword)}
		}
	}
	return rec, wire, c.checks, nil
}

// errRecipientNotListed refuses a send to someone the member directory does not list:
// the device answers it recipient_inactive without asking the server (criterion 79).
var errRecipientNotListed = errors.New("no such member in the directory")

// errRecipientAmbiguous refuses a display name more than one member carries.
var errRecipientAmbiguous = errors.New("more than one member has that display name; use the user id")

// resolveRecipient finds the member `to` names: a user id, or a display name compared
// without case. The directory is the cache of the linked organization.
func resolveRecipient(organizationID, to string) (teamHandoffMember, error) {
	members, err := governor.ix.TeamMembers(organizationID)
	if err != nil {
		return teamHandoffMember{}, err
	}
	var named []teamHandoffMember
	for _, m := range members {
		entry := teamHandoffMember{UserID: m.UserID, DisplayName: m.DisplayName, Self: m.Self}
		if m.UserID == to {
			return entry, nil
		}
		if strings.EqualFold(m.DisplayName, to) {
			named = append(named, entry)
		}
	}
	switch len(named) {
	case 0:
		return teamHandoffMember{}, errRecipientNotListed
	case 1:
		return named[0], nil
	}
	return teamHandoffMember{}, errRecipientAmbiguous
}
