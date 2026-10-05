package main

import (
	"fmt"

	"crossing-guard/internal/daemon"
	"crossing-guard/store"
)

// doctorErrorSuffix renders an optional error text after a doctor line.
func doctorErrorSuffix(text string) string {
	if text == "" {
		return ""
	}
	return " — " + text
}

// teamMemoryHealth is the linked device's shared-memory state as the daemon reports it
// (GET /api/team's memory block; team item 5). Nil when the daemon is not answering or
// the device is not linked.
type teamMemoryHealth struct {
	Organization string                  `json:"organization"`
	Counts       store.MemorySyncSummary `json:"counts"`
	PullOutcome  string                  `json:"pull_outcome,omitempty"`
	PullError    string                  `json:"pull_error,omitempty"`
	PullLastAt   string                  `json:"pull_last_at,omitempty"`
	Unlandable   int64                   `json:"unlandable,omitempty"`
}

func teamMemoryProbe() *teamMemoryHealth {
	loc, err := daemon.LocateConsole()
	if err != nil {
		return nil
	}
	var status struct {
		State        string `json:"state"`
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
		Memory *struct {
			store.MemorySyncSummary
			Pull struct {
				LastAt     string `json:"last_at"`
				Outcome    string `json:"outcome"`
				Error      string `json:"error"`
				Unlandable int64  `json:"unlandable"`
			} `json:"pull"`
		} `json:"memory"`
	}
	if err := loc.GetJSON("/api/team", &status); err != nil || status.Memory == nil {
		return nil
	}
	return &teamMemoryHealth{Organization: status.Organization.Name, Counts: status.Memory.MemorySyncSummary,
		PullOutcome: status.Memory.Pull.Outcome, PullError: status.Memory.Pull.Error, PullLastAt: status.Memory.Pull.LastAt,
		Unlandable: status.Memory.Pull.Unlandable}
}

// teamMemoryLines words the shared-memory state for doctor: what is shared and received,
// then each count that is not zero — records that stay on this device, name collisions,
// conflict copies, edits a teammate's deletion displaced, and rows refused on this device.
func teamMemoryLines(h *teamMemoryHealth) []string {
	c := h.Counts
	lines := []string{fmt.Sprintf("team %s: %d shared, %d from teammates", h.Organization, c.Shared, c.Pulled)}
	if h.PullOutcome != "" {
		lines[0] += fmt.Sprintf(" · last pull %s (%s)%s", h.PullLastAt, h.PullOutcome, doctorErrorSuffix(h.PullError))
	}
	for _, item := range []struct {
		n    int
		text string
	}{
		{c.WeakRepository, "repository records stay on this device (identified only by a folder name)"},
		{c.ShareCandidates + c.ImportCandidates, "records could be shared and are not (Settings → Team)"},
		{c.NotShareable, "queued records not sent (not_shareable)"},
		{c.Shadowed, "team records not recalled: shadowed by a record of yours with the same name"},
		{c.Aliased, "team records under a local name: name collision"},
		{c.Conflicts, "conflict copies kept (a teammate's revision replaced an edit made here)"},
		{c.DiscardedEdits, "edits discarded by a teammate's deletion (kept as conflict copies)"},
		{c.Held, "teammate revisions held until an edit sent from here is answered"},
		{c.DeletedByTeam, "records deleted by teammates"},
		{c.DeletionsRefused, "deletions the team did not take — refused or never delivered (what the team holds returns)"},
		{int(h.Unlandable), "team records this version could not read and skipped (a newer version pulls again)"},
	} {
		if item.n > 0 {
			lines = append(lines, fmt.Sprintf("%s: %d", item.text, item.n))
		}
	}
	return lines
}
