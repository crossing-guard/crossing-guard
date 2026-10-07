package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/schemas"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// handoffFake is a team server that speaks the handoff wire types exactly: the two push
// kinds (through pushFake, which validates every body against its wire schema), the
// handoff pull as its own request, and the member directory. older makes it the server
// before handoffs: the pull is answered 400 and both kinds unsupported_kind.
type handoffFake struct {
	pushFake
	hmu          sync.Mutex
	rows         []teamwire.PullRow
	members      []teamwire.Member
	self         string
	older        bool
	onePerPage   bool
	handoffPulls atomic.Int64
	bootstraps   atomic.Int64 // handoff pulls that carried bootstrap
	memoryPulls  atomic.Int64
}

func (f *handoffFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case teamwire.RoutePull:
		var in teamwire.PullRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		if len(in.Kinds) != 1 || in.Kinds[0] != teamwire.KindHandoff {
			f.memoryPulls.Add(1)
			_ = json.NewEncoder(w).Encode(teamwire.PullResponse{Rows: []teamwire.PullRow{}, Cursor: in.Cursor})
			return
		}
		f.handoffPulls.Add(1)
		if in.Bootstrap {
			f.bootstraps.Add(1)
		}
		f.hmu.Lock()
		defer f.hmu.Unlock()
		if f.older {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(teamwire.ErrorBody{Error: teamwire.Error{Code: "invalid_request", Message: "kinds"}})
			return
		}
		out := teamwire.PullResponse{Rows: []teamwire.PullRow{}, Cursor: in.Cursor}
		for _, row := range f.rows {
			if row.Seq <= in.Cursor {
				continue
			}
			if f.onePerPage && len(out.Rows) == 1 {
				out.More = true
				break
			}
			out.Rows = append(out.Rows, row)
			out.Cursor = row.Seq
		}
		_ = json.NewEncoder(w).Encode(out)
	case teamwire.RouteMembers:
		f.hmu.Lock()
		defer f.hmu.Unlock()
		if f.older {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(teamwire.MembersResponse{Members: f.members, Self: f.self, ServerTime: time.Now().Unix()})
	case teamwire.RouteAck:
		_ = json.NewEncoder(w).Encode(teamwire.AckResponse{})
	default:
		f.pushFake.ServeHTTP(w, r)
	}
}

func (f *handoffFake) serve(rows ...teamwire.PullRow) {
	f.hmu.Lock()
	f.rows = append(f.rows, rows...)
	f.hmu.Unlock()
}

// handoffRig is a linked test device with a Claude session to hand off.
type handoffRig struct {
	t       *testing.T
	tl      *teamLinker
	ix      *store.Index
	fake    *handoffFake
	mux     *http.ServeMux
	home    string
	cwd     string
	session string
}

const (
	// Member ids as the server mints them: usr_ and a ULID (the wire schema's typed id).
	handoffMe          = "usr_01J8ZQ4M7T2V9K3NXW5R6YHBCM"
	handoffTeammate    = "usr_01J8ZQ4M7T2V9K3NXW5R6YHBCK"
	handoffTestSession = "3f6c1a2e-9d4b-4c8a-b1f0-7e5d2c9a8b17"
	// handoffPlanted is a marker the privacy test looks for outside the handoff's own
	// columns; handoffSecret is a shape one of the named secret patterns matches.
	handoffPlanted = "ZEBRA-PLANTED-7731"
	handoffSecret  = "AKIAIOSFODNN7EXAMPLE"
)

// writeHandoffSession writes a Claude transcript under home whose folder is cwd: a
// request, an edit of a file under cwd, and a last agent message with a list.
func writeHandoffSession(t *testing.T, home, cwd string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-work-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(v map[string]any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	body := line(map[string]any{"type": "user", "uuid": "u1", "sessionId": handoffTestSession, "cwd": cwd, "timestamp": "2026-10-04T15:00:00Z",
		"message": map[string]any{"role": "user", "content": "Please finish the outbox drain " + handoffPlanted + " using key " + handoffSecret}}) +
		line(map[string]any{"type": "assistant", "uuid": "a1", "sessionId": handoffTestSession, "timestamp": "2026-10-04T15:00:05Z",
			"message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "Edit", "input": map[string]any{"file_path": filepath.Join(cwd, "internal", "x.go")}}}}}) +
		line(map[string]any{"type": "assistant", "uuid": "a2", "sessionId": handoffTestSession, "timestamp": "2026-10-04T15:00:09Z",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text",
				"text": "I edited " + filepath.Join(cwd, "internal", "x.go") + ". Two things remain:\n- Drain handoff kinds first\n- Add the parked-kind retry\n"}}}})
	if err := os.WriteFile(filepath.Join(dir, handoffTestSession+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newHandoffRig(t *testing.T, link bool) *handoffRig {
	t.Helper()
	tl, ix, home := teamTestLinker(t)
	cwd := filepath.Join(home, "work", "repo")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHandoffSession(t, home, cwd)
	rig := &handoffRig{t: t, tl: tl, ix: ix, home: home, cwd: cwd, session: handoffTestSession, mux: http.NewServeMux()}
	registerTeamHandoffRoutes(rig.mux)
	rig.fake = &handoffFake{self: handoffMe, members: []teamwire.Member{{UserID: handoffMe, DisplayName: "Me"}, {UserID: handoffTeammate, DisplayName: "Teammate"}}}
	rig.fake.answer = map[string]string{}
	rig.fake.status.Store("approved")
	if link {
		linkTo(t, tl, rig.fake)
		if !tl.refreshMembersNow() {
			t.Fatal("the directory refresh needs a link")
		}
	}
	return rig
}

func (r *handoffRig) call(method, path string, body any, into any) (int, teamHandoffError) {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest(method, path, reader))
	var refusal teamHandoffError
	if rec.Code != http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
		return rec.Code, refusal
	}
	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			r.t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, refusal
}

func (r *handoffRig) request(to string) teamHandoffSendRequest {
	return teamHandoffSendRequest{Runtime: "claude", SessionID: r.session, To: to, Title: "Finish the outbox drain",
		BodyMarkdown: "Edited " + filepath.Join(r.cwd, "internal", "x.go") + " and read /Users/alice/notes.txt with " + handoffSecret,
		Remaining:    []string{"Drain handoff kinds first", "Check " + filepath.Join(r.cwd, "store", "y.go")}}
}

// preview asks for the preview and returns it with the request completed for the send.
func (r *handoffRig) preview(req teamHandoffSendRequest) (teamHandoffSendResponse, teamHandoffSendRequest) {
	r.t.Helper()
	req.Preview = true
	var shown teamHandoffSendResponse
	if code, refusal := r.call("POST", "/api/team/handoffs/send", req, &shown); code != http.StatusOK {
		r.t.Fatalf("preview: %d %+v", code, refusal)
	}
	req.Preview, req.ID, req.CreatedAt, req.WireHash = false, shown.ID, shown.CreatedAt, shown.WireHash
	return shown, req
}

func (r *handoffRig) send(req teamHandoffSendRequest) teamHandoffSendResponse {
	r.t.Helper()
	_, req = r.preview(req)
	var sent teamHandoffSendResponse
	if code, refusal := r.call("POST", "/api/team/handoffs/send", req, &sent); code != http.StatusOK {
		r.t.Fatalf("send: %d %+v", code, refusal)
	}
	return sent
}

func (r *handoffRig) list() teamHandoffsResponse {
	r.t.Helper()
	var out teamHandoffsResponse
	if code, refusal := r.call("GET", "/api/team/handoffs", nil, &out); code != http.StatusOK {
		r.t.Fatalf("list: %d %+v", code, refusal)
	}
	return out
}

// drain runs one drain tick after any the routes started themselves have finished.
func (r *handoffRig) drain() {
	r.t.Helper()
	r.tl.pushOnce()
	if len(r.fake.invalid) > 0 {
		r.t.Fatalf("the device pushed a record its wire schema refuses: %v", r.fake.invalid)
	}
}

// pulledDocument builds a delivery row as the server serves a recipient's device.
func pulledDocument(t *testing.T, seq int64, title, body, state string) (teamwire.PullRow, teamwire.HandoffRecord) {
	t.Helper()
	rec := teamwire.HandoffRecord{SchemaVersion: teamwire.HandoffSchemaVersion, ID: engine.NewTypedID(teamwire.HandoffIDPrefix),
		Session:   teamwire.SessionIdentity{ID: engine.NewTypedID("ses"), Runtime: "codex", NativeID: "native-9", CatalogID: "catalog-9", ResumeID: "resume-9"},
		CreatedAt: "2026-10-04T16:00:00Z", CreatedBy: teamwire.Actor{Type: "user", ID: "member"},
		Recipient: teamwire.HandoffRecipient{UserID: handoffMe}, Title: title, BodyMarkdown: body,
		Remaining: []string{"one thing"}, Agents: []teamwire.HandoffAgent{}, GovernanceState: teamwire.HandoffGovernance{Tags: []teamwire.HandoffTag{}},
		AnchorsScope: teamwire.HandoffAnchorsScope}
	rec.ContentHash = teamwire.HandoffWireHash(rec)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := schemas.Violations(teamwire.SchemaForKind(teamwire.KindHandoff), raw); err != nil || len(v) > 0 {
		t.Fatalf("the fake serves schema-valid documents: %v %v", v, err)
	}
	return teamwire.PullRow{Seq: seq, Kind: teamwire.KindHandoff, Handoff: &teamwire.PulledHandoff{ID: rec.ID, ToMe: true,
		SenderUserID: handoffTeammate, SenderName: "Teammate", SenderDeviceID: "dev_t", RecipientUserID: handoffMe, RecipientName: "Me",
		State: state, CreatedAt: rec.CreatedAt, StateAt: rec.CreatedAt, WireHash: rec.ContentHash, Document: raw}}, rec
}

func stateOnly(seq int64, id, state string) teamwire.PullRow {
	return teamwire.PullRow{Seq: seq, Kind: teamwire.KindHandoff, Handoff: &teamwire.PulledHandoff{ID: id, ToMe: true,
		SenderUserID: handoffTeammate, SenderName: "Teammate", RecipientUserID: handoffMe, RecipientName: "Me",
		State: state, CreatedAt: "2026-10-04T16:00:00Z", StateAt: "2026-10-04T16:05:00Z"}}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err == nil && path != dir {
			out = append(out, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Criterion 62: the preview is exactly what leaves — the bytes pushed equal the bytes
// the preview described; a planted secret leaves as a marker, a path under the home
// directory as "[absolute path]", a path under the checkout repository-relative; the
// excerpt is present only when ticked; a send whose document no longer hashes to the
// preview is refused and writes nothing. Criterion 69: nothing is written into the
// session's folder.
func TestHandoffPreviewIsExactlyWhatLeaves(t *testing.T) {
	rig := newHandoffRig(t, true)
	req := rig.request("Teammate")
	req.IncludeConversation = true
	before, _ := rig.ix.OutboxPending()
	shown, sendReq := rig.preview(req)
	if n, _ := rig.ix.OutboxPending(); n != before {
		t.Fatalf("a preview writes nothing: %d outbox rows, %d before", n, before)
	}
	if rows, _ := rig.ix.ListHandoffs(""); len(rows) != 0 {
		t.Fatalf("a preview writes no row: %d", len(rows))
	}
	for _, want := range []string{"internal/x.go", absolutePathMarker, "[redacted:secret.aws-key]"} {
		if !strings.Contains(shown.BodyMarkdown, want) {
			t.Fatalf("the shown body lacks %q: %q", want, shown.BodyMarkdown)
		}
	}
	for _, leaked := range []string{rig.cwd, "/Users/alice", handoffSecret} {
		if strings.Contains(shown.BodyMarkdown, leaked) || strings.Contains(strings.Join(shown.Remaining, "\n"), leaked) {
			t.Fatalf("%q survived the checks", leaked)
		}
	}
	if shown.Remaining[1] != "Check store/y.go" {
		t.Fatalf("each remaining item is checked: %q", shown.Remaining)
	}
	if shown.Checks.RedactionCount < 2 || shown.Checks.Redactions["secret.aws-key"] < 2 || shown.Checks.AbsolutePaths != 1 {
		t.Fatalf("the checks are counted by marker name (body and excerpt): %+v", shown.Checks)
	}
	if shown.Conversation == nil || shown.Conversation.TurnCount != 2 || shown.Conversation.Bytes == 0 || len(shown.Conversation.Turns) != 2 {
		t.Fatalf("the ticked excerpt shows every turn: %+v", shown.Conversation)
	}
	if strings.Contains(shown.Conversation.Turns[0].Text, handoffSecret) || strings.Contains(shown.Conversation.Turns[1].Text, rig.cwd) {
		t.Fatalf("each excerpt turn is checked: %+v", shown.Conversation.Turns)
	}
	if shown.Recipient == nil || shown.Recipient.UserID != handoffTeammate || shown.Session.Runtime != "claude" || shown.Session.NativeID != handoffTestSession {
		t.Fatalf("recipient and session: %+v %+v", shown.Recipient, shown.Session)
	}
	if shown.RepositoryID != nil {
		t.Fatalf("a folder with no remote has no repository id: %v", *shown.RepositoryID)
	}

	// The same request with different text, under the previewed hash: refused.
	edited := sendReq
	edited.BodyMarkdown += " and one more line"
	if code, refusal := rig.call("POST", "/api/team/handoffs/send", edited, nil); code != http.StatusConflict || refusal.Code != handoffCodePreviewMismatch {
		t.Fatalf("a send that differs from its preview is refused: %d %+v", code, refusal)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/send", rig.request("Teammate"), nil); code != http.StatusBadRequest || refusal.Code != handoffCodePreviewRequired {
		t.Fatalf("a send with no preview is refused: %d %+v", code, refusal)
	}
	if n, _ := rig.ix.OutboxPending(); n != before {
		t.Fatalf("a refused send queues nothing: %d", n)
	}

	var sent teamHandoffSendResponse
	if code, refusal := rig.call("POST", "/api/team/handoffs/send", sendReq, &sent); code != http.StatusOK {
		t.Fatalf("send: %d %+v", code, refusal)
	}
	if sent.WireHash != shown.WireHash || sent.State != store.HandoffQueued || sent.Bytes != shown.Bytes {
		t.Fatalf("the send is the previewed document, queued: %+v", sent)
	}
	rig.drain()
	records := rig.fake.records(teamwire.KindHandoff)
	if len(records) != 1 {
		t.Fatalf("one handoff pushed: %d", len(records))
	}
	if len(records[0].Body) != shown.Bytes {
		t.Fatalf("the bytes sent are the bytes the preview counted: %d != %d", len(records[0].Body), shown.Bytes)
	}
	var pushed teamwire.HandoffRecord
	if err := json.Unmarshal(records[0].Body, &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.ContentHash != shown.WireHash || teamwire.HandoffWireHash(pushed) != shown.WireHash || pushed.Title != shown.Title ||
		pushed.BodyMarkdown != shown.BodyMarkdown || strings.Join(pushed.Remaining, "\n") != strings.Join(shown.Remaining, "\n") ||
		pushed.Conversation == nil || len(pushed.Conversation.Turns) != shown.Conversation.TurnCount || pushed.Recipient.UserID != handoffTeammate {
		t.Fatalf("the pushed document is the shown one: %+v", pushed)
	}
	h, found, _ := rig.ix.HandoffByID("org_1", sent.ID)
	if !found || h.WireBody != string(records[0].Body) || h.State != teamwire.HandoffSent {
		t.Fatalf("the stored body is the pushed body and the item reads sent: %+v", h)
	}
	if list := rig.list(); len(list.Sent) != 1 || len(list.Received) != 0 || list.Sent[0].Peer.DisplayName != "Teammate" || !list.Transport.ServerCarriesHandoffs {
		t.Fatalf("listed under Sent: %+v", list)
	}
	if entries := dirEntries(t, rig.cwd); len(entries) != 0 {
		t.Fatalf("a handoff writes nothing into the session's folder: %v", entries)
	}

	// Not ticked: no excerpt in the document at all.
	plain, _ := rig.preview(rig.request(handoffTeammate))
	if plain.Conversation != nil {
		t.Fatalf("the excerpt is present only when ticked: %+v", plain.Conversation)
	}
}

// Criterion 62's fail paths and criterion 79: an unknown or removed recipient is
// refused on the device as recipient_inactive with nothing written; the server's
// refusals are kept by name with the text; the directory route carries a user id and a
// display name and nothing else about a member.
func TestHandoffSendRefusalsKeepTheText(t *testing.T) {
	rig := newHandoffRig(t, true)
	req := rig.request("Nobody")
	req.Preview = true
	if code, refusal := rig.call("POST", "/api/team/handoffs/send", req, nil); code != http.StatusConflict || refusal.Code != teamwire.CodeRecipientInactive {
		t.Fatalf("a name the directory does not list: %d %+v", code, refusal)
	}
	rec := httptest.NewRecorder()
	rig.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/team/members", nil))
	var raw struct {
		Members []map[string]any `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Members) != 2 {
		t.Fatalf("directory: %s", rec.Body.String())
	}
	for _, member := range raw.Members {
		for key := range member {
			if key != "user_id" && key != "display_name" && key != "self" {
				t.Fatalf("the directory holds a user id and a display name only; found %q", key)
			}
		}
	}

	// A member removed on the server is absent at the next refresh, and a send to them
	// is refused here.
	shown, sendReq := rig.preview(rig.request("Teammate"))
	rig.fake.hmu.Lock()
	rig.fake.members = rig.fake.members[:1]
	rig.fake.hmu.Unlock()
	var members teamMembersResponse
	if code, _ := rig.call("GET", "/api/team/members?refresh=1", nil, &members); code != http.StatusOK || len(members.Members) != 1 || members.Members[0].UserID != handoffMe {
		t.Fatalf("a removed member is absent at the next refresh: %+v", members)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/send", sendReq, nil); code != http.StatusConflict || refusal.Code != teamwire.CodeRecipientInactive {
		t.Fatalf("a send to someone no longer listed: %d %+v", code, refusal)
	}
	if _, found, _ := rig.ix.HandoffByID("org_1", shown.ID); found {
		t.Fatal("a refused send writes no row")
	}
	rig.fake.hmu.Lock()
	rig.fake.members = append(rig.fake.members, teamwire.Member{UserID: handoffTeammate, DisplayName: "Teammate"})
	rig.fake.hmu.Unlock()
	rig.tl.refreshMembersNow()

	for _, code := range []string{teamwire.CodeUnknownRecipient, teamwire.CodeRecipientInactive, teamwire.CodeRecipientInboxFull, teamwire.CodeRateLimited} {
		rig.fake.script(map[string]string{teamwire.KindHandoff: code}, false)
		sent := rig.send(rig.request("Teammate"))
		rig.drain()
		var detail teamHandoffDetailResponse
		if status, refusal := rig.call("GET", "/api/team/handoffs/"+sent.ID, nil, &detail); status != http.StatusOK {
			t.Fatalf("detail: %d %+v", status, refusal)
		}
		if detail.Handoff.State != store.HandoffRefused || detail.Handoff.RefusalCode != code {
			t.Fatalf("%s is refused by name: %+v", code, detail.Handoff)
		}
		if detail.Document == nil || detail.Document.BodyMarkdown != sent.BodyMarkdown || detail.Handoff.Title != sent.Title {
			t.Fatalf("%s: the text is kept so it can go out as a new handoff: %+v", code, detail)
		}
		if len(detail.Handoff.Offers) != 0 {
			t.Fatalf("%s: a refused handoff offers nothing: %v", code, detail.Handoff.Offers)
		}
	}
}

// Criterion 62's last fail path: an older server parks the two kinds (the handoff stays
// queued and nothing is lost) and the handoff pull, which is retried on
// parked_kind_retry; the memory pull, a different request, is untouched.
func TestOlderServerParksTheHandoffKindsAndTheHandoffPullOnly(t *testing.T) {
	// The clock is installed before the link starts the linker's jobs: they read it
	// without the linker's lock, so it is never reassigned under them.
	rig := newHandoffRig(t, false)
	clock := time.Now()
	var clockMu sync.Mutex
	rig.tl.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	linkTo(t, rig.tl, rig.fake)
	if !rig.tl.refreshMembersNow() {
		t.Fatal("the directory refresh needs a link")
	}
	rig.tl.mu.Lock()
	retry := rig.tl.doc.ParkedKindRetry.Duration
	rig.tl.mu.Unlock()
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }

	rig.fake.hmu.Lock()
	rig.fake.older = true
	rig.fake.hmu.Unlock()
	rig.fake.script(map[string]string{teamwire.KindHandoff: teamwire.CodeUnsupportedKind, teamwire.KindHandoffReceipt: teamwire.CodeUnsupportedKind}, false)
	sent := rig.send(rig.request("Teammate"))
	rig.drain()
	h, _, _ := rig.ix.HandoffByID("org_1", sent.ID)
	if h.State != store.HandoffQueued {
		t.Fatalf("an unsupported kind is parked, not refused: the item stays queued: %+v", h)
	}
	if n, _ := rig.ix.OutboxPending(); n != 1 {
		t.Fatalf("the row is not acknowledged: %d pending", n)
	}

	rig.tl.pullHandoffsOnce()
	rig.tl.pullHandoffsOnce()
	if n := rig.fake.handoffPulls.Load(); n != 1 {
		t.Fatalf("a parked handoff pull is not asked again before parked_kind_retry: %d requests", n)
	}
	if list := rig.list(); list.Transport.ServerCarriesHandoffs || list.Transport.PullOutcome != handoffSyncParked || len(list.Sent) != 1 || list.Sent[0].State != store.HandoffQueued {
		t.Fatalf("this team server does not carry handoffs, as data: %+v", list)
	}
	before := rig.fake.memoryPulls.Load()
	rig.tl.pullMemoryOnce()
	if rig.fake.memoryPulls.Load() != before+1 {
		t.Fatal("the memory pull is a different request and is untouched")
	}
	rig.tl.mu.Lock()
	outcome := rig.tl.report.Pull.Outcome
	rig.tl.mu.Unlock()
	if outcome != "pulled" {
		t.Fatalf("the memory pull succeeded: %q", outcome)
	}

	// The server is upgraded: the retry finds it, and the queued handoff leaves.
	advance(retry + time.Second)
	rig.fake.hmu.Lock()
	rig.fake.older = false
	rig.fake.hmu.Unlock()
	rig.fake.script(map[string]string{}, false)
	rig.tl.pullHandoffsOnce()
	if n := rig.fake.handoffPulls.Load(); n != 2 {
		t.Fatalf("the parked pull is retried after parked_kind_retry: %d", n)
	}
	rig.drain()
	if h, _, _ := rig.ix.HandoffByID("org_1", sent.ID); h.State != teamwire.HandoffSent {
		t.Fatalf("the parked handoff leaves when the server carries the kind: %+v", h)
	}
	if list := rig.list(); !list.Transport.ServerCarriesHandoffs {
		t.Fatalf("un-parked: %+v", list.Transport)
	}
}

// The pull is its own request and cursor, bounded by handoff.pull_pages; a landed
// document is listed under Received and answered with a received receipt. Criterion
// 67, device half: landing a withdrawal erases the unopened copy and the device says
// the sender withdrew. Criterion 78: an expiry this device never held shows the sender
// and the time only, and offers nothing.
func TestHandoffPullLandsListsAndAnswersReceived(t *testing.T) {
	rig := newHandoffRig(t, true)
	rig.tl.mu.Lock()
	rig.tl.doc.Handoff.PullPages = 2
	rig.tl.mu.Unlock()
	first, firstDoc := pulledDocument(t, 1, "First handoff", "first body", teamwire.HandoffSent)
	second, _ := pulledDocument(t, 2, "Second handoff", "second body", teamwire.HandoffSent)
	third, _ := pulledDocument(t, 3, "Third handoff", "third body", teamwire.HandoffSent)
	rig.fake.onePerPage = true
	rig.fake.serve(first, second, third)

	rig.tl.pullHandoffsOnce()
	if n := rig.fake.handoffPulls.Load(); n != 2 {
		t.Fatalf("one tick asks for handoff.pull_pages pages: %d", n)
	}
	if c, _ := rig.ix.SyncCursor(store.HandoffCursorScope("org_1")); c != 2 {
		t.Fatalf("its own cursor, scope handoff:<organization>: %d", c)
	}
	if c, _ := rig.ix.SyncCursor(memoryPullCursor("org_1")); c != 0 {
		t.Fatalf("the memory cursor is another row: %d", c)
	}
	if n := rig.fake.bootstraps.Load(); n != 2 {
		t.Fatalf("every page of the link's first handoff pull carries bootstrap: %d", n)
	}
	rig.tl.pullHandoffsOnce()
	if n, total := rig.fake.bootstraps.Load(), rig.fake.handoffPulls.Load(); n != total {
		t.Fatalf("until a page reports no more: %d of %d", n, total)
	}
	rig.tl.pullHandoffsOnce()
	if n, total := rig.fake.bootstraps.Load(), rig.fake.handoffPulls.Load(); n != total-1 {
		t.Fatalf("once drained, later pulls do not carry it: %d of %d", n, total)
	}
	list := rig.list()
	if len(list.Received) != 3 || len(list.Sent) != 0 {
		t.Fatalf("received: %+v", list)
	}
	rig.drain()
	receipts := rig.fake.records(teamwire.KindHandoffReceipt)
	if len(receipts) != 3 {
		t.Fatalf("each landed document is answered with one received receipt: %d", len(receipts))
	}
	deviceID, _, _ := rig.ix.Device()
	var receipt teamwire.HandoffReceipt
	_ = json.Unmarshal(receipts[0].Body, &receipt)
	if receipt.Transition != teamwire.TransitionReceived || receipts[0].ID != teamwire.ReceiptID(receipt.HandoffID, teamwire.TransitionReceived, deviceID, "", "") {
		t.Fatalf("the receipt id is deterministic: %+v", receipt)
	}

	var detail teamHandoffDetailResponse
	rig.call("GET", "/api/team/handoffs/"+firstDoc.ID, nil, &detail)
	if detail.Document == nil || detail.Document.BodyMarkdown != "first body" || detail.Handoff.Peer.DisplayName != "Teammate" ||
		detail.Handoff.Session == nil || detail.Handoff.Session.CatalogID != "catalog-9" || detail.Opens == nil {
		t.Fatalf("the recipient's device reads the document: %+v", detail)
	}
	if len(detail.Handoff.Offers) != 1 || detail.Handoff.Offers[0] != teamwire.TransitionDeclined {
		t.Fatalf("decline is offered while received: %v", detail.Handoff.Offers)
	}

	// The sender withdraws; a handoff this device never held expires.
	never := engine.NewTypedID(teamwire.HandoffIDPrefix)
	rig.fake.onePerPage = false
	rig.fake.serve(stateOnly(4, firstDoc.ID, teamwire.HandoffWithdrawn), stateOnly(5, never, teamwire.HandoffExpired))
	rig.tl.pullHandoffsOnce()
	detail = teamHandoffDetailResponse{}
	rig.call("GET", "/api/team/handoffs/"+firstDoc.ID, nil, &detail)
	if detail.Document != nil || detail.Handoff.Title != "" || detail.Handoff.HasText || !detail.Handoff.EverHeld || detail.Handoff.State != teamwire.HandoffWithdrawn {
		t.Fatalf("landing a withdrawal erases the recipient's unopened copy: %+v", detail)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+firstDoc.ID+"/decline", nil, nil); code != http.StatusConflict ||
		refusal.Code != teamwire.CodeHandoffWithdrawn || !strings.Contains(refusal.Error, "the sender withdrew") {
		t.Fatalf("the device says the sender withdrew: %d %+v", code, refusal)
	}
	var expired teamHandoffDetailResponse
	rig.call("GET", "/api/team/handoffs/"+never, nil, &expired)
	if expired.Document != nil || expired.Handoff.Title != "" || expired.Handoff.HasText || expired.Handoff.EverHeld ||
		expired.Handoff.State != teamwire.HandoffExpired || expired.Handoff.Peer.DisplayName != "Teammate" || expired.Handoff.CreatedAt == "" || len(expired.Handoff.Offers) != 0 {
		t.Fatalf("expired before it reached this device: the sender and the time, nothing offered: %+v", expired)
	}

	// Decline the second; the receipt leaves at the head of the next drain.
	var declined teamHandoffDetailResponse
	secondID := second.Handoff.ID
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+secondID+"/decline", nil, &declined); code != http.StatusOK || declined.Handoff.State != teamwire.HandoffDeclined {
		t.Fatalf("decline: %d %+v %+v", code, refusal, declined)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+secondID+"/close", nil, nil); code != http.StatusConflict || refusal.Code != teamwire.CodeHandoffDeclined {
		t.Fatalf("a declined handoff offers nothing more: %d %+v", code, refusal)
	}
	rig.drain()
	if n := len(rig.fake.records(teamwire.KindHandoffReceipt)); n != 4 {
		t.Fatalf("the declined receipt was pushed: %d receipts", n)
	}
	if code, _ := rig.call("GET", "/api/team/handoffs/hnd_missing", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}
}

// Criterion 66, device half: the sender's other device lists the handoff under Sent
// with no text and can withdraw it; a self-send is listed once on the originating
// device, under Sent.
func TestSenderDevicesListAndWithdraw(t *testing.T) {
	rig := newHandoffRig(t, true)
	other := engine.NewTypedID(teamwire.HandoffIDPrefix)
	rig.fake.serve(teamwire.PullRow{Seq: 1, Kind: teamwire.KindHandoff, Handoff: &teamwire.PulledHandoff{ID: other, FromMe: true,
		SenderUserID: handoffMe, SenderName: "Me", RecipientUserID: handoffTeammate, RecipientName: "Teammate",
		State: teamwire.HandoffReceived, CreatedAt: "2026-10-04T16:00:00Z", StateAt: "2026-10-04T16:00:01Z"}})
	rig.tl.pullHandoffsOnce()
	list := rig.list()
	if len(list.Sent) != 1 || !list.Sent[0].FromAnotherDevice || list.Sent[0].Title != "" || list.Sent[0].HasText || list.Sent[0].Peer.DisplayName != "Teammate" {
		t.Fatalf("the sender's second device holds no text and lists it under Sent: %+v", list)
	}
	var withdrawn teamHandoffDetailResponse
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+other+"/withdraw", nil, &withdrawn); code != http.StatusOK || withdrawn.Handoff.State != teamwire.HandoffWithdrawn {
		t.Fatalf("it can withdraw: %d %+v", code, refusal)
	}
	rig.drain()
	receipts := rig.fake.records(teamwire.KindHandoffReceipt)
	if len(receipts) != 1 || !strings.Contains(string(receipts[0].Body), `"transition":"withdrawn"`) {
		t.Fatalf("a withdrawn receipt and no received: %d", len(receipts))
	}

	self := rig.send(rig.request("Me"))
	list = rig.list()
	count := 0
	for _, item := range append(list.Sent, list.Received...) {
		if item.ID == self.ID {
			count++
		}
	}
	if count != 1 || len(list.Received) != 0 {
		t.Fatalf("a self-send is listed once on the originating device, under Sent: %+v", list)
	}
}

// Criterion 68: a local handoff works on a device that is not linked — the handoff
// bounds still load — with no server, no outbox row and no receipt; after an unlink
// what was received stays readable and nothing is sent. Criterion 69: nothing is
// written into the session's folder.
func TestLocalHandoffUnlinkedAndRowsAfterUnlink(t *testing.T) {
	rig := newHandoffRig(t, false)
	config, _ := consoleConfig()
	if config.Handoff.ConversationTurns != 20 || config.Handoff.InjectMaxBytes != 4900 || config.Handoff.BriefWait.Duration != 168*time.Hour {
		t.Fatalf("the handoff defaults load unlinked: %+v", config.Handoff)
	}
	req := rig.request("")
	req.Local = true
	sent := rig.send(req)
	if sent.State != teamwire.HandoffReceived || !sent.Local || sent.Recipient != nil {
		t.Fatalf("a local handoff: %+v", sent)
	}
	if n, _ := rig.ix.OutboxPending(); n != 0 {
		t.Fatalf("no outbox row for a local handoff: %d", n)
	}
	list := rig.list()
	if list.Linked || len(list.Received) != 1 || !list.Received[0].Local || len(list.Sent) != 0 {
		t.Fatalf("listed, local: %+v", list)
	}
	if entries := dirEntries(t, rig.cwd); len(entries) != 0 {
		t.Fatalf("nothing is written into the session's folder: %v", entries)
	}
	team := rig.request("Teammate")
	team.Preview = true
	if code, refusal := rig.call("POST", "/api/team/handoffs/send", team, nil); code != http.StatusConflict || refusal.Code != handoffCodeNotLinked {
		t.Fatalf("a team send needs a link: %d %+v", code, refusal)
	}

	// Link, receive one, unlink: it stays readable; nothing is sent; nothing is offered.
	linkTo(t, rig.tl, rig.fake)
	row, doc := pulledDocument(t, 1, "Kept after unlink", "kept body", teamwire.HandoffSent)
	rig.fake.serve(row)
	rig.tl.pullHandoffsOnce()
	rig.drain()
	pushedBefore := len(rig.fake.records(teamwire.KindHandoffReceipt))
	if _, err := rig.tl.unlink(); err != nil {
		t.Fatal(err)
	}
	var detail teamHandoffDetailResponse
	if code, _ := rig.call("GET", "/api/team/handoffs/"+doc.ID, nil, &detail); code != http.StatusOK || detail.Document == nil ||
		detail.Document.BodyMarkdown != "kept body" || !detail.Handoff.LinkEnded || len(detail.Handoff.Offers) != 0 {
		t.Fatalf("after unlink a received handoff stays readable: %+v", detail)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+doc.ID+"/decline", nil, nil); code != http.StatusConflict || refusal.Code != store.HandoffCodeLinkEnded {
		t.Fatalf("nothing is sent for it: %d %+v", code, refusal)
	}
	rig.tl.pushOnce()
	if n, _ := rig.ix.OutboxPending(); n != 0 || len(rig.fake.records(teamwire.KindHandoffReceipt)) != pushedBefore {
		t.Fatalf("after unlink nothing is queued and nothing is sent: %d pending", n)
	}
	var members teamMembersResponse
	rig.call("GET", "/api/team/members", nil, &members)
	if members.Linked || len(members.Members) != 0 {
		t.Fatalf("the directory ends with the link: %+v", members)
	}
}

// A handoff job that is scheduled only after its link was ended takes nothing back.
// The job's first act reclaims an earlier link's rows for a relink to the same
// organization; unlink marks the rows as an ended link's. Both run under the linker's
// lock and the job checks its stop first, so a late job cannot undo the unlink — which
// would offer Decline and Close on handoffs of a link the device no longer has.
func TestALateHandoffJobDoesNotTakeBackAnEndedLinksRows(t *testing.T) {
	rig := newHandoffRig(t, true)
	row, doc := pulledDocument(t, 1, "Ended with the link", "body", teamwire.HandoffSent)
	rig.fake.serve(row)
	rig.tl.pullHandoffsOnce()
	rig.tl.mu.Lock()
	stop, organization := rig.tl.pushStop, rig.tl.doc.Organization.ID
	rig.tl.mu.Unlock()
	if stop == nil || organization == "" {
		t.Fatalf("the link's handoff job is running for an organization: %v %q", stop != nil, organization)
	}
	if _, _, running := rig.tl.reclaimHandoffsAtStart(stop); !running {
		t.Fatal("a job whose link is live starts")
	}
	if _, err := rig.tl.unlink(); err != nil {
		t.Fatal(err)
	}
	ended, found, err := rig.ix.HandoffByID(organization, doc.ID)
	if err != nil || !found || !ended.LinkEnded {
		t.Fatalf("the unlink ends the link's rows: %+v %v %v", ended, found, err)
	}
	// The job as it runs when the scheduler reaches it only now.
	if _, _, running := rig.tl.reclaimHandoffsAtStart(stop); running {
		t.Fatal("a job whose link was ended before it ran must not start")
	}
	if after, _, _ := rig.ix.HandoffByID(organization, doc.ID); !after.LinkEnded {
		t.Fatalf("the late job took the ended link's rows back: %+v", after)
	}
}

// lockedBuffer collects log output from the test and the linker's jobs at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// Criterion 65, device half: every chained device event and every log line about a
// handoff carries ids and codes — never a title, a body, a remaining item or an excerpt.
func TestHandoffEventsAndLogsCarryIDsNotText(t *testing.T) {
	rig := newHandoffRig(t, true)
	logs := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	req := rig.request("Teammate")
	req.Title, req.BodyMarkdown = "Title "+handoffPlanted, "Body "+handoffPlanted
	req.Remaining = []string{"Remaining " + handoffPlanted}
	req.IncludeConversation = true
	accepted := rig.send(req)
	rig.drain()
	rig.fake.script(map[string]string{teamwire.KindHandoff: teamwire.CodeRateLimited}, false)
	refused := rig.send(req)
	rig.drain()
	rig.fake.script(map[string]string{teamwire.KindHandoffReceipt: teamwire.CodeHandoffWithdrawn}, false)
	row, doc := pulledDocument(t, 1, "Pulled "+handoffPlanted, "pulled body "+handoffPlanted, teamwire.HandoffSent)
	// The same id under another wire hash: a forged or corrupted row.
	forgedDoc := doc
	forgedDoc.Title, forgedDoc.BodyMarkdown = "Forged "+handoffPlanted, "forged body "+handoffPlanted
	forgedDoc.ContentHash = teamwire.HandoffWireHash(forgedDoc)
	forgedBody, _ := json.Marshal(forgedDoc)
	forged := stateOnly(2, doc.ID, teamwire.HandoffSent)
	forged.Handoff.WireHash, forged.Handoff.Document = forgedDoc.ContentHash, forgedBody
	rig.fake.serve(row, forged)
	rig.tl.pullHandoffsOnce()
	rig.drain()
	rig.call("POST", "/api/team/handoffs/"+accepted.ID+"/withdraw", nil, nil)
	rig.fake.script(map[string]string{}, false)
	rig.drain()

	if h, _, _ := rig.ix.HandoffByID("org_1", accepted.ID); !strings.Contains(h.WireBody, handoffPlanted) || !strings.Contains(h.Title, handoffPlanted) {
		t.Fatalf("the text lives in the handoff's own columns: %+v", h)
	}
	if h, _, _ := rig.ix.HandoffByID("org_1", refused.ID); h.State != store.HandoffRefused {
		t.Fatalf("the second send was refused: %+v", h)
	}
	if h, _, _ := rig.ix.HandoffByID("org_1", doc.ID); h.HashConflicts != 1 || h.ReceiptCode != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("the conflict was counted and the rejected receipt landed: %+v", h)
	}
	rows, err := rig.ix.EventChainRows(teamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, r := range rows {
		kinds[r.Body.Tool] = true
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), handoffPlanted) || strings.Contains(string(raw), "Finish the outbox drain") {
			t.Fatalf("a chained event carries handoff text: %s", raw)
		}
	}
	for _, want := range []string{"team.handoff.send", "team.handoff.refused", "team.handoff.hash_conflict", "team.handoff.receipt_rejected", "team.handoff.withdraw"} {
		if !kinds[want] {
			t.Fatalf("no %s event was chained (have %v)", want, kinds)
		}
	}
	if !strings.Contains(logs.String(), accepted.ID) && !strings.Contains(logs.String(), refused.ID) {
		t.Fatalf("the log names handoffs by id: %s", logs.String())
	}
	if strings.Contains(logs.String(), handoffPlanted) {
		t.Fatalf("a log line carries handoff text: %s", logs.String())
	}
}

// The extract emits repository-relative paths and offers the last agent message's list
// as the remaining prefill; nothing asks the session (OD-12).
func TestHandoffExtractIsRelativeAndPrefillsRemaining(t *testing.T) {
	rig := newHandoffRig(t, false)
	session, err := LoadSession("claude", rig.session)
	if err != nil {
		t.Fatal(err)
	}
	draft := generateHandoff(session, handoffCheckout(session).root)
	if !strings.Contains(draft.Markdown, "`internal/x.go`") || strings.Contains(draft.Markdown, "`"+rig.cwd) {
		t.Fatalf("files touched are repository-relative: %s", draft.Markdown)
	}
	if strings.Contains(draft.Markdown, "EDIT ME") {
		t.Fatalf("no placeholder section: %s", draft.Markdown)
	}
	if len(draft.Remaining) != 2 || draft.Remaining[0] != "Drain handoff kinds first" || draft.Remaining[1] != "Add the parked-kind retry" {
		t.Fatalf("the prefill is the last agent message's list: %q", draft.Remaining)
	}
	if draft.Title == "" || draft.SessionRef != "claude/"+rig.session {
		t.Fatalf("draft: %+v", draft)
	}
}

// The excerpt is bounded by handoff.conversation_turns and conversation_max_bytes, and
// says when the byte bound cut it.
func TestHandoffExcerptIsBoundedByTurnsAndBytes(t *testing.T) {
	session := &SessionDetail{}
	for i := 1; i <= 6; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		session.Events = append(session.Events, CanonicalEvent{Seq: i, Kind: role, Text: strings.Repeat("é", 50)})
	}
	session.Events = append(session.Events, CanonicalEvent{Seq: 7, Kind: "tool_call", Text: "ignored"})
	checker := &handoffChecker{checks: handoffChecks{Redactions: map[string]int{}}}
	byTurns := handoffExcerpt(session, 4, 65536, checker)
	if len(byTurns.Turns) != 4 || byTurns.Turns[0].Seq != 3 || byTurns.Truncated {
		t.Fatalf("the last N turns: %+v", byTurns)
	}
	byBytes := handoffExcerpt(session, 4, 250, checker)
	if len(byBytes.Turns) != 2 || !byBytes.Truncated || byBytes.Turns[1].Seq != 6 {
		t.Fatalf("older turns are dropped to fit the byte bound: %+v", byBytes)
	}
	one := handoffExcerpt(session, 1, 51, checker)
	if len(one.Turns) != 1 || len(one.Turns[0].Text) != 50 || !one.Truncated {
		t.Fatalf("a single oversized turn is cut at a character boundary: %d bytes", len(one.Turns[0].Text))
	}
}
