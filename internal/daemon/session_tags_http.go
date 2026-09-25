package daemon

// Owner tags, the owner's one-line note, and saved views over HTTP
// (session-organization implementation plan §4.3). Every body is a declared
// type and every request is size-bounded from configuration.
//
// A tag or note request names sessions the way the rail row does — runtime and
// the row's id. This file resolves each to a scanned session through the
// runtime adapter and stores under the adapter's canonical id; an identity the
// client supplies is never trusted as canonical. A session the scan no longer
// finds can still be addressed by the id its tag was stored under, so a tagged
// session whose transcript is gone can be untagged.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/store"
)

type sessionRef struct {
	Runtime string `json:"runtime"`
	ID      string `json:"id"`
}

type sessionTagsRequest struct {
	Sessions []sessionRef                 `json:"sessions"`
	Apply    []store.SessionOwnerTagValue `json:"apply,omitempty"`
	Retract  []store.SessionOwnerTagValue `json:"retract,omitempty"`
}

// sessionTagsResponse returns each addressed session as the rail would now
// show it, so the browser patches rows in place instead of redrawing the rail.
type sessionTagsResponse struct {
	Sessions []railSession `json:"sessions"`
}

type sessionTagRenameRequest struct {
	From store.SessionOwnerTagValue `json:"from"`
	To   store.SessionOwnerTagValue `json:"to"`
}

type sessionTagPurgeRequest struct {
	Tag store.SessionOwnerTagValue `json:"tag"`
}

type sessionTagChangeResponse struct {
	Sessions int `json:"sessions"`
}

type sessionNoteRequest struct {
	Session sessionRef `json:"session"`
	Text    string     `json:"text"`
}

type sessionVocabularyResponse struct {
	// Tags are the owner's own, most recently used first: the only suggestions
	// for applying a tag, and the source of the header's one-click toggles.
	Tags []store.SessionOwnerTagUse `json:"tags"`
	// Known lists every distinct tag present on any session, from any source,
	// for completing a filter. It is what exists, never what might.
	Known []sessionKnownTag `json:"known"`
}

type sessionKnownTag struct {
	Key        string `json:"key,omitempty"`
	Value      string `json:"value"`
	Provenance string `json:"provenance"`
	Sessions   int    `json:"sessions"`
}

type sessionViewWriteRequest struct {
	StateToken string           `json:"state_token"`
	View       SavedSessionView `json:"view"`
}

type sessionViewOrderRequest struct {
	StateToken string   `json:"state_token"`
	Order      []string `json:"order"`
}

func registerSessionOrganizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/session-tags", handleSessionTagsGet)
	mux.HandleFunc("POST /api/session-tags", handleSessionTags)
	mux.HandleFunc("POST /api/session-tags/rename", handleSessionTagRename)
	mux.HandleFunc("POST /api/session-tags/purge", handleSessionTagPurge)
	mux.HandleFunc("GET /api/session-tags/vocabulary", handleSessionTagVocabulary)
	mux.HandleFunc("PUT /api/session-notes", handleSessionNote)
	mux.HandleFunc("GET /api/session-views", handleSessionViewsGet)
	mux.HandleFunc("POST /api/session-views", handleSessionViewCreate)
	mux.HandleFunc("PUT /api/session-views/order", handleSessionViewOrder)
	mux.HandleFunc("PUT /api/session-views/{view}", handleSessionViewUpdate)
	mux.HandleFunc("DELETE /api/session-views/{view}", handleSessionViewDelete)
}

func sessionOrganizationConfig() ConsoleSessionOrganization {
	config, _ := consoleConfig()
	return config.SessionOrganization
}

func sessionViewsDataDir() string { return filepath.Dir(indexPath()) }

// decodeSessionOrganizationBody reads one bounded, strictly typed body.
func decodeSessionOrganizationBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, sessionOrganizationConfig().TagRequestBytesMax)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		http.Error(w, "the request could not be read: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func sessionOwnerIndex(w http.ResponseWriter) *store.Index {
	if governor == nil || governor.ix == nil {
		http.Error(w, "tags are unavailable: the store is not open", http.StatusServiceUnavailable)
		return nil
	}
	return governor.ix
}

// resolveSessionTargets turns rail references into store targets. A reference
// the scan cannot place is accepted only if an owner tag already names it.
func resolveSessionTargets(refs []sessionRef, scan []SessionSummary, snapshot sessionTagSnapshot) ([]store.SessionOwnerTarget, []SessionSummary, error) {
	targets := make([]store.SessionOwnerTarget, 0, len(refs))
	rows := make([]SessionSummary, 0, len(refs))
	candidates := append(append([]SessionSummary(nil), scan...), snapshot.vanished(scan)...)
	for _, ref := range refs {
		if ref.Runtime == "" || ref.ID == "" {
			return nil, nil, errSessionRefMalformed
		}
		row, found := findSession(candidates, ref)
		if !found {
			return nil, nil, fmt.Errorf("no session %s/%s", ref.Runtime, ref.ID)
		}
		rows = append(rows, row)
		targets = append(targets, store.SessionOwnerTarget{Runtime: row.Runtime, SessionID: harvest.CanonicalID(row),
			Title: row.Title, Repository: sessionRepositoryKey(row), Cwd: row.Cwd, TouchedAt: row.Modified.Unix()})
	}
	return targets, rows, nil
}

func findSession(rows []SessionSummary, ref sessionRef) (SessionSummary, bool) {
	if ref.Runtime == "" || ref.ID == "" {
		return SessionSummary{}, false
	}
	for _, row := range rows {
		if row.Runtime == ref.Runtime && (row.ID == ref.ID || harvest.MatchID(row, ref.ID)) {
			return row, true
		}
	}
	return SessionSummary{}, false
}

// handleSessionTagsGet answers for one session as the rail would show it, with
// what detectors saw: the open session's header is built from this.
func handleSessionTagsGet(w http.ResponseWriter, r *http.Request) {
	ref := sessionRef{Runtime: r.URL.Query().Get("runtime"), ID: r.URL.Query().Get("id")}
	if len(ref.Runtime) > maxSessionRuntimeBytes || len(ref.ID) > maxSessionIdentityBytes {
		http.Error(w, "session identity is too long", http.StatusBadRequest)
		return
	}
	now := time.Now()
	_, rows, err := resolveSessionTargets([]sessionRef{ref}, ScanSessions(), sessionTagSnapshots.get(now))
	if err != nil {
		writeSessionRefError(w, err)
		return
	}
	writeJSON(w, sessionTagsResponse{Sessions: decorateSessions(rows, now)})
}

func handleSessionTags(w http.ResponseWriter, r *http.Request) {
	var request sessionTagsRequest
	if !decodeSessionOrganizationBody(w, r, &request) {
		return
	}
	limits := sessionOrganizationConfig()
	if len(request.Sessions) == 0 || len(request.Sessions) > limits.BulkSelectionMax {
		http.Error(w, fmt.Sprintf("name between 1 and %d sessions", limits.BulkSelectionMax), http.StatusBadRequest)
		return
	}
	if changes := len(request.Apply) + len(request.Retract); changes == 0 || changes > limits.TagsPerRequestMax {
		http.Error(w, fmt.Sprintf("name between 1 and %d tags to apply or retract", limits.TagsPerRequestMax), http.StatusBadRequest)
		return
	}
	ix := sessionOwnerIndex(w)
	if ix == nil {
		return
	}
	now := time.Now()
	targets, rows, err := resolveSessionTargets(request.Sessions, ScanSessions(), sessionTagSnapshots.get(now))
	if err != nil {
		writeSessionRefError(w, err)
		return
	}
	// One transaction: a request that takes one tag off and puts another on
	// either does both or neither. The snapshot is dropped whatever happened,
	// so a failed write can never leave the rail showing what it replaced.
	err = ix.ChangeSessionOwnerTags(targets, request.Apply, request.Retract, now.Unix())
	sessionTagSnapshots.drop()
	if err != nil {
		writeSessionOwnerError(w, err)
		return
	}
	writeJSON(w, sessionTagsResponse{Sessions: decorateSessions(rows, time.Now())})
}

var errSessionRefMalformed = errors.New("a session needs a runtime and an id")

// writeSessionRefError tells a reference that was never valid (400) from one
// that names no session (404).
func writeSessionRefError(w http.ResponseWriter, err error) {
	if errors.Is(err, errSessionRefMalformed) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusNotFound)
}

func writeSessionOwnerError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrSessionOwnerTagInvalid) {
		http.Error(w, strings.TrimPrefix(err.Error(), store.ErrSessionOwnerTagInvalid.Error()+": "), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// decorateSessions describes sessions in full — every tag, the detector facts
// and the note — for the open session's header and for patching a row after a
// change. It is never capped: the header must be able to show and remove the
// owner's seventh tag.
func decorateSessions(rows []SessionSummary, now time.Time) []railSession {
	snapshot := sessionTagSnapshots.get(now)
	out := make([]railSession, 0, len(rows))
	for _, row := range rows {
		out = append(out, snapshot.decorate(row))
	}
	return out
}

func handleSessionTagRename(w http.ResponseWriter, r *http.Request) {
	var request sessionTagRenameRequest
	if !decodeSessionOrganizationBody(w, r, &request) {
		return
	}
	ix := sessionOwnerIndex(w)
	if ix == nil {
		return
	}
	changed, err := ix.RenameSessionOwnerTag(request.From, request.To, time.Now().Unix())
	if err != nil {
		writeSessionOwnerError(w, err)
		return
	}
	sessionTagSnapshots.drop()
	writeJSON(w, sessionTagChangeResponse{Sessions: changed})
}

func handleSessionTagPurge(w http.ResponseWriter, r *http.Request) {
	var request sessionTagPurgeRequest
	if !decodeSessionOrganizationBody(w, r, &request) {
		return
	}
	ix := sessionOwnerIndex(w)
	if ix == nil {
		return
	}
	removed, err := ix.PurgeSessionOwnerTag(request.Tag)
	if err != nil {
		writeSessionOwnerError(w, err)
		return
	}
	sessionTagSnapshots.drop()
	writeJSON(w, sessionTagChangeResponse{Sessions: removed})
}

func handleSessionNote(w http.ResponseWriter, r *http.Request) {
	var request sessionNoteRequest
	if !decodeSessionOrganizationBody(w, r, &request) {
		return
	}
	ix := sessionOwnerIndex(w)
	if ix == nil {
		return
	}
	now := time.Now()
	targets, rows, err := resolveSessionTargets([]sessionRef{request.Session}, ScanSessions(), sessionTagSnapshots.get(now))
	if err != nil {
		writeSessionRefError(w, err)
		return
	}
	err = ix.PutSessionOwnerNote(targets[0], request.Text, now.Unix())
	sessionTagSnapshots.drop()
	if err != nil {
		writeSessionOwnerError(w, err)
		return
	}
	writeJSON(w, sessionTagsResponse{Sessions: decorateSessions(rows, time.Now())})
}

func handleSessionTagVocabulary(w http.ResponseWriter, r *http.Request) {
	ix := sessionOwnerIndex(w)
	if ix == nil {
		return
	}
	limits := sessionOrganizationConfig()
	tags, err := ix.SessionOwnerTagVocabulary(limits.VocabularyMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionVocabularyResponse{Tags: tags, Known: knownSessionTags(sessionTagSnapshots.get(time.Now()), limits.VocabularyMax)})
}

// knownSessionTags counts every distinct tag in the snapshot, most used first.
func knownSessionTags(snapshot sessionTagSnapshot, limit int) []sessionKnownTag {
	counts := map[sessionKnownTag]int{}
	for _, tags := range snapshot.owner {
		for _, tag := range tags {
			counts[sessionKnownTag{Key: tag.Key, Value: tag.Value, Provenance: string(engine.UserAsserted)}]++
		}
	}
	for _, tags := range snapshot.agent {
		seen := map[string]bool{}
		for _, tag := range tags {
			if !seen[tag.Tag] {
				seen[tag.Tag] = true
				counts[sessionKnownTag{Value: tag.Tag, Provenance: provenanceModelClaimed}]++
			}
		}
	}
	for _, facets := range snapshot.facets {
		for _, facet := range facets {
			if facet.Value != "" {
				counts[sessionKnownTag{Key: facet.Key, Value: facet.Value, Provenance: facet.Provenance}]++
			}
		}
	}
	return rankKnownSessionTags(counts, limit)
}

// rankKnownSessionTags orders distinct tags by how many sessions carry them.
func rankKnownSessionTags(counts map[sessionKnownTag]int, limit int) []sessionKnownTag {
	out := make([]sessionKnownTag, 0, len(counts))
	for tag, sessions := range counts {
		tag.Sessions = sessions
		out = append(out, tag)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
