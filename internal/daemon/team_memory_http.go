package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
)

// Team memory on the local API (team item 5): the memory block of GET /api/team, the
// explicit share action, deletion reach, conflict copies, and "delete what was sent" for
// session content. Typed responses only (ADR 0022); the console never talks to the team
// server itself.

// teamMemoryResponse is GET /api/team's memory block: counts from the store and the pull
// job's persisted state. Facts only; the console words them.
type teamMemoryResponse struct {
	store.MemorySyncSummary
	Pull teamMemoryPull `json:"pull"`
}

type teamMemoryPull struct {
	LastAt   string `json:"last_at,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Error    string `json:"error,omitempty"`
	Cursor   int64  `json:"cursor"`
	Interval string `json:"interval"`
	// Unlandable counts team records this build could not read and skipped; a later
	// build pulls again from the start.
	Unlandable int64 `json:"unlandable"`
}

// memorySummary reads the store's memory counts for the linked organization, with the
// not_shareable window from team.json. It takes t.mu only to read the document: the
// store is never queried under the link's lock.
func (t *teamLinker) memorySummary() (store.MemorySyncSummary, error) {
	t.mu.Lock()
	orgID, since := t.doc.Organization.ID, t.now().Add(-t.doc.NotShareableWindow.Duration).Unix()
	t.mu.Unlock()
	return governor.ix.MemorySyncSummaryCounts(orgID, since)
}

// memoryStatusLocked builds the block from counts the caller read BEFORE taking t.mu (the
// store is never queried under the link's lock).
func (t *teamLinker) memoryStatusLocked(sum store.MemorySyncSummary, err error) *teamMemoryResponse {
	if err != nil {
		return nil
	}
	p := t.report.Pull
	out := &teamMemoryResponse{MemorySyncSummary: sum, Pull: teamMemoryPull{Outcome: p.Outcome, Error: p.Error, Cursor: p.Cursor,
		Interval: t.doc.PullInterval.String(), Unlandable: p.Unlandable}}
	if !p.LastAt.IsZero() {
		out.Pull.LastAt = p.LastAt.UTC().Format(time.RFC3339)
	}
	return out
}

type teamMemoryShareRequest struct {
	// IDs names records to share one by one (the individual review, which may include an
	// imported record). Absent shares every bulk candidate — never an imported record.
	IDs []string `json:"ids"`
}

type teamMemoryShareResponse struct {
	Shared int                `json:"shared"`
	Memory teamMemoryResponse `json:"memory"`
}

// handleTeamMemoryShare is the explicit, counted share action (decision 14): records that
// existed before the link, and every identity upgrade, leave the device only by this.
// 409 when not linked; 400 for a record that cannot travel; 404 for an unknown id.
func handleTeamMemoryShare(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req teamMemoryShareRequest
	config, _ := consoleConfig()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, config.WriteBytesMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {} to share every reviewed candidate, or {\"ids\": [\"<record id>\", …]}"})
		return
	}
	team.mu.Lock()
	linked, org, orgID := team.state == teamLinked, team.doc.Organization.Name, team.doc.Organization.ID
	team.mu.Unlock()
	if !linked {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, teamErrorResponse{Error: "this device is not linked to a team; there is no one to share with"})
		return
	}
	n, err := governor.ix.ShareMemory(req.IDs, orgID)
	switch {
	case errors.Is(err, store.ErrMemoryNotFound):
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, teamErrorResponse{Error: err.Error()})
		return
	case errors.Is(err, store.ErrMemoryNotShareable):
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: err.Error()})
		return
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: "the records could not be shared: " + err.Error()})
		return
	}
	if n > 0 {
		team.linkEvent("team.memory.share", countNoun(n, "memory record")+" shared with "+org)
	}
	sum, sumErr := team.memorySummary()
	team.mu.Lock()
	mem := team.memoryStatusLocked(sum, sumErr)
	team.mu.Unlock()
	out := teamMemoryShareResponse{Shared: n}
	if mem != nil {
		out.Memory = *mem
	}
	writeJSON(w, out)
}

// countNoun words a count: "1 memory record", "3 memory records".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// teamMemoryDeletion is one deletion this device sent and how far it has reached.
type teamMemoryDeletion struct {
	GlobalID     string `json:"global_id"`
	Slug         string `json:"slug"`
	DeletedAt    string `json:"deleted_at"`
	Acknowledged int    `json:"acknowledged"`
	Outstanding  int    `json:"outstanding"`
	Stale        int    `json:"stale"`
	Revoked      int    `json:"revoked"`
	Error        string `json:"error,omitempty"`
}

type teamMemoryDeletionsResponse struct {
	Deletions []teamMemoryDeletion `json:"deletions"`
	// Retention is the O-5 wording: what "deleted" does and does not cover.
	Retention string `json:"retention"`
}

// teamDeletionRetention is the O-5 retention wording (plan §5.10).
const teamDeletionRetention = "Deleted records are erased from the team server's live store when the deletion arrives, and from each active device when it next syncs. Devices that have been offline past the acknowledgement horizon are listed as stale; revoked devices may retain a copy. Server point-in-time backups and local index backups on each device may retain a copy until their own retention ends; that retention is reported separately and is not a live record."

// handleTeamMemoryDeletions asks the server how far this device's recent memory deletions
// have reached (decision 16). It makes signed requests, so it is its own route, never
// part of GET /api/team.
func handleTeamMemoryDeletions(w http.ResponseWriter, _ *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	out := teamMemoryDeletionsResponse{Deletions: []teamMemoryDeletion{}, Retention: teamDeletionRetention}
	team.mu.Lock()
	linked := team.state == teamLinked && team.key != nil
	var client *teamlink.Client
	var err error
	limit := team.doc.DeletionsVerifyMax
	if linked {
		client, err = teamlink.NewClient(team.doc.Server, team.deviceID, team.key, team.doc.RequestTimeout.Duration, team.now)
	}
	team.mu.Unlock()
	if !linked || err != nil {
		writeJSON(w, out)
		return
	}
	sent, err := governor.ix.MemoryDeletionsSent(limit)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: "the deletions could not be read: " + err.Error()})
		return
	}
	for _, d := range sent {
		row := teamMemoryDeletion{GlobalID: d.GlobalID, Slug: d.Slug, DeletedAt: time.Unix(0, d.DeletedAt).UTC().Format(time.RFC3339)}
		if v, err := client.VerifyDeletion(d.GlobalID); err != nil {
			row.Error = err.Error()
		} else {
			row.Acknowledged, row.Outstanding, row.Stale, row.Revoked = len(v.Acknowledged), len(v.Outstanding), len(v.Stale), len(v.Revoked)
		}
		out.Deletions = append(out.Deletions, row)
	}
	writeJSON(w, out)
}

type memoryConflictsResponse struct {
	Conflicts []store.MemoryConflict `json:"conflicts"`
}

// handleMemoryConflicts lists conflict copies: a local version the team's revision or
// deletion displaced, kept and never merged. id narrows to one record's global id.
func handleMemoryConflicts(w http.ResponseWriter, r *http.Request) {
	ix, err := openIndexForRead()
	if errors.Is(err, errIndexNotCreated) {
		writeJSON(w, memoryConflictsResponse{Conflicts: []store.MemoryConflict{}})
		return
	}
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer ix.Close()
	conflicts, err := ix.MemoryConflicts(r.URL.Query().Get("id"), conflictsPageMax())
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if conflicts == nil {
		conflicts = []store.MemoryConflict{}
	}
	writeJSON(w, memoryConflictsResponse{Conflicts: conflicts})
}

type teamContentSentResponse struct {
	Session string `json:"session"`
	Chunks  int    `json:"chunks"` // accepted by the server and not covered by a later deletion
}

// handleTeamContentSent says how much of one session's content the server holds from this
// device — the fact the footer's "Delete what was sent" renders from (decision 9).
func handleTeamContentSent(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	key, found, err := governor.ix.ContentSessionKeyFor(r.URL.Query().Get("runtime"), r.URL.Query().Get("session"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: "the session could not be read: " + err.Error()})
		return
	}
	out := teamContentSentResponse{}
	if found {
		out.Session = key
		if out.Chunks, err = governor.ix.ContentSent(key); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, teamErrorResponse{Error: "the outbox could not be read: " + err.Error()})
			return
		}
	}
	writeJSON(w, out)
}

// deleteSentContent is "delete what was sent" (decision 9, O-3): a tombstone naming the
// session's WIRE id (never the local one, invariant 8), committed to what the server
// accepted, and the session's consent withdrawn in the same transaction. Allowed under an
// organization mandate too (O-8): the server audits it, and content produced afterwards
// is shared per the mandate.
func (t *teamLinker) deleteSentContent(key string) (int, error) {
	t.mu.Lock()
	deviceID, now := t.deviceID, t.now()
	t.mu.Unlock()
	n, err := governor.ix.EnqueueContentTombstone(key, engine.WireSessionID(deviceID, key), now.Unix())
	if err != nil {
		return 0, err
	}
	t.linkEvent("team.content.delete", "session "+key+": deletion of "+countNoun(n, "sent content chunk")+" queued; sharing turned off for the session")
	return n, nil
}

// conflictsPageMax is team.json's conflicts_page_max (its embedded default when the
// device has never linked).
func conflictsPageMax() int {
	if team != nil {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.doc.ConflictsPageMax
	}
	def, _ := teamlink.Default() // the embedded document always decodes (teamlink's own test)
	return def.ConflictsPageMax
}

type teamMemoryTakeRequest struct {
	ID string `json:"id"`
}

// teamMemoryTakeResponse is POST /api/team/memory/take-team-version's body: the record
// whose local edit was given up. The team's revision lands with the next pull.
type teamMemoryTakeResponse struct {
	ID string `json:"id"`
}

// handleTeamMemoryTake gives up this device's edit of a team record and pulls the team's
// current revision back (FR-6); the local body is kept as a conflict copy when it lands.
// 409 when not linked, when the team does not hold the record, or while a revision of it
// is still being sent; 404 for an unknown id.
func handleTeamMemoryTake(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req teamMemoryTakeRequest
	config, _ := consoleConfig()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, config.WriteBytesMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.ID == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {\"id\": \"<record id>\"}"})
		return
	}
	err := governor.ix.TakeTeamVersion(req.ID)
	switch {
	case errors.Is(err, store.ErrMemoryNotFound):
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, teamErrorResponse{Error: err.Error()})
		return
	case errors.Is(err, store.ErrMemoryNotTeam), errors.Is(err, store.ErrMemoryInFlight):
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, teamErrorResponse{Error: err.Error()})
		return
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: "the team's version could not be requested: " + err.Error()})
		return
	}
	team.pullMemorySoon()
	writeJSON(w, teamMemoryTakeResponse(req))
}
