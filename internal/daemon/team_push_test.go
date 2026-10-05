package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/schemas"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// pushFake is a team server whose per-kind answer the test scripts. It validates every
// body against its wire schema, as the real server does, so a record the device builds
// wrong is caught here and not in a journey.
type pushFake struct {
	teamFake
	mu          sync.Mutex
	answer      map[string]string // kind → status or error code ("unsupported_kind", "invalid_record")
	fail        bool              // answer the whole push with a 500
	tooLarge    bool              // answer the whole push with a 413
	received    []teamwire.PushRecord
	invalid     []string
	pushEntered chan struct{} // optional block for lifecycle cleanup test
	pushRelease chan struct{}
	// memory answers a memory record with what the real server adds: its server
	// revision, or on stale_base the revision it holds. nil = no memory answer.
	memory func(rec teamwire.PushRecord, status, code string) *teamwire.MemoryAnswer
}

func (f *pushFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != teamwire.RoutePush {
		f.teamFake.ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in teamwire.PushRequest
	_ = json.Unmarshal(body, &in)
	if len(in.Records) > 0 && in.Records[0].Kind == teamwire.KindDeviceReport {
		f.teamFake.ServeHTTP(w, httptest.NewRequest("POST", teamwire.RoutePush, strings.NewReader(string(body))))
		return
	}
	if f.pushEntered != nil {
		select {
		case f.pushEntered <- struct{}{}:
		default:
		}
		<-f.pushRelease
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		w.WriteHeader(500)
		return
	}
	if f.tooLarge {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	out := teamwire.PushResponse{ServerTime: time.Now().Unix()}
	for _, rec := range in.Records {
		f.received = append(f.received, rec)
		if v, err := schemas.Violations(teamwire.SchemaForKind(rec.Kind), rec.Body); err != nil || len(v) > 0 {
			f.invalid = append(f.invalid, rec.Kind+": "+string(rec.Body))
		}
		if teamwire.ContentHash(rec.Body) != rec.ContentHash {
			f.invalid = append(f.invalid, rec.Kind+": content hash")
		}
		res := teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusAccepted}
		switch a := f.answer[rec.Kind]; a {
		case "", teamwire.StatusAccepted:
		case teamwire.StatusDuplicate, teamwire.StatusConflict:
			res.Status = a
		default:
			res.Status, res.Error = teamwire.StatusRejected, &teamwire.Error{Code: a, Message: "scripted"}
		}
		if f.memory != nil && rec.Kind == teamwire.KindMemory {
			res.Memory = f.memory(rec, res.Status, f.answer[rec.Kind])
		}
		out.Results = append(out.Results, res)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *pushFake) script(answer map[string]string, fail bool) {
	f.mu.Lock()
	f.answer, f.fail, f.received = answer, fail, nil
	f.mu.Unlock()
}

func (f *pushFake) got(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.received {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

// linkForPush links a test linker through the fake (the real enrollment flow), with a
// cadence long enough that only the test's own ticks drain.
func linkForPush(t *testing.T) (*teamLinker, *store.Index, *pushFake, *time.Time) {
	t.Helper()
	tl, ix, _ := teamTestLinker(t)
	clock := time.Now()
	var clockMu sync.Mutex
	tl.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	fake := &pushFake{answer: map[string]string{}}
	fake.status.Store("approved")
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	tl.doc.PushInterval.Duration = time.Hour
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for tl.status().State != teamLinked && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if st := tl.status(); st.State != teamLinked {
		t.Fatalf("not linked: %+v", st)
	}
	tl.mu.Lock()
	tl.doc.PushInterval.Duration = time.Hour // the running job waits; the test ticks
	tl.mu.Unlock()
	return tl, ix, fake, &clock
}

func TestTeamLinkStopJobsWaitsForActivePush(t *testing.T) {
	tl, _, _ := teamTestLinker(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	fake := &pushFake{answer: map[string]string{}, pushEntered: entered, pushRelease: release}
	fake.status.Store("approved")
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	tl.doc.PushInterval.Duration = time.Millisecond
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("push worker did not enter the fake server")
	}
	done := make(chan struct{})
	go func() { tl.stopJobs(); close(done) }()
	select {
	case <-done:
		t.Fatal("stopJobs returned while its push worker was active")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopJobs did not wait for the released push worker")
	}
}

func pending(t *testing.T, ix *store.Index) store.OutboxSummary {
	t.Helper()
	s, err := ix.OutboxPendingSummary()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Criterion 31 (item 4): the drain pushes events and their session projection, acks what
// the server answered terminally, parks what it answered unsupported, and never wedges.
func TestOutboxDrainAcksParksAndNeverWedges(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	tl.pushMu.Lock() // hold the job's first tick until the script is set
	tl.linkEvent("team.test", "one")
	tl.linkEvent("team.test", "two")
	tl.pushMu.Unlock()
	before := pending(t, ix)
	if before.ByKind[store.OutboxEvent] < 3 { // team.link + the two test events
		t.Fatalf("the link events enqueue while linked: %+v", before)
	}

	// An older server: events refused as unsupported. Nothing is acknowledged; the kind
	// parks and the next tick sends nothing (the parked kind never occupies the head).
	fake.script(map[string]string{teamwire.KindEvent: teamwire.CodeUnsupportedKind, teamwire.KindSession: teamwire.CodeUnsupportedKind}, false)
	tl.pushOnce()
	if got := pending(t, ix); got.Pending != before.Pending {
		t.Fatalf("an unsupported kind must not be acknowledged: before %+v after %+v", before, got)
	}
	st := tl.status()
	if st.Outbox == nil || len(st.Outbox.Parked) == 0 || st.Outbox.Parked[0].Kind != teamwire.KindEvent || st.Outbox.Parked[0].NextProbeAt == "" {
		t.Fatalf("the parked kind is named with its probe time: %+v", st.Outbox)
	}
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	if fake.got(teamwire.KindEvent) != 0 {
		t.Fatal("a parked kind must be left out of the batch until its probe is due")
	}

	// The probe: one row once the retry interval passes; accepted, it un-parks the kind,
	// and the next tick drains the rest.
	tl.mu.Lock()
	for k := range tl.parked {
		tl.parked[k] = time.Time{}
	}
	tl.mu.Unlock()
	tl.pushOnce()
	if fake.got(teamwire.KindEvent) == 0 {
		t.Fatal("the probe must push the parked kind's oldest row")
	}
	tl.pushOnce()
	if got := pending(t, ix); got.ByKind[store.OutboxEvent] != 0 {
		t.Fatalf("after the un-park every event drains: %+v", got)
	}
	if fake.got(teamwire.KindSession) == 0 {
		t.Fatal("the batch carries a session projection for the sessions it touched")
	}
	fake.mu.Lock()
	invalid := append([]string(nil), fake.invalid...)
	fake.mu.Unlock()
	if len(invalid) > 0 {
		t.Fatalf("every pushed body must satisfy its wire schema: %v", invalid)
	}

	// Refused on its merits, and a conflict: both acknowledged terminally and counted.
	tl.linkEvent("team.test", "three")
	fake.script(map[string]string{teamwire.KindEvent: teamwire.CodeInvalidRecord}, false)
	tl.pushOnce()
	tl.linkEvent("team.test", "four")
	fake.script(map[string]string{teamwire.KindEvent: teamwire.StatusConflict}, false)
	tl.pushOnce()
	if got := pending(t, ix); got.ByKind[store.OutboxEvent] != 0 {
		t.Fatalf("rejected and conflict are terminal: %+v", got)
	}
	st = tl.status()
	if st.Outbox.DeadLetter[teamwire.KindEvent][teamwire.CodeInvalidRecord] != 1 || st.Outbox.Conflicts[teamwire.KindEvent] != 1 {
		t.Fatalf("terminal refusals are counted per kind and code: %+v", st.Outbox)
	}

	// The server failing to store a row is transient: pending, retried, and dead-lettered
	// only once push_max_attempts is spent.
	tl.linkEvent("team.test", "internal")
	tl.mu.Lock()
	tl.doc.PushMaxAttempts = 2
	tl.mu.Unlock()
	fake.script(map[string]string{teamwire.KindEvent: teamwire.CodeInternal}, false)
	tl.pushOnce()
	if got := pending(t, ix); got.ByKind[store.OutboxEvent] != 1 {
		t.Fatalf("an internal failure leaves the row pending: %+v", got)
	}
	tl.pushOnce()
	if got := pending(t, ix); got.ByKind[store.OutboxEvent] != 0 || tl.status().Outbox.DeadLetter[teamwire.KindEvent]["internal_exhausted"] != 1 {
		t.Fatalf("after the cap the row is dead-lettered by name: %+v %+v", got, tl.status().Outbox)
	}

	// A transport failure acknowledges nothing.
	tl.linkEvent("team.test", "five")
	fake.script(nil, true)
	tl.pushOnce()
	if got := pending(t, ix); got.ByKind[store.OutboxEvent] != 1 {
		t.Fatalf("a failed push leaves its rows pending: %+v", got)
	}
	if st := tl.status(); st.Outbox.Outcome != "error" || st.Outbox.OldestAt == "" {
		t.Fatalf("the failure and the backlog's age are shown: %+v", st.Outbox)
	}
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	if got := pending(t, ix); got.Pending != 0 {
		t.Fatalf("recovered: %+v", got)
	}
}

// The session projection carries the device's chain report as a claim, with identities
// that agree with the events' own session object.
func TestSessionProjectionAgreesWithItsEvents(t *testing.T) {
	tl, _, fake, _ := linkForPush(t)
	tl.linkEvent("team.test", "one")
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var sessionID string
	var rec teamwire.SessionRecord
	for _, r := range fake.received {
		switch r.Kind {
		case teamwire.KindEvent:
			var ev struct {
				Session struct {
					ID string `json:"id"`
				} `json:"session"`
			}
			_ = json.Unmarshal(r.Body, &ev)
			sessionID = ev.Session.ID
		case teamwire.KindSession:
			_ = json.Unmarshal(r.Body, &rec)
		}
	}
	if sessionID == "" || rec.ID != sessionID || rec.Session.ID != sessionID {
		t.Fatalf("session projection id %q vs event session %q", rec.ID, sessionID)
	}
	if rec.ChainClaim.Status != "verified" || rec.Events.Count == 0 || rec.ChainClaim.HeldSpan == nil {
		t.Fatalf("the device's chain claim: %+v", rec)
	}
}

// appendWithInput writes one governed event with a captured tool input, the way observe
// does (one transaction), for the content path.
func appendWithInput(t *testing.T, ix *store.Index, session, input string) {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	id, err := tx.AppendEvent(store.EventRecord{TS: time.Now().Unix(), SessionID: session, Runtime: "claude", Verb: "exec", Tool: "Bash", Decision: "allow", Origin: "live", Tags: "[]"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventInput(store.EventInput{EventID: id, MediaType: "application/json", RawBytes: len(input), CapturedBytes: len(input),
		Digest: "sha256:x", Completeness: "complete", Payload: []byte(input)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func postContent(t *testing.T, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleTeamContent(rec, httptest.NewRequest("POST", "/api/team/content", strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

// Criterion 34 (item 4, the device half): content is off by default; an opted-in session
// sends its captured inputs, redacted, schema-valid; opting out stops what has not been
// sent and names the retention; a chunk past the session cap is refused by name.
func TestContentOptInSendsRedactedInputsForwardOnly(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	fake.script(map[string]string{}, false)
	appendWithInput(t, ix, "s-content", `{"command":"echo before opt-in"}`)
	tl.pushOnce()
	if fake.got(teamwire.KindSessionContent) != 0 {
		t.Fatal("content is off by default: nothing before an opt-in")
	}
	if code, body := postContent(t, `{"runtime":"claude","session":"s-content","enabled":true}`); code != 200 || !strings.Contains(body, `"claude/s-content"`) {
		t.Fatalf("opt in: %d %s", code, body)
	}
	appendWithInput(t, ix, "s-content", `{"command":"curl -H 'Authorization: Bearer abcdefghijklmnop' x"}`)
	appendWithInput(t, ix, "s-other", `{"command":"not opted in"}`)
	tl.pushOnce()
	fake.mu.Lock()
	var chunks []teamwire.ContentChunk
	for _, r := range fake.received {
		if r.Kind == teamwire.KindSessionContent {
			var c teamwire.ContentChunk
			_ = json.Unmarshal(r.Body, &c)
			chunks = append(chunks, c)
		}
	}
	invalid := append([]string(nil), fake.invalid...)
	fake.mu.Unlock()
	if len(invalid) > 0 {
		t.Fatalf("schema: %v", invalid)
	}
	if len(chunks) != 1 || chunks[0].Consent != "consent" || strings.Contains(chunks[0].Body, "abcdefghijklmnop") || len(chunks[0].Redactions) == 0 {
		t.Fatalf("one redacted chunk for the opted-in session only (forward-only): %+v", chunks)
	}

	// Opt out with a chunk still queued: it is refused locally by name, never sent.
	fake.script(map[string]string{teamwire.KindSessionContent: teamwire.CodeUnsupportedKind}, false)
	appendWithInput(t, ix, "s-content", `{"command":"queued"}`)
	tl.pushOnce() // parks content; the chunk stays queued
	if code, body := postContent(t, `{"runtime":"claude","session":"s-content","enabled":false}`); code != 200 || !strings.Contains(body, "until you delete what was sent") {
		t.Fatalf("opt out names the retention: %d %s", code, body)
	}
	tl.mu.Lock()
	for k := range tl.parked {
		tl.parked[k] = time.Time{}
	}
	tl.mu.Unlock()
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	if fake.got(teamwire.KindSessionContent) != 0 {
		t.Fatal("a revoked consent must stop the queued chunk")
	}
	if st := tl.status(); st.Outbox.Refused[teamwire.KindSessionContent]["consent_revoked"] != 1 {
		t.Fatalf("the stopped chunk is named: %+v", st.Outbox)
	}

	// The per-session cap refuses by name.
	tl.mu.Lock()
	tl.doc.ContentSessionCap = 10
	tl.parked = map[string]time.Time{} // the server has since accepted the kind
	tl.mu.Unlock()
	_, _ = postContent(t, `{"runtime":"claude","session":"s-content","enabled":true}`)
	appendWithInput(t, ix, "s-content", `{"command":"this is longer than ten bytes"}`)
	tl.pushOnce()
	if st := tl.status(); st.Outbox.Refused[teamwire.KindSessionContent]["over_session_cap"] != 1 {
		t.Fatalf("over the session cap: %+v", st.Outbox)
	}
}

// A batch is fitted under the server's request cap: session projections always ride,
// records wait for the next tick when they do not fit, and a record that cannot fit
// even alone is refused by name instead of failing every push (the wedge).
func TestPushBatchFitsUnderTheRequestCap(t *testing.T) {
	row := func(seq int64) store.OutboxRow { return store.OutboxRow{Seq: seq, Kind: store.OutboxEvent} }
	b := pushBatch{rows: map[string][]store.OutboxRow{"a": {row(1)}, "b": {row(2)}, "huge": {row(3)}}, kinds: map[string]string{}}
	for _, r := range []teamwire.PushRecord{
		{Kind: teamwire.KindEvent, ID: "a", Body: make([]byte, 40_000)},
		{Kind: teamwire.KindEvent, ID: "b", Body: make([]byte, 40_000)},
		{Kind: teamwire.KindEvent, ID: "huge", Body: make([]byte, 200_000)},
		{Kind: teamwire.KindSession, ID: "s", Body: make([]byte, 1_000)},
	} {
		b.records = append(b.records, r)
		b.kinds[r.ID] = r.Kind
	}
	got := b.fit(65_536, contentPolicy{cap: 1 << 30, sent: map[string]int64{}})
	ids := []string{}
	for _, r := range got.records {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "a,s" {
		t.Fatalf("fitted: %v", ids)
	}
	if len(got.local) != 1 || got.local[0].code != "over_push_limit" || got.local[0].row.Seq != 3 {
		t.Fatalf("the unfittable record is refused by name: %+v", got.local)
	}
	if _, ok := got.rows["b"]; ok {
		t.Fatal("a record that did not fit this time is not settled; it waits")
	}
}

// Postwork C2: unlink ends the link's queue and consents — a later link, perhaps to
// another organization, receives none of this link's backlog or opt-ins.
func TestUnlinkDropsTheQueueAndConsents(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	fake.script(map[string]string{}, true) // nothing gets through
	appendWithInput(t, ix, "s-unlink", `{"command":"x"}`)
	if code, body := postContent(t, `{"runtime":"claude","session":"s-unlink","enabled":true}`); code != 200 {
		t.Fatalf("opt in: %d %s", code, body)
	}
	appendWithInput(t, ix, "s-unlink", `{"command":"y"}`)
	tl.pushOnce()
	if got := pending(t, ix); got.Pending == 0 {
		t.Fatal("premise: rows are pending while the server fails")
	}
	if _, err := tl.unlink(); err != nil {
		t.Fatal(err)
	}
	if got := pending(t, ix); got.Pending != 0 {
		t.Fatalf("unlink leaves nothing queued: %+v", got)
	}
	if optIns, _ := ix.ContentOptIns(); len(optIns) != 0 {
		t.Fatalf("unlink leaves no consent: %v", optIns)
	}
}

// Postwork C10: consent is keyed from the session's stored events; a session this device
// never governed is refused rather than recorded under a key nothing matches.
func TestContentConsentNeedsAGovernedSession(t *testing.T) {
	_, ix, _, _ := linkForPush(t)
	if code, _ := postContent(t, `{"runtime":"claude","session":"never-seen","enabled":true}`); code != 400 {
		t.Fatalf("unknown session: %d", code)
	}
	appendWithInput(t, ix, "codex-thread", `{}`)
	if code, body := postContent(t, `{"runtime":"","session":"codex-thread","enabled":true}`); code != 200 || !strings.Contains(body, `"claude/codex-thread"`) {
		t.Fatalf("the key comes from the stored event's runtime: %d %s", code, body)
	}
}

// Postwork C7/C8: a 413 halves the fitted size and refuses a lone record by name; a
// transport failure never spends a row's retry budget.
func TestPushTooLargeShrinksAndTransportFailuresDoNotSpendAttempts(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	tl.mu.Lock()
	tl.doc.PushMaxAttempts = 2
	tl.mu.Unlock()
	fake.script(nil, true)
	tl.pushOnce()
	tl.pushOnce()
	tl.pushOnce()
	fake.script(map[string]string{teamwire.KindEvent: teamwire.CodeInternal}, false)
	tl.pushOnce()
	if got := pending(t, ix); got.Pending == 0 {
		t.Fatal("three transport failures and one internal answer must not exhaust a budget of two")
	}
	fake.mu.Lock()
	fake.tooLarge = true
	fake.mu.Unlock()
	tl.pushOnce()
	tl.mu.Lock()
	fit := tl.fitBytes
	tl.mu.Unlock()
	if fit == 0 || fit >= tl.doc.PushMaxBytes {
		t.Fatalf("a 413 halves the fitted size: %d", fit)
	}
	if st := tl.status(); st.Outbox.Refused[teamwire.KindEvent]["over_push_limit"] == 0 {
		t.Fatalf("a lone record the server refuses as too large is refused by name: %+v", st.Outbox)
	}
}

func TestRowsThroughKeepsTheHeldSnapshot(t *testing.T) {
	rows := []engine.ChainRow{{Hash: "a", Body: engine.EventChainBody{Seq: 1}}, {Hash: "", Body: engine.EventChainBody{}}, {Hash: "b", Body: engine.EventChainBody{Seq: 2}}}
	if got := rowsThrough(rows, 1); len(got) != 2 || got[0].Hash != "a" || got[1].Hash != "" {
		t.Fatalf("%+v", got)
	}
}
