package daemon

// The session tag index (session-organization plan §3.2): one read model over
// three owners that keep writing exactly as they do today —
//
//	owner tags        store.session_owner_tag   user-asserted
//	agent tags        store.orchestration_tag   model-claimed
//	detector facets   store.session_state       observed
//
// Nothing is copied or merged: a row is decorated per request from a snapshot
// that lives no longer than the session scan it accompanies. A stored row
// belongs to a rail row only when the runtime adapter's MatchID says so; the
// row's raw id alternates are used to LOOK UP candidates and never to decide,
// because a codex child rollout carries its parent's thread id.
//
// Owner tags organize; they never govern and never reach an agent. What holds
// that line is narrow and stated: session_organization_boundary_test.go refuses
// any mention of the owner-tag reads in the rule evaluator's input (decide.go,
// govern.go), the agent context builder (orchestration_context.go), the agent
// prompt, and the engine, orchestration and rulebook packages.

import (
	"sort"
	"strings"
	"sync"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/store"
)

// sessionTag is one tag on a rail row. Provenance is the engine's own word for
// who asserted it, or "model-claimed" as the orchestration owner stores it; the
// browser maps it to a colour and never prints it.
type sessionTag struct {
	Key        string `json:"key,omitempty"`
	Value      string `json:"value"`
	Provenance string `json:"provenance"`
	// By names the agent for a model-claimed tag. It is empty otherwise.
	By string `json:"by,omitempty"`
	// At is when the tag became true of the session, in Unix seconds.
	At int64 `json:"at,omitempty"`
}

const provenanceModelClaimed = store.OrchestrationTagProvenance

// railSession is a rail row with what the owner and his agents have said about
// it. SessionSummary is embedded so the JSON stays flat, and every added field
// is omitempty: a session nobody tagged serializes exactly as it always has.
type railSession struct {
	SessionSummary
	// Tags are the owner's and agents' tags; Facts are what detectors saw. They
	// are separate because a row shows the first and only the header the second.
	Tags  []sessionTag `json:"tags,omitempty"`
	Facts []sessionTag `json:"facts,omitempty"`
	Note  string       `json:"note,omitempty"`
	// InViewSince is when the row came to satisfy the active query.
	InViewSince int64 `json:"in_view_since,omitempty"`
	// PlacedBy, InColumnSince and ObservedGroup are set only on a row a board
	// read returns (board-observed-columns plan §2.2). PlacedBy says what put
	// the row in the group it is listed under: the owner's tag, or one of the
	// board's placement rules. InColumnSince is when: the tag's applied time,
	// or when the row came to satisfy the rule. ObservedGroup is on a row the
	// owner placed whose rules name another column: that column.
	PlacedBy      string `json:"placed_by,omitempty"`
	InColumnSince int64  `json:"in_column_since,omitempty"`
	ObservedGroup string `json:"observed_group,omitempty"`
	// TranscriptMissing marks a row the session scan did not return, served
	// from what the owner's tag remembered. It says what was observed — no
	// transcript was found — not why: the file may have been deleted, or its
	// store may have been unreadable during this scan.
	TranscriptMissing bool `json:"transcript_missing,omitempty"`
}

// listRow is a decorated session as a rail LIST row: tags capped for display,
// and no detector facts — a row never shows them, and carrying them would
// change the bytes of a plain repository page for sessions nobody tagged.
// Matching always runs on the uncapped decoration, never on this.
func (r railSession) listRow(maxTags int) railSession {
	r.Facts = nil
	if maxTags > 0 && len(r.Tags) > maxTags {
		r.Tags = r.Tags[:maxTags]
	}
	return r
}

// sessionTagSnapshot is one read of the three sources, indexed by session id.
type sessionTagSnapshot struct {
	owner     map[string][]store.SessionOwnerTag
	agent     map[string][]store.OrchestrationTag
	facets    map[string][]store.SessionStateFacet
	notes     map[string][]store.SessionOwnerNote
	parents   map[string]store.AgentSessionParent
	truncated bool // a read reached its bound; totals derived from this are not the whole truth
	available bool
}

// sessionTagsRead loads the snapshot. A test may stub it; an absent governor or
// a failed read yields an unavailable snapshot, and rows then render without
// tags rather than the request failing.
var sessionTagsRead = func(now time.Time) sessionTagSnapshot {
	if governor == nil || governor.ix == nil {
		return sessionTagSnapshot{}
	}
	config, _ := consoleConfig()
	return readSessionTagSnapshot(governor.ix, config.SessionOrganization.TagIndexRowsMax, now)
}

func readSessionTagSnapshot(ix *store.Index, limit int, now time.Time) sessionTagSnapshot {
	snapshot := sessionTagSnapshot{owner: map[string][]store.SessionOwnerTag{}, agent: map[string][]store.OrchestrationTag{},
		facets: map[string][]store.SessionStateFacet{}, notes: map[string][]store.SessionOwnerNote{}}
	owner, cutOwner, err := ix.AllActiveSessionOwnerTags(limit)
	if err != nil {
		return sessionTagSnapshot{}
	}
	agent, cutAgent, err := ix.AllActiveOrchestrationTags(now.Unix(), limit)
	if err != nil {
		return sessionTagSnapshot{}
	}
	facets, cutFacets, err := ix.AllSessionStateFacets(limit)
	if err != nil {
		return sessionTagSnapshot{}
	}
	notes, cutNotes, err := ix.AllSessionOwnerNotes(limit)
	if err != nil {
		return sessionTagSnapshot{}
	}
	parents, cutParents, err := ix.AllAgentSessionParents(limit)
	if err != nil {
		return sessionTagSnapshot{}
	}
	for _, tag := range owner {
		snapshot.owner[tag.SessionID] = append(snapshot.owner[tag.SessionID], tag)
	}
	for _, tag := range agent {
		snapshot.agent[tag.SessionID] = append(snapshot.agent[tag.SessionID], tag)
	}
	for _, facet := range facets {
		snapshot.facets[facet.SessionID] = append(snapshot.facets[facet.SessionID], facet)
	}
	for _, note := range notes {
		snapshot.notes[note.SessionID] = append(snapshot.notes[note.SessionID], note)
	}
	snapshot.parents = parents
	snapshot.truncated = cutOwner || cutAgent || cutFacets || cutNotes || cutParents
	snapshot.available = true
	return snapshot
}

// sessionTagCoalescer shares one snapshot among the requests of one rail load
// (the rail asks for its groups and then for each open group's page). It holds
// a snapshot no longer than the session scan is held, and a write drops it, so
// the owner never sees his own tag missing.
type sessionTagCoalescer struct {
	mu       sync.Mutex
	snapshot sessionTagSnapshot
	taken    time.Time
}

var sessionTagSnapshots = &sessionTagCoalescer{}

func (c *sessionTagCoalescer) get(now time.Time) sessionTagSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshot.available && now.Sub(c.taken) < scanReuseWindow && !now.Before(c.taken) {
		return c.snapshot
	}
	c.snapshot, c.taken = sessionTagsRead(now), now
	return c.snapshot
}

func (c *sessionTagCoalescer) drop() {
	c.mu.Lock()
	c.snapshot = sessionTagSnapshot{}
	c.mu.Unlock()
}

// candidateIDs are the ids under which something about this row may be stored.
// They find candidates; belongsTo decides.
func candidateIDs(row SessionSummary) []string {
	ids := sessionIdentityAlternates(row)
	canonical := harvest.CanonicalID(row)
	for _, id := range ids {
		if id == canonical {
			return ids
		}
	}
	if canonical != "" {
		ids = append(ids, canonical)
	}
	return ids
}

// belongsTo is the one identity rule. runtime is empty for a detector facet,
// whose table has no runtime column.
func belongsTo(row SessionSummary, runtime, sessionID string) bool {
	if runtime != "" && runtime != row.Runtime {
		return false
	}
	return harvest.MatchID(row, sessionID)
}

// countable reports a snapshot complete enough to count or filter on. An
// unavailable snapshot is not "no tags"; a truncated one is not "all tags".
func (s sessionTagSnapshot) countable() bool { return s.available && !s.truncated }

// decorate attaches every tag, fact and the note to one row, uncapped.
func (s sessionTagSnapshot) decorate(row SessionSummary) railSession {
	out := railSession{SessionSummary: row}
	if !s.available {
		return out
	}
	seenAgent := map[string]bool{}
	for _, id := range candidateIDs(row) {
		for _, tag := range s.owner[id] {
			if belongsTo(row, tag.Runtime, tag.SessionID) {
				out.Tags = append(out.Tags, ownerSessionTag(tag))
			}
		}
		for _, tag := range s.agent[id] {
			// One agent may have claimed the same tag on several runs and under
			// several identities of one session; the owner sees it once.
			mark := tag.AgentKey + "\x00" + strings.ToLower(tag.Tag)
			if seenAgent[mark] || !belongsTo(row, tag.Runtime, tag.SessionID) {
				continue
			}
			seenAgent[mark] = true
			out.Tags = append(out.Tags, agentSessionTag(tag))
		}
		for _, facet := range s.facets[id] {
			if facet.Value != "" && belongsTo(row, "", facet.SessionID) {
				out.Facts = append(out.Facts, facetSessionTag(facet))
			}
		}
		for _, note := range s.notes[id] {
			if out.Note == "" && belongsTo(row, note.Runtime, note.SessionID) {
				out.Note = note.Text
			}
		}
	}
	out.Tags, out.Facts = dedupeSessionTags(out.Tags), dedupeSessionTags(out.Facts)
	return out
}

// ownerSessionTag, agentSessionTag and facetSessionTag are the one conversion
// of each stored source into a rail tag, for decorate and snapshotVocabulary.
func ownerSessionTag(tag store.SessionOwnerTag) sessionTag {
	return sessionTag{Key: tag.Key, Value: tag.Value, Provenance: string(engine.UserAsserted), At: tag.AppliedAt}
}

func agentSessionTag(tag store.OrchestrationTag) sessionTag {
	return sessionTag{Value: tag.Tag, Provenance: provenanceModelClaimed, By: tag.AgentKey, At: tag.AppliedAt}
}

func facetSessionTag(facet store.SessionStateFacet) sessionTag {
	return sessionTag{Key: facet.Key, Value: facet.Value, Provenance: facet.Provenance, At: facet.FirstSeen}
}

// dedupeSessionTags drops repeats found under more than one candidate id and
// orders what remains: the owner's first, then by age, newest first.
func dedupeSessionTags(tags []sessionTag) []sessionTag {
	seen := map[string]bool{}
	out := tags[:0]
	for _, tag := range tags {
		mark := tag.Provenance + "\x00" + tag.By + "\x00" + strings.ToLower(tag.Key) + "\x00" + strings.ToLower(tag.Value)
		if seen[mark] {
			continue
		}
		seen[mark] = true
		out = append(out, tag)
	}
	sort.SliceStable(out, func(i, j int) bool {
		mine := func(tag sessionTag) bool { return tag.Provenance == string(engine.UserAsserted) }
		if mine(out[i]) != mine(out[j]) {
			return mine(out[i])
		}
		return out[i].At > out[j].At
	})
	return out
}

// vanished returns a row for each owner-tagged session the scan no longer
// finds, built from what the tag remembered. Only the owner's tag does this: it
// is how a session he marked stays listed after the vendor deletes its file.
func (s sessionTagSnapshot) vanished(rows []SessionSummary) []SessionSummary {
	if !s.available || len(s.owner) == 0 {
		return nil
	}
	byID := map[string][]int{}
	for index, row := range rows {
		for _, id := range candidateIDs(row) {
			byID[id] = append(byID[id], index)
		}
	}
	// s.owner is keyed by session id alone; two runtimes could share an id, so
	// remembered sessions are separated by runtime here.
	newest := map[string]store.SessionOwnerTag{}
	for sessionID, tags := range s.owner {
		for _, tag := range tags {
			key := presenceCatalogKey(tag.Runtime, sessionID)
			if held, seen := newest[key]; !seen || tag.TouchedAt > held.TouchedAt {
				newest[key] = tag
			}
		}
	}
	var out []SessionSummary
	for _, tag := range newest {
		present := false
		for _, index := range byID[tag.SessionID] {
			if belongsTo(rows[index], tag.Runtime, tag.SessionID) {
				present = true
				break
			}
		}
		if !present {
			out = append(out, rememberedSummary(tag))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func rememberedSummary(tag store.SessionOwnerTag) SessionSummary {
	return SessionSummary{Runtime: tag.Runtime, ID: tag.SessionID, Title: tag.Title, Cwd: tag.Cwd,
		RepositoryKey: tag.Repository, Modified: time.Unix(tag.TouchedAt, 0).UTC()}
}

// ownerRememberedSession is what an owner tag remembers of a session: enough
// to show it when no transcript is found. It is a keyed read — the same row
// vanished() picks — so the rail and the opened session always agree.
func ownerRememberedSession(ix *store.Index, runtime, sessionID string) (SessionSummary, bool) {
	tag, found, err := ix.RememberedSessionOwnerTag(runtime, sessionID)
	if err != nil || !found {
		return SessionSummary{}, false
	}
	return rememberedSummary(tag), true
}

// keptTranscriptEvents renders the text kept for search as transcript rows.
// It is lossy by construction — replies clipped to the search bound, tool rows
// one line each, no reasoning — and carries no vendor sequence numbers, so rows
// are numbered by their kept order. The session title document is not a row.
func keptTranscriptEvents(ix *store.Index, runtime, sessionID string) []harvest.CanonicalEvent {
	config, _ := consoleConfig()
	documents, err := ix.TranscriptDocuments(runtime, sessionID, config.SessionOrganization.KeptTextDocumentsMax)
	if err != nil {
		return nil
	}
	events := make([]harvest.CanonicalEvent, 0, len(documents))
	for _, document := range documents {
		if !viewRowKinds[document.Kind] {
			continue
		}
		events = append(events, harvest.CanonicalEvent{Seq: len(events), Kind: document.Kind, Ts: document.TS, Text: document.Text})
	}
	return events
}
