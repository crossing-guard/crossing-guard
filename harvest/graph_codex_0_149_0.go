package harvest

// Codex observed lineage and edges. The child-side session_meta.source.subagent
// is the AUTHORITATIVE lineage source: it covers both thread_spawn children
// (parent_thread_id, depth, agent_path, agent_nickname, agent_role) and
// guardian auto-review children (source.subagent.other == "guardian"), which
// have NO parent-side record at all. Parent-side SubAgentActivity merely
// corroborates thread_spawn children.

import (
	"encoding/json"
	"os"
	"strings"
)

// applyCodexSessionMetaLineage reads the observed lineage facts out of one
// session_meta body into the summary-pass yield. FIRST session_meta wins: a
// thread_spawn child replays the parent's transcript, so a later session_meta
// line is the PARENT's replayed header — trusting it re-folded the child onto
// the parent (measured on rollout 01a045e6-cd51, 2026-08-29).
func applyCodexSessionMetaLineage(meta *codexFileMeta, body map[string]any) {
	if meta.metaSeen {
		return
	}
	meta.metaSeen = true
	if id := anyString(body["id"]); id != "" {
		meta.metaID = id
	}
	source, _ := body["source"].(map[string]any)
	sub, _ := source["subagent"].(map[string]any)
	if sub == nil {
		return
	}
	if spawn, ok := sub["thread_spawn"].(map[string]any); ok && spawn != nil {
		meta.lineageKind = "native-thread-spawn"
		meta.parentThreadID = anyString(spawn["parent_thread_id"])
		meta.lineageDepth = int(asInt64(spawn["depth"]))
		meta.agentRole = anyString(spawn["agent_role"])
		meta.agentNickname = anyString(spawn["agent_nickname"])
	} else if other := anyString(sub["other"]); other != "" {
		// guardian auto-review and any future vendor-named subagent flavor.
		// The vendor-published word itself is the role label; nothing inferred.
		meta.lineageKind = "native-subagent"
		meta.agentRole = other
	} else {
		meta.lineageKind = "native-subagent"
	}
	if meta.parentThreadID == "" {
		// guardian children state the parent only at the session_meta top level
		// (parent_thread_id, or session_id — the same value in the corpus).
		if parent := anyString(body["parent_thread_id"]); parent != "" {
			meta.parentThreadID = parent
		} else {
			meta.parentThreadID = anyString(body["session_id"])
		}
	}
}

func applyCodexLineageSummary(s *SessionSummary, m codexFileMeta) {
	s.MetaID = m.metaID
	if m.lineageKind == "" {
		return
	}
	s.ParentID = m.parentThreadID
	s.LineageKind = m.lineageKind
	s.LineageDepth = m.lineageDepth
	s.LineageRole = m.agentRole
	s.LineageNickname = m.agentNickname
}

// codexTurnAnchorFromRecord composes the opaque turn anchor for one decoded
// record: the turn_id Codex stamps on flat event_msg envelopes. The ordinal is
// deliberately excluded — an anchor names a TURN, and including per-item
// ordinals would make two events of the same turn compare unequal.
func codexTurnAnchorFromRecord(record codexRecord) string {
	if record.Envelope != "event_msg" {
		return ""
	}
	return anyString(record.Body["turn_id"])
}

// Lineage reports the child side only: codex records lineage in the child's
// own session_meta (parsed once by Summarize), so parent facts are free here.
// Enumerating a parent's children would reparse every rollout per call —
// callers invert ParentID across the scanned summaries instead.
func (codexRuntime) Lineage(s SessionSummary) (LineageFacts, bool) {
	if s.LineageKind == "" {
		return LineageFacts{}, false
	}
	return LineageFacts{
		Parent:     EdgeEndpoint{Runtime: s.Runtime, ID: s.ParentID},
		Kind:       s.LineageKind,
		Provenance: EdgeProvenanceObserved,
		Depth:      s.LineageDepth,
		Role:       s.LineageRole,
		Nickname:   s.LineageNickname,
	}, true
}

// ResumeID: the vendor-resumable handle is always the THREAD id — for a
// subagent rollout that is the parent's thread (children are not independently
// resumable), for a primary its own. Canonical identity diverges for children
// (CanonicalID above); the resume affordance must not.
func (codexRuntime) ResumeID(s SessionSummary) string { return s.ThreadID }

// NativeOpen: the desktop app's thread route takes a lowercase thread uuid
// (measured 2026-10-03, Codex Desktop 26.928.31416). A subagent's thread is
// its parent's, so a link there would open the parent and say otherwise.
func (rt codexRuntime) NativeOpen(s SessionSummary) (NativeOpenLink, bool) {
	if s.LineageKind != "" {
		return NativeOpenLink{}, false
	}
	thread := rt.CanonicalID(s)
	if !looksLikeUUID(thread) || thread != strings.ToLower(thread) {
		return NativeOpenLink{}, false
	}
	return NativeOpenLink{URL: "codex://threads/" + thread, App: "Codex"}, true
}

// TurnAnchor: the normalizer already composed the anchor from the flat
// envelope's turn_id; absent one, this vendor cannot anchor that event.
func (codexRuntime) TurnAnchor(_ SessionSummary, e CanonicalEvent) (string, bool) {
	return e.TurnAnchor, e.TurnAnchor != ""
}

// Edges derives the observed cross-session edges from one rollout file.
// Lazy and per-session: a full-transcript parse is acceptable here and only
// here — never during the summary scan.
//
// DECODE HAZARD: the item-type words also appear as free text inside
// response_item envelopes and CommandExecution output (12 real vs 17 false
// positives by substring in one measured file). Decoding is therefore strictly
// structural: event_msg → item_completed → item.type, via decodeCodexRecord.
// Pinned by TestCodexEdgesIgnoreEnvelopeFalsePositives.
//
// No `messaged` edge is emitted for codex: CollabAgentToolCall carries
// recipient fields in its schema, but no corpus instance has ever populated
// them — shipping the mapping on schema alone would assert edges nothing
// observed (graph design review §3.2).
func (runtime codexRuntime) Edges(s SessionSummary) ([]Edge, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	self := EdgeEndpoint{Runtime: s.Runtime, ID: runtime.CanonicalID(s)}
	collector := newEdgeCollector()
	sc := newLineScanner(f)
	for sc.Scan() {
		var obj map[string]any
		if json.Unmarshal(sc.Bytes(), &obj) != nil {
			continue
		}
		record, ok := decodeCodexRecord(obj)
		if !ok || record.Envelope != "event_msg" || record.Kind != "item_completed" {
			continue
		}
		item, _ := record.Body["item"].(map[string]any)
		if item == nil {
			continue
		}
		obs := EdgeObservation{
			Anchor: codexTurnAnchorFromRecord(record),
			Ts:     anyString(obj["timestamp"]),
		}
		switch anyString(item["type"]) {
		case "SubAgentActivity":
			target := anyString(item["agent_thread_id"])
			if target == "" {
				continue
			}
			// A child's activity records reference its ROOT agent (the parent
			// thread). Re-emitting those as spawned would invert the edge the
			// child's own session_meta already reports; skip self/parent refs.
			if target == s.ParentID || target == s.ThreadID || target == s.MetaID {
				continue
			}
			// kind here is a LIFECYCLE word (started/interacted/repeating):
			// one spawned edge, one observation per sighting.
			obs.Note = anyString(item["kind"])
			collector.observe(self, EdgeEndpoint{Runtime: s.Runtime, ID: target}, EdgeKindSpawned, obs)
		case "CollabAgentToolCall":
			if anyString(item["tool"]) != "wait" {
				continue // no other collab tool has observed positive evidence
			}
			if states, ok := item["agents_states"].(map[string]any); ok && len(states) > 0 {
				obs.Note = compactJSON(states)
			}
			receivers, _ := item["receiver_thread_ids"].([]any)
			if len(receivers) == 0 {
				// The real corpus never populates receivers: the record says
				// the session waited, not for whom. Empty target, honestly.
				collector.observe(self, EdgeEndpoint{Runtime: s.Runtime}, EdgeKindWaitedOn, obs)
				continue
			}
			for _, receiver := range receivers {
				if id := anyString(receiver); id != "" {
					collector.observe(self, EdgeEndpoint{Runtime: s.Runtime, ID: id}, EdgeKindWaitedOn, obs)
				}
			}
		case "DynamicToolCall":
			if anyString(item["namespace"]) != "codex_app" || anyString(item["tool"]) != "list_threads" {
				continue
			}
			// The session read other threads' titles/summaries without naming
			// them individually: an unresolved counterparty, not none.
			collector.observe(self, EdgeEndpoint{Runtime: s.Runtime, Raw: "list_threads", Unresolved: true},
				EdgeKindReadContextOf, obs)
		}
	}
	return collector.edges, sc.Err()
}
