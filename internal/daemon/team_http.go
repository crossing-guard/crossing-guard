package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// GET /api/team, POST /api/team/link, POST /api/team/unlink — the console's and the
// CLI's window onto the link. Typed responses only (ADR 0022); documented in
// docs/public/reference/loopback-api.md.

type teamDeviceResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type teamPendingResponse struct {
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Fingerprint     string `json:"fingerprint"`
	ExpiresAt       string `json:"expires_at"`
	Server          string `json:"server"`
	Name            string `json:"name"`
}

type teamReportResponse struct {
	LastAt   string          `json:"last_at,omitempty"`
	Outcome  string          `json:"outcome,omitempty"`
	Error    string          `json:"error,omitempty"`
	RecordID string          `json:"record_id,omitempty"`
	NextAt   string          `json:"next_at,omitempty"`
	Interval string          `json:"interval"`
	Document json.RawMessage `json:"document,omitempty"` // the exact document last sent
}

// teamStatusResponse says where this device stands with a team server. The state
// names one of six facts; problem says why for the three that are not "linked".
type teamStatusResponse struct {
	State        string                `json:"state"`
	Problem      string                `json:"problem,omitempty"`
	Server       string                `json:"server,omitempty"`
	Organization teamlink.Organization `json:"organization,omitzero"`
	Device       teamDeviceResponse    `json:"device"`
	LinkedAt     string                `json:"linked_at,omitempty"`
	ApprovedBy   string                `json:"approved_by,omitempty"`
	// ApprovedByName is the approver's display name from the member directory this
	// device holds; empty when the directory does not list them.
	ApprovedByName string               `json:"approved_by_name,omitempty"`
	Pending        *teamPendingResponse `json:"pending,omitempty"`
	Report         teamReportResponse   `json:"report"`
	Sync           teamlink.Sync        `json:"sync"`
	Sends          []string             `json:"sends"` // what this device sends the server, in words
	Outbox         *teamOutboxResponse  `json:"outbox,omitempty"`
	Content        teamContentResponse  `json:"content"`
	// Memory is the shared-memory sync state (team item 5); present while linked.
	Memory *teamMemoryResponse `json:"memory,omitempty"`
}

// teamContentResponse is session content's state on this device (D-12): which sessions
// the developer opted in and since when, whether an adopted bundle (any scope) mandates
// content (and which), and the per-session cap. Empty lists mean content is off.
type teamContentResponse struct {
	OptedIn    []teamContentSession `json:"opted_in"`
	Mandate    *contentMandate      `json:"mandate"`
	SessionCap int64                `json:"session_cap"`
	Retention  string               `json:"retention"`
}

type teamContentSession struct {
	Session string `json:"session"` // "<runtime>/<native id>"
	Since   string `json:"since"`
}

// teamContentRetention states what turning content off does and does not do, so the
// console never implies more than is true.
const teamContentRetention = "turning content off stops what has not been sent; content the server already holds stays there until you delete what was sent for that session"

// teamOutboxResponse is the drain's state for the developer's own console (invariant 13's
// visibility): the backlog, what is parked and why, and what the server refused. The
// backlog grows by the local event rate while linked; high_water flags it, never caps it.
type teamOutboxResponse struct {
	Pending       int                       `json:"pending"`
	ByKind        map[string]int            `json:"by_kind"`
	OldestAt      string                    `json:"oldest_at,omitempty"`
	HighWater     int                       `json:"high_water"`
	OverHighWater bool                      `json:"over_high_water"`
	Parked        []teamParkedKind          `json:"parked"`
	Interval      string                    `json:"interval"`
	Batch         int                       `json:"batch"`
	LastAt        string                    `json:"last_at,omitempty"`
	Outcome       string                    `json:"outcome,omitempty"`
	Error         string                    `json:"error,omitempty"`
	Accepted      int64                     `json:"accepted"`
	DeadLetter    map[string]map[string]int `json:"dead_letter"` // refused by the server
	Refused       map[string]map[string]int `json:"refused"`     // refused by this device
	Conflicts     map[string]int            `json:"conflicts"`
}

// teamParkedKind is a kind the drain holds back: the server refused it as unsupported
// (re-probed at next_probe_at), or this build has no encoder for it yet.
type teamParkedKind struct {
	Kind        string `json:"kind"`
	Reason      string `json:"reason"`
	NextProbeAt string `json:"next_probe_at,omitempty"`
}

// teamSendsLines states, in words, what each pushed kind carries — the "sends:" line.
var teamSendsLines = []string{
	"events: verb, tool, repository-relative target, frozen tags (checked against the shipped secret patterns, matches redacted), decision, rule, layer, chain hashes — no bodies",
	"sessions: the three identities, the event span, and this device's own chain report (a claim)",
	"checkpoint facts: kind, status, class, base and head revisions, digests — no bodies, no paths",
	"device report: per runtime — attached, last live event, last canary block; rulebook digest; daemon and store versions",
	"memory: active records you shared at organization scope, or at repository scope when the repository is identified by its remote — title, body, tags (checked against the shipped secret patterns, matches redacted); never a record identified only by a folder name, a draft, or a rejected record",
	"deletions: a shared record you delete, and session content you ask to have deleted — ids and a content hash, no bodies",
}

func handleTeamStatus(w http.ResponseWriter, _ *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	writeJSON(w, team.status())
}

// directoryNames is the member directory this device holds for its linked
// organization, as user id → display name. It is read from the store without t.mu;
// a directory that cannot be read names nobody.
func (t *teamLinker) directoryNames() map[string]string {
	names := map[string]string{}
	organization, err := governor.ix.LinkedOrganization()
	if err != nil || organization == "" {
		return names
	}
	members, err := governor.ix.TeamMembers(organization)
	if err != nil {
		return names
	}
	for _, member := range members {
		names[member.UserID] = member.DisplayName
	}
	return names
}

func (t *teamLinker) status() teamStatusResponse {
	summary, summaryErr := governor.ix.OutboxPendingSummary()
	optIns, optErr := governor.ix.ContentOptIns()
	memory, memoryErr := t.memorySummary()
	// The adoption records are read before the lock is taken (a file with its own
	// lock); the mandate is decided from them under it, against the linked organization.
	layers := readAdoptionRecords(t.storeDir)
	members := t.directoryNames()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := teamStatusResponse{State: t.state, Problem: t.problem, Server: t.doc.Server, Organization: t.doc.Organization,
		Device:   teamDeviceResponse{ID: t.deviceID, Name: t.doc.Device.Name, Platform: t.doc.Device.Platform, Fingerprint: t.doc.Device.Fingerprint},
		LinkedAt: t.doc.LinkedAt, ApprovedBy: t.doc.ApprovedBy, Sync: t.doc.Sync,
		Report: teamReportResponse{Interval: t.doc.ReportInterval.String(), Outcome: t.report.Outcome, Error: t.report.Error, RecordID: t.report.RecordID, Document: t.report.Document},
		Sends:  append([]string(nil), teamSendsLines...)}
	out.ApprovedByName = members[t.doc.ApprovedBy]
	if t.state == teamLinked || summary.Pending > 0 {
		out.Outbox = t.outboxLocked(summary, summaryErr)
	}
	out.Content = teamContentResponse{OptedIn: []teamContentSession{}, Mandate: contentMandateIn(layers, t.doc.Organization.ID, t.now()),
		SessionCap: t.doc.ContentSessionCap, Retention: teamContentRetention}
	if optErr == nil {
		for key, since := range optIns {
			if key != store.ContentOptInAll {
				out.Content.OptedIn = append(out.Content.OptedIn, teamContentSession{Session: key, Since: time.Unix(since, 0).UTC().Format(time.RFC3339)})
			}
		}
		sort.Slice(out.Content.OptedIn, func(i, j int) bool { return out.Content.OptedIn[i].Session < out.Content.OptedIn[j].Session })
	}
	if t.state == teamLinked {
		out.Memory = t.memoryStatusLocked(memory, memoryErr)
	}
	if !t.report.LastAt.IsZero() {
		out.Report.LastAt = t.report.LastAt.UTC().Format(time.RFC3339)
		if t.state == teamLinked {
			out.Report.NextAt = t.report.LastAt.Add(t.doc.ReportInterval.Duration).UTC().Format(time.RFC3339)
		}
	}
	if t.pending != nil {
		out.Pending = &teamPendingResponse{UserCode: t.pending.UserCode, VerificationURL: t.pending.VerificationURL, Fingerprint: t.pending.Fingerprint,
			ExpiresAt: t.pending.ExpiresAt.UTC().Format(time.RFC3339), Server: t.pending.Server, Name: t.pending.Name}
	}
	return out
}

type teamLinkRequest struct {
	Server string `json:"server"`
	Name   string `json:"name"`
}

type teamErrorResponse struct {
	Error string `json:"error"`
}

// handleTeamLink starts an enrollment. Its refusals carry three codes with three
// meanings: 409 the link's state refuses (linked, pending, inconsistent — the state is
// the fact), 400 the server URL is not one the daemon will sign to (malformed, or
// plaintext beyond loopback), 502 the enrollment could not be started because the
// server could not be reached or answered with a refusal.
func handleTeamLink(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req teamLinkRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Server == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {\"server\": \"https://…\", \"name\": \"optional device name\"}"})
		return
	}
	p, err := team.beginLink(req.Server, req.Name)
	if err != nil {
		status := http.StatusConflict
		var badURL *teamlink.ErrServerURL
		var refused *teamlink.ErrEnrollmentStart
		if errors.As(err, &badURL) {
			status = http.StatusBadRequest
		} else if errors.As(err, &refused) {
			status = http.StatusBadGateway
		}
		w.WriteHeader(status)
		writeJSON(w, teamErrorResponse{Error: err.Error()})
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, teamPendingResponse{UserCode: p.UserCode, VerificationURL: p.VerificationURL, Fingerprint: p.Fingerprint,
		ExpiresAt: p.ExpiresAt.UTC().Format(time.RFC3339), Server: p.Server, Name: p.Name})
}

type teamUnlinkResponse struct {
	ServerAcknowledged bool   `json:"server_acknowledged"`
	Note               string `json:"note"`
}

func handleTeamUnlink(w http.ResponseWriter, _ *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	acked, err := team.unlink()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: err.Error()})
		return
	}
	note := "unlinked; the server revoked this device's key and deleted nothing"
	if !acked {
		note = "unlinked locally; the server could not be told, so it still lists this device until an admin revokes it"
	}
	writeJSON(w, teamUnlinkResponse{ServerAcknowledged: acked, Note: note})
}

func (t *teamLinker) outboxLocked(summary store.OutboxSummary, summaryErr error) *teamOutboxResponse {
	p := t.report.Push
	o := &teamOutboxResponse{Pending: summary.Pending, ByKind: summary.ByKind, HighWater: t.doc.OutboxHighWater,
		Interval: t.doc.PushInterval.String(), Batch: t.doc.PushBatch, Outcome: p.Outcome, Error: p.Error,
		Accepted: p.Accepted, DeadLetter: p.DeadLetter, Refused: p.Refused, Conflicts: p.Conflicts, Parked: []teamParkedKind{}}
	if summaryErr != nil {
		o.Error = "reading the outbox: " + summaryErr.Error()
	}
	o.OverHighWater = o.Pending > o.HighWater
	if summary.OldestAt > 0 {
		o.OldestAt = time.Unix(summary.OldestAt, 0).UTC().Format(time.RFC3339)
	}
	if !p.LastAt.IsZero() {
		o.LastAt = p.LastAt.UTC().Format(time.RFC3339)
	}
	for kind := range pushKindsWithoutEncoder {
		if summary.ByKind[kind] > 0 {
			o.Parked = append(o.Parked, teamParkedKind{Kind: kind, Reason: "no encoder in this build yet (item 5); held, nothing lost"})
		}
	}
	for kind, due := range t.parked {
		reason := "the server answered unsupported_kind; held, nothing lost"
		if kind == teamwire.KindSession {
			// Session projections are rebuilt from the store, not queued: nothing is
			// held, they are simply not sent until the server accepts the kind.
			reason = "the server answered unsupported_kind; not sent until it accepts them (rebuilt from this device's events, nothing to hold)"
		}
		o.Parked = append(o.Parked, teamParkedKind{Kind: kind, Reason: reason, NextProbeAt: due.UTC().Format(time.RFC3339)})
	}
	sort.Slice(o.Parked, func(i, j int) bool { return o.Parked[i].Kind < o.Parked[j].Kind })
	return o
}

type teamContentRequest struct {
	Runtime string `json:"runtime"`
	Session string `json:"session"`
	Enabled bool   `json:"enabled"`
	// DeleteSent asks the server to erase this session's content it already holds from
	// this device, and turns sharing off for the session (team item 5 decision 9). It
	// is only meaningful with enabled false.
	DeleteSent bool `json:"delete_sent"`
}

// handleTeamContent opts one session's content in or out (D-12's consent path). It is a
// local action on a linked device: 409 when not linked (there is nowhere to send), 400
// for a malformed body. Forward-only: turning it on sends what is captured from now on;
// turning it off stops what has not been sent. Each change is chained under the
// daemon's team session, naming the session — bookkeeping, never in the transcript.
func handleTeamContent(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req teamContentRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.Session) == "" || len(req.Session) > 512 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {\"runtime\": \"<runtime>\", \"session\": \"<id>\", \"enabled\": true|false, \"delete_sent\": optional true}"})
		return
	}
	team.mu.Lock()
	linked := team.state == teamLinked
	team.mu.Unlock()
	if !linked {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, teamErrorResponse{Error: "this device is not linked to a team; there is nowhere to send content"})
		return
	}
	key, found, err := governor.ix.ContentSessionKeyFor(req.Runtime, req.Session)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: "the session could not be read: " + err.Error()})
		return
	}
	if !found {
		// A consent keyed to anything but the session's stored events would read "on"
		// while nothing is ever sent (postwork C10).
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "this device has no governed events for that session; content consent is recorded per governed session"})
		return
	}
	if req.DeleteSent {
		if req.Enabled {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, teamErrorResponse{Error: "delete_sent turns sharing off for the session; send it with enabled false"})
			return
		}
		switch _, err := team.deleteSentContent(key); {
		case errors.Is(err, store.ErrNothingSent):
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, teamErrorResponse{Error: "the server holds no content from this device for that session"})
			return
		case err != nil:
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, teamErrorResponse{Error: "the deletion could not be queued: " + err.Error()})
			return
		}
		writeJSON(w, team.status().Content)
		return
	}
	if err := governor.ix.SetContentOptIn(key, req.Enabled, team.now().Unix()); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: "the consent could not be recorded: " + err.Error()})
		return
	}
	kind, detail := "team.content.optin", "session "+key+" opted in: its captured tool inputs are sent from now on"
	if !req.Enabled {
		kind, detail = "team.content.optout", "session "+key+" opted out: "+teamContentRetention
	}
	team.linkEvent(kind, detail)
	writeJSON(w, team.status().Content)
}
