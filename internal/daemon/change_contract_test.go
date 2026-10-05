package daemon

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

func TestEnvelopeOnlySessionIsFoundWithoutClaimingCapture(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "index.sqlite")
	setIndexPath(filepath.Dir(path))
	ix, e := store.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer ix.Close()
	r := &store.ChangeRecord{SessionID: "envelope-only", RepositoryID: "local-sha256-v1:r", CheckoutID: "checkout-sha256-v1:c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: t.TempDir(), SessionRuntimeClaim: "codex", SessionTitleClaim: "Envelope title", Kind: "declaration", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "plan.md", SourceDisplay: "plan.md", SourceDigest: "sha256-v1:x", RecordedAt: 10, IntentLabel: "claimed intent", Items: []store.ChangeItem{{Path: "planned.go"}}}
	if e := ix.AppendChange(r); e != nil {
		t.Fatal(e)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = prior })
	req := httptest.NewRequest("GET", "/api/govern/session?id=envelope-only", nil)
	w := httptest.NewRecorder()
	handleGovernSession(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got SessionReport
	if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil {
		t.Fatal(e)
	}
	if !got.Found || got.CaptureFound || !got.Change.Available {
		t.Fatalf("truth classes collapsed: %+v", got)
	}
	list := ScanSessions()
	found := false
	for _, s := range list {
		if s.ID == "envelope-only" {
			found = true
			if s.HasTranscript || !s.HasChangeEvidence || s.Runtime != "codex" {
				t.Fatalf("bad envelope row: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("envelope-only session absent from existing session list")
	}
	detail, e := LoadSession("codex", "envelope-only")
	if e != nil {
		t.Fatal(e)
	}
	if detail.HasTranscript || detail.TranscriptNote == "" {
		t.Fatalf("envelope detail hid transcript gap: %+v", detail)
	}
}

func TestChangeEndpointPagesRowsAndCapsWholeResponse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "index.sqlite")
	setIndexPath(filepath.Dir(path))
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	items := make([]store.ChangeItem, 250)
	for i := range items {
		items[i] = store.ChangeItem{Path: strings.Repeat("p", 20) + string(rune(0x1000+i))}
	}
	record := &store.ChangeRecord{SessionID: "paged", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: t.TempDir(), Kind: "declaration", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "plan", SourceDisplay: "plan", SourceDigest: "sha256-v1:x", RecordedAt: 1, IntentLabel: "x", Items: items}
	if err := ix.AppendChange(record); err != nil {
		t.Fatal(err)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = prior })
	req := httptest.NewRequest("GET", "/api/govern/session?id=paged&change_limit=10&change_offset=10", nil)
	w := httptest.NewRecorder()
	handleGovernSession(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() > 2<<20 {
		t.Fatalf("response exceeded budget: %d", w.Body.Len())
	}
	var got SessionReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	p := got.Change.Repositories[0].FilePage
	if p.Total != 250 || p.Returned != 10 || p.Offset != 10 || p.NextOffset == nil || p.Exact {
		t.Fatalf("paging metadata is not explicit: %+v", p)
	}
	negative := httptest.NewRecorder()
	handleGovernSession(negative, httptest.NewRequest("GET", "/api/govern/session?id=paged&change_limit=10&change_offset=-9", nil))
	if negative.Code != 200 {
		t.Fatalf("negative offset status %d: %s", negative.Code, negative.Body.String())
	}
	if err := json.Unmarshal(negative.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if p = got.Change.Repositories[0].FilePage; p.Offset != 0 || p.Returned != 10 || p.NextOffset == nil {
		t.Fatalf("negative offset was not safely canonicalized: %+v", p)
	}
	filtered := httptest.NewRecorder()
	handleGovernSession(filtered, httptest.NewRequest("GET", "/api/govern/session?id=paged&change_filter=session&change_sort=overlap&change_query=p&change_limit=25", nil))
	if filtered.Code != 200 {
		t.Fatalf("filtered projection status %d: %s", filtered.Code, filtered.Body.String())
	}
	if err := json.Unmarshal(filtered.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	fr := got.Change.Repositories[0]
	if fr.FileFilter != "session" || fr.FileSort != "overlap" || fr.FileQuery != "p" || fr.FilePage.Returned != 25 || fr.FilePage.Total != 250 {
		t.Fatalf("filtered projection metadata mismatch: %+v", fr)
	}
	for _, target := range []string{
		"/api/govern/session?id=paged&change_filter=important",
		"/api/govern/session?id=paged&change_sort=smart",
		"/api/govern/session?id=paged&observed_scope=important",
		"/api/govern/session?id=paged&change_query=" + strings.Repeat("x", 201),
	} {
		bad := httptest.NewRecorder()
		handleGovernSession(bad, httptest.NewRequest("GET", target, nil))
		if bad.Code != 400 {
			t.Fatalf("invalid projection status=%d body=%s", bad.Code, bad.Body.String())
		}
	}

	largeID := "file:" + strings.Repeat("x", (2<<20)+1024)
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertEntity(largeID, "file", largeID, 2); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(store.EventRecord{TS: 2, SessionID: "over-budget", Verb: "read", Tool: "fixture", TargetEntityID: largeID, Origin: "live"}, nil); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	over := httptest.NewRecorder()
	handleGovernSession(over, httptest.NewRequest("GET", "/api/govern/session?id=over-budget", nil))
	if over.Code != 413 || !strings.Contains(over.Body.String(), "response_metadata_over_budget") {
		t.Fatalf("fixed metadata budget status=%d body=%q", over.Code, over.Body.String())
	}
}

func TestChangeEndpointPagesObservedTargetsWithoutEnvelope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "index.sqlite")
	setIndexPath(filepath.Dir(path))
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		identity := filepath.Join(string(filepath.Separator), "crossing-guard-c9i-outside", "observed", string(rune('a'+i)))
		id := "file:" + identity
		if err := tx.UpsertEntity(id, "file", identity, int64(i+1)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.AppendEvent(store.EventRecord{TS: int64(i + 1), SessionID: "observed-only-page", Verb: "read", Tool: "fixture", TargetEntityID: id, Origin: "live"}, nil); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = prior })
	w := httptest.NewRecorder()
	handleGovernSession(w, httptest.NewRequest("GET", "/api/govern/session?id=observed-only-page&observed_limit=7&observed_offset=5&observed_scope=outside-identified-roots", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got SessionReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	p := got.Change.ObservedPage
	if got.Change.Available || p.Total != 30 || p.Offset != 5 || p.Returned != 7 || p.NextOffset == nil || got.Change.ObservedCounts["outside-identified-roots"] != 30 || !got.Change.ObservedExact {
		t.Fatalf("observed-only paging contract wrong: %+v", got.Change)
	}
	if len(got.Change.Gaps) != 2 || got.Change.Gaps[0].Code != "revision_missing" || got.Change.Gaps[1].Code != "impact_missing" {
		t.Fatalf("automatic collection gaps are not exact: %+v", got.Change.Gaps)
	}
	for _, gap := range got.Change.Gaps {
		if gap.Code == "declaration_missing" || gap.Code == "implementation_missing" || gap.Code == "verification_missing" || gap.Code == "touch_non_file_actions" {
			t.Fatalf("optional/manual or fileless activity rendered as a collection failure: %+v", got.Change.Gaps)
		}
	}
}

func TestChangeMapStaticContractKeepsFactsAndEscaping(t *testing.T) {
	b, err := staticFS.ReadFile("static/js/views/session-change.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("Declared × observed sets"), []byte("Observed file targets"), []byte("captured events referencing this path"), []byte("Capture details"), []byte("returned of ${total} matching targets"), []byte("evidence-observed-tree"), []byte("columnCount=6"), []byte("evidence-marks"), []byte("partial envelope"), []byte("Outside plan"), []byte("Collected file activity"), []byte("Code changes"), []byte("Evidence set"), []byte("Filter repository paths"), []byte("search.maxLength = 200"), []byte("option.disabled = !available"), []byte("Evidence sources · declaration"), []byte("evidence-source-details"), []byte("evidence-outside-plan"), []byte("evidence-missing-change"), []byte("change_filter"), []byte("change_query"), []byte("change_sort"), []byte("change_limit:25"), []byte("observed_limit:25"), []byte("observed_scope"), []byte("Observed target relation"), []byte("Outside identified roots"), []byte("population incomplete"), []byte("impact_limit:1"), []byte("session_linked_known"), []byte("declaration_source_digest"), []byte("revision_source_digest"), []byte("implementation_source_digest"), []byte("revision_non_atomic"), []byte("revision_sparse_checkout"), []byte("superseded(selected."), []byte("textContent"), []byte("panel-pager.js"), []byte("evidence-folder-toggle"), []byte("evidence-folder-scope"), []byte("scopeMarker.label"), []byte("aria-hidden"), []byte("group.scope==='working-directory'"), []byte("aria-expanded"), []byte("collapsedFolders"), []byte("statusMeaning"), []byte("revision status"), []byte("refAnchor"), []byte("fileEvidencePopulations"), []byte("explicit session paths"), []byte("checkout snapshot paths"), []byte("snapshot change · no explicit touch"), []byte("additional file effects unknown"), []byte("Activity · ${count} actions"), []byte("renderSessionActivity"), []byte("title: 'Changes'"), []byte("required: true")} {
		if !bytes.Contains(b, required) {
			t.Fatalf("change map lost required contract %q", required)
		}
	}
	for _, forbidden := range [][]byte{[]byte("innerHTML"), []byte("overall health"), []byte("risk score"), []byte("AI summary"), []byte("appendPathSet"), []byte("Added to scope"), []byte("evidence-change-facts"), []byte("evidence-filter-buttons"), []byte("path relationships"), []byte("for(const value of ['?','●','?','?'])"), []byte("commandForGap"), []byte("crossing-guard change ")} {
		if bytes.Contains(b, forbidden) {
			t.Fatalf("change map contains forbidden verdict/parser language %q", forbidden)
		}
	}
	tableAt := bytes.Index(b, []byte("const table = el('table', 'evidence-table evidence-tree evidence-change-tree')"))
	sourcesAfterTree := bytes.Index(b, []byte("box.appendChild(sourceDetails)"))
	if tableAt < 0 || sourcesAfterTree < tableAt {
		t.Fatal("source/provenance detail appears before the evidence tree")
	}
	css, err := staticFS.ReadFile("static/css/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("flex-wrap: nowrap"), []byte("padding-bottom:8px"), []byte("min-width:56px"), []byte("evidence-change-tree"), []byte("evidence-observed-tree"), []byte("evidence-value-strip"), []byte("evidence-value-button"), []byte(".evidence-tree tbody tr:not(.evidence-folder) th { padding-left:14px; text-transform:none; letter-spacing:normal; }"), []byte("evidence-outside-plan"), []byte("evidence-missing-change"), []byte("evidence-detail-group"), []byte("evidence-populations"), []byte("position:sticky"), []byte("evidence-folder-toggle"), []byte("evidence-folder-scope"), []byte("evidence-plan-table")} {
		if !bytes.Contains(css, required) {
			t.Fatalf("change map lost visual contract %q", required)
		}
	}
	codeState, err := staticFS.ReadFile("static/js/views/session-code-state.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("code-changes"), []byte("code-change-file"), []byte("repository_id"), []byte("checkout_id"), []byte("code_offset"), []byte("structural_offset"), []byte("current_analyzed_units"), []byte("Current analyzed files"), []byte("Changed analyzed files"), []byte("current declarations"), []byte("declaration changes"), []byte("source span"), []byte("cyclomatic"), []byte("files changed"), []byte("symbols changed"), []byte("source-span Δ"), []byte("cyclomatic Δ"), []byte("Structural matches"), []byte("mechanical match ≠ duplicate"), []byte("Measured structural match relationship changes"), []byte("Dependent packages"), []byte("Referencing files"), []byte("Incoming calls to changed declarations"), []byte("function calls"), []byte("Action attribution for this file"), []byte("of ${coverage?.total || 0} path reconciliations shown"), []byte("request !== state.request"), []byte("request !== detailState.request"), []byte("aria-expanded"), []byte("Retry code state"), []byte("Retry file facts"), []byte("textContent"), []byte("measured = value => value === null || value === undefined"), []byte("sessionEvidence(ctx, 'summary')"), []byte("sessionEvidence(ctx, 'code-changes'"), []byte("sessionEvidence(ctx, 'code-change-file'"), []byte("Code facts · ${target.path}"), []byte("state.codeTargetDetail"), []byte("Code analysis not captured for this file"), []byte("error?.status === 404")} {
		if !bytes.Contains(codeState, required) {
			t.Fatalf("code-state subview lost factual/lazy contract %q", required)
		}
	}
	for _, forbidden := range [][]byte{[]byte("provider("), []byte("innerHTML"), []byte("api("), []byte("Duplicates"), []byte("risk score"), []byte("quality score"), []byte("large method"), []byte("recommended action")} {
		if bytes.Contains(codeState, forbidden) {
			t.Fatalf("code-state subview contains duplicate owner or unsupported judgment %q", forbidden)
		}
	}
	for _, required := range [][]byte{[]byte("renderCodeState"), []byte("state.mode === 'code'"), []byte("aria-pressed"), []byte("mode:'code'"), []byte("codeCheckout:''"), []byte("codeOffset:0"), []byte("codeTarget:null")} {
		if !bytes.Contains(b, required) {
			t.Fatalf("change map lost local lazy code-state mode %q", required)
		}
	}
	if bytes.Count(b, []byte("provider({")) != 1 {
		t.Fatal("change map must retain exactly one provider registration")
	}
	for _, required := range [][]byte{[]byte("evidence-mode-switch"), []byte("button[aria-pressed=\"true\"]"), []byte("evidence-code-files"), []byte("table-layout:fixed"), []byte("overflow-wrap:anywhere"), []byte("evidence-code-file-detail"), []byte("evidence-code-attribution")} {
		if !bytes.Contains(css, required) {
			t.Fatalf("code-state view lost narrow-panel contract %q", required)
		}
	}
	sessions, err := staticFS.ReadFile("static/js/views/sessions.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("150000"), []byte("250000"), []byte("pct > 65"), []byte("pct > 85")} {
		if bytes.Contains(sessions, forbidden) {
			t.Fatalf("session usage contains unsupported context judgment cutoff %q", forbidden)
		}
	}
	for _, forbidden := range [][]byte{[]byte("id: 'session.context'"), []byte("renderFactsCard"), []byte("refAnchor(rel, '')"), []byte("sessionEvidence(ctx, token)")} {
		if bytes.Contains(sessions, forbidden) {
			t.Fatalf("sessions view retained duplicate Session Context ownership %q", forbidden)
		}
	}
	for _, required := range [][]byte{[]byte("Session context"), []byte("working directory"), []byte("recorded file paths"), []byte("created path mentions"), []byte("appendSessionOverview(box, selection)")} {
		if !bytes.Contains(b, required) {
			t.Fatalf("change map lost compact session overview %q", required)
		}
	}
	if bytes.Contains(b, []byte("selection.facts.files.map")) || bytes.Contains(b, []byte("for (const file of selection.facts.files")) {
		t.Fatal("change map overview introduced a second file-path list")
	}
	evidenceShell, err := staticFS.ReadFile("static/js/views/session-evidence.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("qualifiedKnown"), []byte("fileEvidencePopulations"), []byte("observedMillis"), []byte("checkout changed"), []byte("summary.facts?.checkout_changed"), []byte("actor/session attribution is not established"), []byte("nonFileConsistent"), []byte("sessionEvidence(ctx,'summary')"), []byte("summary.file_target_count"), []byte("evidence-header-values"), []byte("activatePane('session.change'"), []byte("governanceLink"), []byte("Diagnostics · provenance · capture")} {
		if !bytes.Contains(evidenceShell, required) {
			t.Fatalf("session evidence header lost population contract %q", required)
		}
	}
	cartography, err := staticFS.ReadFile("static/js/views/cartography.js")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cartography, []byte("evolution.hotspot")) {
		t.Fatal("cartography renders the unsupported hotspot judgment")
	}
	app, err := staticFS.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(app, []byte("./views/session-change.js")) != 1 {
		t.Fatal("change provider is not registered exactly once")
	}
	impact, err := staticFS.ReadFile("static/js/views/session-impact.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("session.impact"), []byte("title: 'Effects'"), []byte("required: true"), []byte("generation #"), []byte("snapshot_digest"), []byte("analyzer_bundle_digest"), []byte("Coverage by fact family"), []byte("Typed edges"), []byte("Mechanical code-shape candidates"), []byte("candidate ≠ duplicate"), []byte("Incoming calls to changed declarations"), []byte("Dependent code populations"), []byte("Observed test and build actions"), []byte("call edges"), []byte("dependent packages"), []byte("raw / retained"), []byte("sessionEvidence(ctx,'code-changes'"), []byte("sessionEvidence(ctx,'checks'"), []byte("Promise.allSettled"), []byte("impact_candidate_offset"), []byte("candidateOffset"), []byte("checkOffset"), []byte("impact_center"), []byte("impact.center.kind"), []byte("repository ${repositoryNumber} of ${repositoryCount}"), []byte("edge.provenance"), []byte("aria-label"), []byte("Request failed"), []byte("panel-pager.js"), []byte("textContent"), []byte("document.createElement('details')"), []byte("document.createElement('summary')"), []byte("of ${total} shown"), []byte("repositoryCount === 1"), []byte("Editor unavailable: multiple repositories"), []byte("cg:open-editor"), []byte("renderSessionVerification"), []byte("Data and secrets"), []byte("Analysis source and coverage"), []byte("codeTarget:{repository_id:")} {
		if !bytes.Contains(impact, required) {
			t.Fatalf("impact module lost factual contract %q", required)
		}
	}
	for _, forbidden := range [][]byte{[]byte("innerHTML"), []byte("risk"), []byte("important"), []byte("safe"), []byte("AI summary")} {
		if bytes.Contains(impact, forbidden) {
			t.Fatalf("impact module contains verdict/generated language %q", forbidden)
		}
	}
	if bytes.Contains(impact, []byte("error.message")) {
		t.Fatal("impact exposes raw request errors to the user")
	}
	for _, forbidden := range [][]byte{[]byte("impact.reason"), []byte("fact.reason ||"), []byte("impact.candidate_reason?`")} {
		if bytes.Contains(impact, forbidden) {
			t.Fatalf("impact exposes raw producer reason %q", forbidden)
		}
	}
	for _, required := range [][]byte{[]byte("impactBoundary"), []byte("candidateBoundary"), []byte("coverageBoundary"), []byte("Repository understanding was not completed for this snapshot.")} {
		if !bytes.Contains(impact, required) {
			t.Fatalf("impact lost deterministic producer-state boundary %q", required)
		}
	}
	if bytes.Count(app, []byte("./views/session-impact.js")) != 1 {
		t.Fatal("impact provider is not registered exactly once")
	}
	for _, module := range []string{"session-evidence.js", "session-trace.js", "session-verification.js", "session-reach.js"} {
		if bytes.Count(app, []byte("./views/"+module)) != 1 {
			t.Fatalf("C9 evidence module %s is not imported exactly once", module)
		}
		body, err := staticFS.ReadFile("static/js/views/" + module)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{[]byte("innerHTML"), []byte("risk score"), []byte("AI summary"), []byte("recommended action")} {
			if bytes.Contains(body, forbidden) {
				t.Fatalf("%s contains forbidden generated/verdict language %q", module, forbidden)
			}
		}
	}
	evidence, err := staticFS.ReadFile("static/js/views/session-evidence.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("zone:'header'"), []byte("zone:'pinned'"), []byte("required:true"), []byte("evidence-value-strip"), []byte("evidence-footer"), []byte("renderUsageStrip"), []byte("import('./session-reach.js')")} {
		if !bytes.Contains(evidence, required) {
			t.Fatalf("session evidence shell lost %q", required)
		}
	}
	trace, err := staticFS.ReadFile("static/js/views/session-trace.js")
	if err != nil || !bytes.Contains(trace, []byte("event_resources")) || !bytes.Contains(trace, []byte("next_cursor")) || !bytes.Contains(trace, []byte("sessionEvidence(ctx,'action'")) || !bytes.Contains(trace, []byte("Filter trace facts")) || !bytes.Contains(trace, []byte("All decisions")) || !bytes.Contains(trace, []byte("results_truncated")) || !bytes.Contains(trace, []byte("effects_truncated")) {
		t.Fatalf("trace lost plural-resource/partial contract: err=%v", err)
	}
	infoPanel, err := staticFS.ReadFile("static/js/infopanel.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("evidencePaneSource"), []byte("createEvidenceController"), []byte("panel-primary-host"), []byte("stableItems"), []byte("refreshItem(item"), []byte("staging.cgState = old.cgState"), []byte("scrollTop = item.host.scrollTop"), []byte("primary.activate"), []byte("enabledModules"), []byte("paneModule"), []byte("infosec-h-primary")} {
		if !bytes.Contains(infoPanel, required) {
			t.Fatalf("panel host lost non-overlapping tab viewport contract %q", required)
		}
	}
	for _, required := range [][]byte{[]byte(".pane-host"), []byte(".pane-host-area"), []byte(".pane-region"), []byte(".pane-splitter"), []byte(".pane-menu")} {
		if !bytes.Contains(css, required) {
			t.Fatalf("panel tab geometry lost %q", required)
		}
	}
	verification, err := staticFS.ReadFile("static/js/views/session-verification.js")
	if err != nil || !bytes.Contains(verification, []byte("Verification witness not captured")) || bytes.Contains(verification, []byte("? no verification evidence captured")) {
		t.Fatalf("verification state vocabulary is ambiguous: err=%v", err)
	}
	if !bytes.Contains(impact, []byte("Repository snapshot not captured")) || !bytes.Contains(impact, []byte("Request failed")) || bytes.Contains(impact, []byte("? no repository change evidence selected")) {
		t.Fatal("impact state vocabulary is ambiguous")
	}
	plan, err := staticFS.ReadFile("static/js/views/session-plan.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{[]byte("id:'session.plan'"), []byte("title:'Plan'"), []byte("required:true"), []byte("Attributed user and assistant statements"), []byte("attributed statements"), []byte("statement.snippet"), []byte("statement.snippet_truncated"), []byte("statement_offset:state.statementOffset"), []byte("statement_limit:25"), []byte("sessionEvidence(ctx,'statements'"), []byte("Promise.allSettled"), []byte("counts.planned"), []byte("counts.changed"), []byte("counts.claimed"), []byte("divergence.added?.total"), []byte("divergence.omitted?.total"), []byte("of ${pageValue(page,'total')}"), []byte("Structured decision records · not collected by this projection"), []byte("change_filter:state.filter"), []byte("change_limit:25"), []byte("textContent")} {
		if !bytes.Contains(plan, required) {
			t.Fatalf("plan view lost exact set/paging contract %q", required)
		}
	}
	for _, forbidden := range [][]byte{[]byte("innerHTML"), []byte("approved"), []byte("quality score"), []byte("risk score"), []byte("AI summary")} {
		if bytes.Contains(plan, forbidden) {
			t.Fatalf("plan view contains inference/generated language %q", forbidden)
		}
	}
	for _, module := range [][]byte{trace, verification} {
		if bytes.Contains(module, []byte("provider({")) {
			t.Fatal("activity and verification must be subviews, not primary providers")
		}
	}
	if bytes.Count(app, []byte("./views/session-plan.js")) != 1 {
		t.Fatal("plan provider is not imported exactly once")
	}
	if bytes.Count(b, []byte("provider({"))+bytes.Count(impact, []byte("provider({"))+bytes.Count(plan, []byte("provider({")) != 3 {
		t.Fatal("session review must have exactly three primary provider registrations")
	}
	governance, err := staticFS.ReadFile("static/js/views/session-governance.js")
	if err != nil {
		t.Fatal(err)
	}
	reach, err := staticFS.ReadFile("static/js/views/session-reach.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"activity": trace, "verification": verification, "reach": reach, "governance": governance} {
		if bytes.Contains(body, []byte("provider({")) {
			t.Fatalf("%s must be a subview/link, not a primary provider", name)
		}
	}
	for _, removed := range [][]byte{[]byte("id: 'session.exposure'"), []byte("id: 'session.usage'")} {
		if bytes.Contains(sessions, removed) {
			t.Fatalf("sessions retained obsolete primary provider %q", removed)
		}
	}
	exposureStart := bytes.Index(sessions, []byte("export function renderExposure"))
	if exposureStart < 0 || bytes.Contains(sessions[exposureStart:], []byte("line.innerHTML")) || !bytes.Contains(sessions[exposureStart:], []byte("box.replaceChildren")) {
		t.Fatal("reused exposure subview must use DOM construction and fixed rendering")
	}
	pager, err := staticFS.ReadFile("static/js/views/panel-pager.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pager, []byte("Next ${label}")) || !bytes.Contains(pager, []byte("Object.entries(reset)")) {
		t.Fatal("shared panel pager lost movement/reset contract")
	}
}

func TestSessionImpactEndpointReturnsExactTypedFactsAndRejectsBadCenter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "index.sqlite")
	setIndexPath(filepath.Dir(path))
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	digest := "git-tree-v2-sha256:endpoint"
	revision := &store.ChangeRecord{SessionID: "impact", RepositoryID: "repo", CheckoutID: "checkout", RepositoryIdentityKind: "local-sha256", CheckoutRoot: t.TempDir(), Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "git:head", SourceDisplay: "Git", SourceDigest: digest, RecordedAt: 10, CaptureStartedAt: 9, CaptureEndedAt: 10, BaseRevision: "base", HeadRevision: "head", SnapshotDigest: digest, Items: []store.ChangeItem{{Path: "changed.go", Layer: "worktree", Status: "M"}}}
	if err := ix.AppendChange(revision); err != nil {
		t.Fatal(err)
	}
	bundle := currentAnalyzerBundle()
	descriptor := func(path, name string) string {
		body, err := json.Marshal(codemap.UnitDescriptor{Identity: codemap.Identity{Path: path, Language: "go", AnalyzerID: "go-ast-v2"}, Symbol: codemap.Symbol{Declarations: []codemap.Declaration{{Name: name, BodyShapeDigest: "shape-v1:same", BodyShapeNodes: 3}}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	generation := &store.UnderstandingGeneration{RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: revision.CheckoutRoot, Status: "complete", SnapshotProtocol: "git-tree-v2", SnapshotDigest: digest, StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: bundle, ConventionState: "none", StartedAt: 20, EndedAt: 30, Units: []store.UnderstandingUnit{{Path: "changed.go", SourceHash: "sha256-v1:changed", Language: "go", DescriptorJSON: descriptor("changed.go", "Changed")}, {Path: "similar.go", SourceHash: "sha256-v1:similar", Language: "go", DescriptorJSON: descriptor("similar.go", "Similar")}}, Edges: []store.UnderstandingEdge{{FromKind: "file", FromRef: "changed.go", Relation: "file_declares_symbol", ToKind: "symbol", ToRef: "pkg::Changed", SourcePath: "changed.go", SourceLine: 7, Provenance: "measured", AnalyzerID: "go-ast-v2", EvidenceDigest: "sha256-v1:edge"}}, Coverage: []store.UnderstandingCoverage{{Family: "symbol_declaration", State: "complete", AnalyzerID: "go-ast-v2", Attempted: 2, Produced: 2}, {Family: "responsibility_fingerprint", State: "complete", AnalyzerID: "go-ast-v2", Attempted: 2, Produced: 2}, {Family: "symbol_call", State: "unsupported", AnalyzerID: "framework-boundary-v1", Reason: "no producer"}}}
	if err := ix.AppendUnderstanding(generation); err != nil {
		t.Fatal(err)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = prior })
	w := httptest.NewRecorder()
	handleGovernSession(w, httptest.NewRequest("GET", "/api/govern/session?id=impact&impact_limit=1", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got SessionReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	impact := got.Change.Repositories[0].Downstream
	if impact.State != "exact" || impact.Generation == nil || impact.Generation.ID != generation.ID || impact.EdgePage.Total != 1 || len(impact.Edges) != 1 || impact.Edges[0].Relation != "file_declares_symbol" {
		t.Fatalf("typed impact contract mismatch: %+v", impact)
	}
	bad := httptest.NewRecorder()
	handleGovernSession(bad, httptest.NewRequest("GET", "/api/govern/session?id=impact&impact_center=malformed", nil))
	if bad.Code != 400 {
		t.Fatalf("malformed center status=%d body=%s", bad.Code, bad.Body.String())
	}
	unknown := httptest.NewRecorder()
	handleGovernSession(unknown, httptest.NewRequest("GET", "/api/govern/session?id=impact&impact_center=symbol:invented", nil))
	if unknown.Code != 404 || !strings.Contains(unknown.Body.String(), "impact center not found") {
		t.Fatalf("unknown center status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	centered := httptest.NewRecorder()
	handleGovernSession(centered, httptest.NewRequest("GET", "/api/govern/session?id=impact&impact_center=symbol:pkg::Changed", nil))
	if centered.Code != 200 {
		t.Fatalf("known center status=%d body=%s", centered.Code, centered.Body.String())
	}
	var centeredReport SessionReport
	if err := json.Unmarshal(centered.Body.Bytes(), &centeredReport); err != nil {
		t.Fatal(err)
	}
	center := centeredReport.Change.Repositories[0].Downstream.Center
	if center == nil || center.Kind != "symbol" || center.Ref != "pkg::Changed" || center.Provenance != "measured" || center.SourceID != generation.ID {
		t.Fatalf("known center did not resolve to stored fact: %+v", center)
	}
	fileCentered := httptest.NewRecorder()
	handleGovernSession(fileCentered, httptest.NewRequest("GET", "/api/govern/session?id=impact&impact_center=file:changed.go&impact_candidate_offset=0&impact_limit=1", nil))
	if fileCentered.Code != 200 {
		t.Fatalf("file center status=%d body=%s", fileCentered.Code, fileCentered.Body.String())
	}
	var fileReport SessionReport
	if err := json.Unmarshal(fileCentered.Body.Bytes(), &fileReport); err != nil {
		t.Fatal(err)
	}
	fileImpact := fileReport.Change.Repositories[0].Downstream
	if fileImpact.CandidateState != "exact" || fileImpact.CandidatePage.Total != 1 || fileImpact.CandidatePage.Limit != 1 || len(fileImpact.Candidates) != 1 || fileImpact.Candidates[0].RightPath != "similar.go" {
		t.Fatalf("candidate API contract mismatch: %+v", fileImpact)
	}
}
