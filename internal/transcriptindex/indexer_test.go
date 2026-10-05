package transcriptindex

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var fixedIndexTime = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

type fakeCatalog struct {
	discovery       Discovery
	discoverErr     error
	snapshots       map[SessionKey]SourceSnapshot
	readErrors      map[SessionKey]error
	generations     map[SessionKey]string
	generationErr   map[SessionKey]error
	readCalls       []SessionKey
	generationCalls []SessionKey
}

func (f *fakeCatalog) Discover(context.Context, Limits) (Discovery, error) {
	return f.discovery, f.discoverErr
}

func (f *fakeCatalog) Read(_ context.Context, session SourceSession, _ Limits) (SourceSnapshot, error) {
	f.readCalls = append(f.readCalls, session.Key)
	if err := f.readErrors[session.Key]; err != nil {
		return SourceSnapshot{}, err
	}
	return f.snapshots[session.Key], nil
}

func (f *fakeCatalog) Generation(_ context.Context, session SourceSession, _ Limits) (string, error) {
	f.generationCalls = append(f.generationCalls, session.Key)
	if err := f.generationErr[session.Key]; err != nil {
		return "", err
	}
	if generation, ok := f.generations[session.Key]; ok {
		return generation, nil
	}
	return session.Generation, nil
}

type fakeRepository struct {
	states       map[SessionKey]ProjectionState
	statesErr    error
	replacements []ProjectionReplacement
	replaceErr   error
	removeCalls  int
	removeKeys   []SessionKey
	removeCount  int
	removeErr    error
	recordErr    error
	recordState  *ProjectionState
}

func (f *fakeRepository) ProjectionStates(context.Context) (map[SessionKey]ProjectionState, error) {
	return copyProjectionStates(f.states), f.statesErr
}

func (f *fakeRepository) ReplaceTranscriptProjection(_ context.Context,
	replacement ProjectionReplacement) (ReplaceResult, error) {
	if f.replaceErr != nil {
		return ReplaceResult{}, f.replaceErr
	}
	f.replacements = append(f.replacements, replacement)
	state := ProjectionState{
		Key: replacement.Session.Key, Generation: replacement.Generation,
		IndexedAt: replacement.IndexedAt, SourceCount: replacement.SourceCount,
		DocumentCount: len(replacement.Documents), LastAttemptedAt: replacement.IndexedAt,
	}
	if f.states == nil {
		f.states = map[SessionKey]ProjectionState{}
	}
	f.states[state.Key] = state
	return ReplaceResult{State: state, Replaced: true}, nil
}

func (f *fakeRepository) RecordTranscriptProjectionLimitation(_ context.Context, key SessionKey,
	expected ProjectionState, attemptedAt time.Time, limitation Limitation) (ProjectionState, error) {
	if f.recordErr != nil {
		return ProjectionState{}, f.recordErr
	}
	if f.recordState != nil {
		return *f.recordState, nil
	}
	if f.states == nil {
		f.states = map[SessionKey]ProjectionState{}
	}
	state := f.states[key]
	if state.Generation != expected.Generation || state.IndexedAt != expected.IndexedAt {
		return state, nil
	}
	state.Key = key
	state.LastAttemptedAt = attemptedAt
	limitationCopy := limitation
	state.Limitation = &limitationCopy
	f.states[key] = state
	return state, nil
}

func TestStaleFailureCannotDowngradeConcurrentSuccessfulCoverage(t *testing.T) {
	session := sourceSession("claude", "concurrent-success", "g1", fixedIndexTime)
	newer := ProjectionState{Key: session.Key, Generation: "g1",
		IndexedAt: fixedIndexTime.Add(time.Minute)}
	catalog := &fakeCatalog{
		discovery: Discovery{Sessions: []SourceSession{session}, Complete: true, AsOf: fixedIndexTime},
		readErrors: map[SessionKey]error{session.Key: NewLimitedError(Limitation{
			Kind: LimitationSourceUnreadable}, errors.New("stale read failed"))},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{}, recordState: &newer}
	result, err := newTestIndexer(catalog, repository).Refresh(context.Background(), RefreshRequest{})
	if err != nil {
		t.Fatal(err)
	}
	coverage := result.Coverage.ForSession(session.Key)
	if len(coverage.Sessions) != 1 || coverage.Sessions[0].IndexedGeneration != "g1" ||
		coverage.Sessions[0].Limitations != nil {
		t.Fatalf("concurrent success was downgraded: %+v", coverage)
	}
}

func (f *fakeRepository) RemoveTranscriptOrphans(_ context.Context, keys []SessionKey) (int, error) {
	f.removeCalls++
	f.removeKeys = append([]SessionKey(nil), keys...)
	return f.removeCount, f.removeErr
}

func sourceSession(runtime, id, generation string, modified time.Time) SourceSession {
	key := SessionKey{Runtime: runtime, SessionID: id}
	return SourceSession{
		Key: key, Title: "A useful title", TitleSource: "native", Path: "/opaque/source",
		CWD: "/repo", Project: "repo", Modified: modified, Turns: 2,
		SourceBytes: 100, Generation: generation,
		Segments: []SourceSegment{{ID: "segment-1", SourceRef: "opaque", SourceBytes: 100}},
	}
}

func sourceSnapshot(session SourceSession, events ...SourceEvent) SourceSnapshot {
	return SourceSnapshot{
		Session: session, Events: events, Generation: session.Generation,
		SourceBytes: session.SourceBytes,
	}
}

func newTestIndexer(catalog Catalog, repository Repository) *Indexer {
	indexer := New(catalog, repository)
	indexer.now = func() time.Time { return fixedIndexTime }
	return indexer
}

func TestPlanIncrementalAndForceAreNewestFirstAndBounded(t *testing.T) {
	old := sourceSession("claude", "old", "g2", fixedIndexTime.Add(-2*time.Hour))
	newest := sourceSession("codex", "new", "g1", fixedIndexTime)
	middle := sourceSession("claude", "middle", "g1", fixedIndexTime.Add(-time.Hour))
	catalog := &fakeCatalog{discovery: Discovery{
		Sessions: []SourceSession{old, newest, middle}, Complete: true, AsOf: fixedIndexTime,
	}}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{
		newest.Key: {Key: newest.Key, Generation: newest.Generation, IndexedAt: fixedIndexTime},
		old.Key:    {Key: old.Key, Generation: "g1", IndexedAt: fixedIndexTime.Add(-time.Hour)},
	}}
	indexer := newTestIndexer(catalog, repository)

	incremental, err := indexer.Plan(context.Background(), RefreshRequest{
		Mode: RefreshModeIncremental, MaxSessions: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionKeys(incremental.Sessions); !reflect.DeepEqual(got, []SessionKey{middle.Key}) {
		t.Fatalf("incremental plan = %#v, want newest outstanding session", got)
	}
	if incremental.Stats.Skipped != 1 || incremental.Stats.Remaining != 1 ||
		incremental.Coverage.State != CoverageStale {
		t.Fatalf("incremental stats/coverage = %+v / %+v", incremental.Stats, incremental.Coverage)
	}

	force, err := indexer.Plan(context.Background(), RefreshRequest{
		Mode: RefreshModeForce, MaxSessions: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionKeys(force.Sessions); !reflect.DeepEqual(got, []SessionKey{newest.Key, middle.Key}) {
		t.Fatalf("force plan = %#v", got)
	}
	if force.Stats.Skipped != 0 || force.Stats.Remaining != 1 {
		t.Fatalf("force stats = %+v", force.Stats)
	}
}

func TestRefreshMapsOneTitleAndCanonicalEventKinds(t *testing.T) {
	session := sourceSession("claude", "session-1", "g1", fixedIndexTime.Add(-time.Minute))
	snapshot := sourceSnapshot(session,
		SourceEvent{Ordinal: 0, Lineage: "event-0", Timestamp: "t0", Kind: "user", Text: "  sprint retro  "},
		SourceEvent{Ordinal: 1, Lineage: "event-1", Timestamp: "t1", Kind: "assistant", Text: "agreements\x00"},
		SourceEvent{Ordinal: 2, Lineage: "event-2", Timestamp: "t2", Kind: "tool_call", Name: "Read", Text: strings.Repeat("x", 205)},
		SourceEvent{Ordinal: 3, Lineage: "event-3", Timestamp: "t3", Kind: "tool_result", Text: " result "},
		SourceEvent{Ordinal: 4, Lineage: "event-4", Timestamp: "t4", Kind: "thinking", Text: "private"},
		SourceEvent{Ordinal: 5, Lineage: "event-5", Timestamp: "t5", Kind: "user", Text: "   "},
	)
	catalog := &fakeCatalog{
		discovery: Discovery{Sessions: []SourceSession{session}, Complete: true, AsOf: fixedIndexTime},
		snapshots: map[SessionKey]SourceSnapshot{session.Key: snapshot},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{}}
	result, err := newTestIndexer(catalog, repository).Refresh(context.Background(), RefreshRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.replacements) != 1 {
		t.Fatalf("replacement calls = %d", len(repository.replacements))
	}
	replacement := repository.replacements[0]
	if replacement.ExpectedGeneration != "" {
		t.Fatalf("new projection expected generation = %q", replacement.ExpectedGeneration)
	}
	if got := documentKinds(replacement.Documents); !reflect.DeepEqual(got,
		[]string{"title", "user", "assistant", "tool_call", "tool_result"}) {
		t.Fatalf("document kinds = %#v", got)
	}
	if replacement.Documents[0].Text != session.Title || replacement.Documents[0].Order != 0 {
		t.Fatalf("title document = %+v", replacement.Documents[0])
	}
	if replacement.Documents[1].Text != "sprint retro" ||
		replacement.Documents[2].Text != "agreements" ||
		replacement.Documents[3].Text != "Read "+strings.Repeat("x", 200)+"…" {
		t.Fatalf("mapped documents = %+v", replacement.Documents)
	}
	for order, document := range replacement.Documents {
		if document.Order != order {
			t.Fatalf("document order %d = %d", order, document.Order)
		}
	}
	if result.Coverage.State != CoverageCurrent || result.Stats.Updated != 1 ||
		repository.removeCalls != 1 {
		t.Fatalf("refresh result = %+v; remove calls = %d", result, repository.removeCalls)
	}
}

func TestForceReplacementCarriesPlannedCurrentGeneration(t *testing.T) {
	session := sourceSession("claude", "force-current", "g1", fixedIndexTime)
	catalog := &fakeCatalog{
		discovery: Discovery{Sessions: []SourceSession{session}, Complete: true, AsOf: fixedIndexTime},
		snapshots: map[SessionKey]SourceSnapshot{session.Key: sourceSnapshot(session)},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{
		session.Key: {Key: session.Key, Generation: session.Generation,
			IndexedAt: fixedIndexTime.Add(-time.Hour)},
	}}
	result, err := newTestIndexer(catalog, repository).Refresh(context.Background(), RefreshRequest{
		Mode: RefreshModeForce,
	})
	if err != nil || result.Stats.Updated != 1 || len(repository.replacements) != 1 {
		t.Fatalf("force result=%+v replacements=%d err=%v", result, len(repository.replacements), err)
	}
	if got := repository.replacements[0].ExpectedGeneration; got != session.Generation {
		t.Fatalf("expected generation=%q want=%q", got, session.Generation)
	}
	if got := repository.replacements[0].ExpectedIndexedAt; got != fixedIndexTime.Add(-time.Hour) {
		t.Fatalf("expected indexed-at=%v", got)
	}
}

func TestAggregateAdmissionCeilingsPreserveOldGoodProjection(t *testing.T) {
	t.Run("exact 25000 document ceiling", func(t *testing.T) {
		session := sourceSession("claude", "many-documents", "g2", fixedIndexTime)
		events := make([]SourceEvent, DefaultMaxDocuments)
		for index := range events {
			events[index] = SourceEvent{Kind: "user", Text: "x", Lineage: "event"}
		}
		assertLimitPreservesOldGood(t, session, sourceSnapshot(session, events...),
			LimitationDocumentLimit)
	})

	t.Run("exact 16 MiB text ceiling", func(t *testing.T) {
		session := sourceSession("codex", "large-text", "g2", fixedIndexTime)
		session.Title = ""
		snapshot := sourceSnapshot(session, SourceEvent{
			Kind: "assistant", Text: strings.Repeat("x", int(DefaultMaxTextBytes)+1), Lineage: "event",
		})
		assertLimitPreservesOldGood(t, session, snapshot, LimitationTextLimit)
	})
}

func assertLimitPreservesOldGood(t *testing.T, session SourceSession, snapshot SourceSnapshot,
	want LimitationKind) {
	t.Helper()
	oldIndexedAt := fixedIndexTime.Add(-time.Hour)
	catalog := &fakeCatalog{
		discovery: Discovery{Sessions: []SourceSession{session}, Complete: true, AsOf: fixedIndexTime},
		snapshots: map[SessionKey]SourceSnapshot{session.Key: snapshot},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{
		session.Key: {Key: session.Key, Generation: "g1", IndexedAt: oldIndexedAt},
	}}
	result, err := newTestIndexer(catalog, repository).Refresh(context.Background(), RefreshRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.replacements) != 0 || repository.removeCalls != 0 {
		t.Fatalf("limit mutated repository: replacements=%d removes=%d",
			len(repository.replacements), repository.removeCalls)
	}
	if repository.states[session.Key].Generation != "g1" {
		t.Fatalf("old-good generation changed: %+v", repository.states[session.Key])
	}
	if repository.states[session.Key].Limitation == nil ||
		repository.states[session.Key].Limitation.Kind != want {
		t.Fatalf("attempt limitation was not recorded: %+v", repository.states[session.Key])
	}
	if result.Coverage.State != CoverageIncomplete || result.Stats.Failed != 1 ||
		len(result.Coverage.Limitations) != 1 || result.Coverage.Limitations[0].Kind != want {
		t.Fatalf("limit coverage = %+v", result)
	}
}

func TestRefreshRechecksGenerationBeforeRepositoryWrite(t *testing.T) {
	session := sourceSession("codex", "mutated", "g1", fixedIndexTime)
	catalog := &fakeCatalog{
		discovery:   Discovery{Sessions: []SourceSession{session}, Complete: true, AsOf: fixedIndexTime},
		snapshots:   map[SessionKey]SourceSnapshot{session.Key: sourceSnapshot(session)},
		generations: map[SessionKey]string{session.Key: "g2"},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{}}
	result, err := newTestIndexer(catalog, repository).Refresh(context.Background(), RefreshRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.replacements) != 0 || result.Coverage.State != CoverageIncomplete ||
		result.Coverage.Limitations[0].Kind != LimitationSourceMutated {
		t.Fatalf("mutation result = %+v; replacements=%d", result, len(repository.replacements))
	}
}

func TestOrphansAreRemovedOnlyAfterCompleteSuccessfulDiscovery(t *testing.T) {
	session := sourceSession("opencode", "session", "g1", fixedIndexTime)
	for _, test := range []struct {
		name        string
		complete    bool
		limitations []Limitation
		wantRemoves int
	}{
		{name: "complete", complete: true, wantRemoves: 1},
		{name: "incomplete", complete: false, wantRemoves: 0},
		{name: "limited", complete: true,
			limitations: []Limitation{{Kind: LimitationUnsupportedAdapter, Runtime: "future"}},
			wantRemoves: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := &fakeCatalog{discovery: Discovery{
				Sessions: []SourceSession{session}, Complete: test.complete,
				AsOf: fixedIndexTime, Limitations: test.limitations,
			}}
			repository := &fakeRepository{states: map[SessionKey]ProjectionState{
				session.Key: {Key: session.Key, Generation: session.Generation, IndexedAt: fixedIndexTime},
			}}
			_, err := newTestIndexer(catalog, repository).Refresh(context.Background(), RefreshRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if repository.removeCalls != test.wantRemoves {
				t.Fatalf("remove calls = %d, want %d", repository.removeCalls, test.wantRemoves)
			}
		})
	}
}

func TestDiscoveryLimitationCannotReplaceAPartialCanonicalSession(t *testing.T) {
	session := sourceSession("codex", "resumed", "partial-generation", fixedIndexTime)
	limitation := Limitation{Kind: LimitationSourceTooLarge, Runtime: "codex",
		ObservedBytes: DefaultMaxSourceBytes + 1, LimitBytes: DefaultMaxSourceBytes}
	catalog := &fakeCatalog{
		discovery: Discovery{Sessions: []SourceSession{session}, Complete: false,
			AsOf: fixedIndexTime, Limitations: []Limitation{limitation}},
		snapshots: map[SessionKey]SourceSnapshot{session.Key: sourceSnapshot(session)},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{
		session.Key: {Key: session.Key, Generation: "old-good", IndexedAt: fixedIndexTime.Add(-time.Hour)},
	}}
	result, err := newTestIndexer(catalog, repository).Refresh(context.Background(), RefreshRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.readCalls) != 0 || len(repository.replacements) != 0 ||
		repository.states[session.Key].Generation != "old-good" {
		t.Fatalf("limited discovery touched partial session: reads=%v replacements=%d state=%+v",
			catalog.readCalls, len(repository.replacements), repository.states[session.Key])
	}
	if result.Coverage.State != CoverageIncomplete ||
		result.Coverage.ForSession(session.Key).State != CoverageIncomplete {
		t.Fatalf("limited discovery coverage = %+v", result.Coverage)
	}
}

func TestRecoverableReadFailureRetriesToCurrent(t *testing.T) {
	session := sourceSession("claude", "retry", "g2", fixedIndexTime)
	catalog := &fakeCatalog{
		discovery: Discovery{Sessions: []SourceSession{session}, Complete: true, AsOf: fixedIndexTime},
		snapshots: map[SessionKey]SourceSnapshot{session.Key: sourceSnapshot(session)},
		readErrors: map[SessionKey]error{session.Key: NewLimitedError(Limitation{
			Kind: LimitationSourceUnreadable, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID}, errors.New("diagnostic only"))},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{
		session.Key: {Key: session.Key, Generation: "g1", IndexedAt: fixedIndexTime.Add(-time.Hour)},
	}}
	indexer := newTestIndexer(catalog, repository)

	first, err := indexer.Refresh(context.Background(), RefreshRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Coverage.State != CoverageIncomplete || repository.states[session.Key].Generation != "g1" {
		t.Fatalf("first refresh = %+v", first)
	}
	delete(catalog.readErrors, session.Key)
	second, err := indexer.Refresh(context.Background(), RefreshRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Coverage.State != CoverageCurrent || repository.states[session.Key].Generation != "g2" {
		t.Fatalf("second refresh = %+v; state = %+v", second, repository.states[session.Key])
	}
}

func TestRecordedForceFailureRetriesEvenWhenGenerationStillMatches(t *testing.T) {
	session := sourceSession("claude", "force-retry", "g1", fixedIndexTime)
	catalog := &fakeCatalog{
		discovery: Discovery{Sessions: []SourceSession{session}, Complete: true, AsOf: fixedIndexTime},
		snapshots: map[SessionKey]SourceSnapshot{session.Key: sourceSnapshot(session)},
		readErrors: map[SessionKey]error{session.Key: NewLimitedError(Limitation{
			Kind: LimitationSourceUnreadable}, errors.New("transient read failure"))},
	}
	repository := &fakeRepository{states: map[SessionKey]ProjectionState{
		session.Key: {Key: session.Key, Generation: session.Generation,
			IndexedAt: fixedIndexTime.Add(-time.Hour)},
	}}
	indexer := newTestIndexer(catalog, repository)

	first, err := indexer.Refresh(context.Background(), RefreshRequest{Mode: RefreshModeForce})
	if err != nil || first.Coverage.State != CoverageIncomplete {
		t.Fatalf("force failure = %+v, err=%v", first, err)
	}
	delete(catalog.readErrors, session.Key)
	second, err := indexer.Refresh(context.Background(), RefreshRequest{Mode: RefreshModeIncremental})
	if err != nil {
		t.Fatal(err)
	}
	if second.Stats.Updated != 1 || second.Coverage.State != CoverageCurrent {
		t.Fatalf("incremental retry = %+v", second)
	}
}

func TestCoverageTransitionsAndSessionProjection(t *testing.T) {
	current := sourceSession("claude", "current", "g1", fixedIndexTime)
	missing := sourceSession("codex", "missing", "g1", fixedIndexTime.Add(-time.Minute))
	stale := sourceSession("opencode", "stale", "g2", fixedIndexTime.Add(-2*time.Minute))
	discovery := Discovery{
		Sessions: []SourceSession{current, missing, stale}, Complete: true, AsOf: fixedIndexTime,
	}
	states := map[SessionKey]ProjectionState{
		current.Key: {Key: current.Key, Generation: "g1", IndexedAt: fixedIndexTime.Add(-time.Hour)},
		stale.Key:   {Key: stale.Key, Generation: "g1", IndexedAt: fixedIndexTime.Add(-2 * time.Hour)},
	}
	coverage := buildCoverage(discovery, states, nil)
	if coverage.State != CoverageStale || coverage.IndexedSessions != 1 ||
		coverage.NewestUnindexedModified != missing.Modified {
		t.Fatalf("mixed coverage = %+v", coverage)
	}
	if narrowed := coverage.ForSession(current.Key); narrowed.State != CoverageCurrent ||
		narrowed.IndexedSessions != 1 || narrowed.DiscoveredSessions != 1 {
		t.Fatalf("current session coverage = %+v", narrowed)
	}
	if narrowed := coverage.ForSession(missing.Key); narrowed.State != CoverageCatchingUp ||
		narrowed.IndexedSessions != 0 {
		t.Fatalf("missing session coverage = %+v", narrowed)
	}

	discovery.Complete = false
	discovery.Limitations = []Limitation{{Kind: LimitationUnsupportedAdapter, Runtime: "future"}}
	if incomplete := buildCoverage(discovery, states, nil); incomplete.State != CoverageIncomplete {
		t.Fatalf("incomplete coverage = %+v", incomplete)
	}

	repository := &fakeRepository{statesErr: errors.New("database unavailable")}
	result, err := newTestIndexer(&fakeCatalog{discovery: discovery}, repository).
		Refresh(context.Background(), RefreshRequest{})
	if err == nil || result.Coverage.State != CoverageUnavailable ||
		result.Coverage.Limitations[0].Kind != LimitationRepositoryUnavailable {
		t.Fatalf("unavailable result = %+v, err=%v", result, err)
	}
}

func TestDefaultLimitsPinAggregateProductionCeilings(t *testing.T) {
	limits := DefaultLimits()
	if limits.MaxSourceBytes != 64<<20 || limits.MaxDocuments != 25_000 ||
		limits.MaxTextBytes != 16<<20 {
		t.Fatalf("default limits = %+v", limits)
	}
}

func sessionKeys(sessions []SourceSession) []SessionKey {
	keys := make([]SessionKey, 0, len(sessions))
	for _, session := range sessions {
		keys = append(keys, session.Key)
	}
	return keys
}

func documentKinds(documents []SearchDocument) []string {
	kinds := make([]string, 0, len(documents))
	for _, document := range documents {
		kinds = append(kinds, document.Kind)
	}
	return kinds
}
