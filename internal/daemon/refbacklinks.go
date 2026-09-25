package daemon

// Backlinks — "what else points here" (console-and-info-panel §7 L2).
//
// Two independent sources, deliberately kept distinguishable in the answer:
// DOC references come from the reference index (which doc line names this
// target), and SESSION touches come from the governor's own event log via
// EntityReport — the cross-session backlink the governance model already
// answers for free (§10).
//
// Read-only. A source that cannot answer says so in Notes rather than returning
// silence: an empty list and an unavailable source look identical to a reader,
// and only one of them is the truth (INV — count, don't hide).

import (
	"path/filepath"
	"strings"
)

// backlinkDocCap bounds how many doc references are returned for one target. Our
// own tracker docs mention hot paths dozens of times; the panel wants the shape
// of the answer, not every line.
const backlinkDocCap = 50

// BacklinkRef is one incoming reference from a doc.
type BacklinkRef struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// BacklinkReport is the merged answer for one target.
type BacklinkReport struct {
	Target string `json:"target"`
	// Docs are lines in the .md corpus that name this target.
	Docs []BacklinkRef `json:"docs"`
	// DocsTruncated reports that Docs hit backlinkDocCap.
	DocsTruncated bool `json:"docs_truncated"`
	// Sessions are prior sessions that touched this entity, newest first.
	Sessions []EntityTouch `json:"sessions"`
	// Notes explain any source that could not answer. Never omitted silently.
	Notes []string `json:"notes,omitempty"`
}

// RefBacklinks merges doc references and cross-session touches for one target.
func RefBacklinks(root, target string) (*BacklinkReport, error) {
	target = strings.TrimSpace(target)
	rep := &BacklinkReport{Target: target, Docs: []BacklinkRef{}, Sessions: []EntityTouch{}}
	if target == "" {
		rep.Notes = append(rep.Notes, "no target given")
		return rep, nil
	}
	idx, err := loadRefIndex(root)
	if err != nil {
		return nil, err
	}
	docRefs, total := idx.Backlinks(target, backlinkDocCap)
	for _, ref := range docRefs {
		rep.Docs = append(rep.Docs, BacklinkRef(ref))
	}
	rep.DocsTruncated = total > len(rep.Docs)
	rep.Sessions, rep.Notes = sessionBacklinks(idx.Root(), target, rep.Notes)
	return rep, nil
}

// entityCandidates lists the ids the governor might have stored for one target,
// best first.
//
// The reference index speaks repo-RELATIVE paths; the governor's event log
// records what the tool reported, which is ABSOLUTE. Asking it for
// "store/govern.go" therefore never matched anything, so every file in every
// project reported "no session has touched this" — including files the open
// session had just created. The two halves of the answer were keyed in
// different vocabularies and nothing said so.
//
// The /private prefix is not paranoia: on macOS /tmp is a symlink to
// /private/tmp, and sessions record cwd both ways, so a project under either
// spelling would miss for the same reason at one remove.
func entityCandidates(root, target string) []string {
	if strings.HasPrefix(target, "file:") {
		return []string{target}
	}
	if !strings.HasPrefix(target, "/") {
		abs := filepath.Join(root, filepath.FromSlash(target))
		out := []string{abs}
		if alt, ok := strings.CutPrefix(abs, "/private/"); ok {
			out = append(out, "/"+alt)
		} else {
			out = append(out, filepath.Join("/private", abs))
		}
		// The bare target last: ids that are not paths at all (a D-item, an mcp
		// tool name) still resolve, and a store written by an older build may
		// hold the relative form.
		return append(out, target)
	}
	return []string{target}
}

// sessionBacklinks asks the governor which sessions touched this entity. The
// governor being absent is a normal state (the console runs without one), so it
// is reported as a note, never as an error that hides the doc half of the answer.
//
// An empty answer is reported with the governor's OWN reason rather than as a
// bare zero: "no governed session touched this" and "we could not look" are
// different claims, and rendering the second as the first is what made this bug
// invisible for a day.
func sessionBacklinks(root, target string, notes []string) ([]EntityTouch, []string) {
	if governor == nil {
		return []EntityTouch{}, append(notes, "session touches unavailable: no governor configured")
	}
	var lastNote string
	for _, cand := range entityCandidates(root, target) {
		rep, err := governor.EntityReport(canonicalEntityID(cand), entityEventCap)
		if err != nil {
			return []EntityTouch{}, append(notes, "session touches unavailable: "+err.Error())
		}
		if rep.Truncated {
			notes = append(notes, "session touches are partial: hit the event cap")
		}
		if len(rep.Touches) > 0 {
			return rep.Touches, notes
		}
		if rep.Note != "" {
			lastNote = rep.Note
		}
	}
	if lastNote != "" {
		notes = append(notes, lastNote)
	}
	return []EntityTouch{}, notes
}
