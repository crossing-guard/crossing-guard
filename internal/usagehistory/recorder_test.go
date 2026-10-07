package usagehistory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// fakeSource serves fixed calls per source key. Each read costs the source's
// Size in bytes and completes it.
type fakeSource struct {
	reader  string
	refs    []harvest.UsageSourceRef
	calls   map[string][]harvest.UsageCall
	failKey string
	reads   []string
	// roles is what a read learns from inside the source (Codex).
	roles map[string]string
}

func (f *fakeSource) UsageReader() string { return f.reader }
func (f *fakeSource) UsageSources(context.Context) ([]harvest.UsageSourceRef, error) {
	return f.refs, nil
}
func (f *fakeSource) ReadUsage(_ context.Context, request harvest.UsageReadRequest) (harvest.UsageBatch, error) {
	f.reads = append(f.reads, request.Source.Key)
	if request.Source.Key == f.failKey {
		return harvest.UsageBatch{}, errors.New("unreadable")
	}
	return harvest.UsageBatch{Calls: f.calls[request.Source.Key], Cursor: []byte("end"),
		BytesRead: request.Source.Size, Complete: true, Role: f.roles[request.Source.Key]}, nil
}

func stated(value int64) *int64 { return &value }

func call(id, session string, at time.Time, output int64) harvest.UsageCall {
	return harvest.UsageCall{ID: id, Session: session, FirstAt: at, At: at, Model: "m",
		TokenClasses: harvest.TokenClasses{Input: stated(1), Output: stated(output)}}
}

func openStore(t *testing.T) *store.Index {
	t.Helper()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func totalOutput(t *testing.T, ix *store.Index) (int64, int64) {
	t.Helper()
	groups, err := ix.UsageGroups(store.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		return 0, 0
	}
	return groups[0].Calls, groups[0].Output.Sum
}

func TestPassSpendsTheBudgetAndResumes(t *testing.T) {
	ix := openStore(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{reader: "fake/usage-1", calls: map[string][]harvest.UsageCall{}}
	for index, key := range []string{"a", "b", "c"} {
		source.refs = append(source.refs, harvest.UsageSourceRef{Key: key, Session: "s-" + key, Marker: "1",
			Size: 100, Modified: now.Add(-time.Duration(index) * time.Hour)})
		source.calls[key] = []harvest.UsageCall{call("call-"+key, "s-"+key, now, 10)}
	}
	recorder := New(map[string]harvest.UsageSource{"fake": source}, ix)
	first, err := recorder.Pass(context.Background(), 150)
	if err != nil || first.Recorded != 2 || first.Pending != 1 || first.Discovered != 3 {
		t.Fatalf("first pass: %+v err=%v", first, err)
	}
	if source.reads[0] != "a" || source.reads[1] != "b" {
		t.Fatalf("a backfill reads newest first: %v", source.reads)
	}
	second, err := recorder.Pass(context.Background(), 150)
	if err != nil || second.Recorded != 3 || second.Pending != 0 {
		t.Fatalf("second pass: %+v err=%v", second, err)
	}
	if calls, output := totalOutput(t, ix); calls != 3 || output != 30 {
		t.Fatalf("calls=%d output=%d", calls, output)
	}
	// Nothing changed: the next pass reads nothing.
	source.reads = nil
	third, _ := recorder.Pass(context.Background(), 150)
	if len(source.reads) != 0 || third.Recorded != 3 {
		t.Fatalf("an unchanged source is not read again: %v %+v", source.reads, third)
	}
}

func TestPassReadsLiveSourcesBeforeTheBackfill(t *testing.T) {
	ix := openStore(t)
	start := time.Now().Add(-time.Hour)
	source := &fakeSource{reader: "fake/usage-1", calls: map[string][]harvest.UsageCall{}}
	source.refs = []harvest.UsageSourceRef{{Key: "old", Session: "s-old", Marker: "1", Size: 100, Modified: start.Add(-48 * time.Hour)}}
	recorder := New(map[string]harvest.UsageSource{"fake": source}, ix)
	if _, err := recorder.Pass(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// A large old backfill source and a new live one appear; the live one goes first.
	source.refs = append(source.refs,
		harvest.UsageSourceRef{Key: "older", Session: "s-older", Marker: "1", Size: 100, Modified: start.Add(-72 * time.Hour)},
		harvest.UsageSourceRef{Key: "live", Session: "s-live", Marker: "1", Size: 100, Modified: time.Now().Add(time.Minute)})
	source.reads = nil
	if _, err := recorder.Pass(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(source.reads) != 1 || source.reads[0] != "live" {
		t.Fatalf("live first: %v", source.reads)
	}
}

func TestReaderChangeRestartsTheSource(t *testing.T) {
	ix := openStore(t)
	now := time.Now()
	source := &fakeSource{reader: "fake/usage-1", refs: []harvest.UsageSourceRef{{Key: "a", Session: "s", Marker: "1", Size: 1}},
		calls: map[string][]harvest.UsageCall{"a": {call("old-id", "s", now, 5)}}}
	recorder := New(map[string]harvest.UsageSource{"fake": source}, ix)
	if _, err := recorder.Pass(context.Background(), 1<<20); err != nil {
		t.Fatal(err)
	}
	// A new mapping names the same call differently: the old row must go.
	source.reader = "fake/usage-2"
	source.calls["a"] = []harvest.UsageCall{call("new-id", "s", now, 5)}
	result, err := recorder.Pass(context.Background(), 1<<20)
	if err != nil || result.Removed != 1 {
		t.Fatalf("result: %+v err=%v", result, err)
	}
	if calls, _ := totalOutput(t, ix); calls != 1 {
		t.Fatalf("one call after the re-read, got %d", calls)
	}
}

func TestAFailedReadDoesNotStopThePass(t *testing.T) {
	ix := openStore(t)
	now := time.Now()
	source := &fakeSource{reader: "fake/usage-1", failKey: "bad",
		refs: []harvest.UsageSourceRef{{Key: "bad", Session: "s1", Marker: "1", Size: 1, Modified: now},
			{Key: "good", Session: "s2", Marker: "1", Size: 1, Modified: now.Add(-time.Minute)}},
		calls: map[string][]harvest.UsageCall{"good": {call("g", "s2", now, 3)}}}
	result, err := New(map[string]harvest.UsageSource{"fake": source}, ix).Pass(context.Background(), 1<<20)
	if err != nil || result.Failed != 1 || result.Recorded != 1 || result.Pending != 1 {
		t.Fatalf("result: %+v err=%v", result, err)
	}
}

func TestCallRecordIsCanonical(t *testing.T) {
	at := time.UnixMilli(5000)
	record, err := callRecord("rt", harvest.UsageSourceRef{Key: "k", Session: "s", Born: time.UnixMilli(10)}, "r",
		harvest.UsageCall{ID: "c", At: at, Parts: []harvest.TokenPart{{ID: "z", Of: "cache-write", Count: 1},
			{ID: "a", Of: "cache-write", Count: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if record.FirstAtMS != 5000 || *record.SourceBornMS != 10 || record.SessionID != "s" ||
		record.Parts != `[{"id":"a","label":"","of":"cache-write","count":2},{"id":"z","label":"","of":"cache-write","count":1}]` {
		t.Fatalf("record: %+v parts=%s", record, record.Parts)
	}
}

// A role learned while reading is stored and never erased by a later read or
// listing that states none; a role the listing states for an up-to-date
// source is stored without reading it, before any read (plan S-1).
func TestRolesAreStoredWithoutAReadAndNeverErased(t *testing.T) {
	ix := openStore(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{reader: "fake/usage-1", roles: map[string]string{"kid": "guardian"},
		refs: []harvest.UsageSourceRef{
			{Key: "kid", Session: "kid", Marker: "1", Size: 10, Modified: now},
			{Key: "sub", Session: "parent", Marker: "1", Size: 10, Modified: now},
		}}
	recorder := New(map[string]harvest.UsageSource{"fake": source}, ix)
	role := func(key string) string {
		t.Helper()
		states, err := ix.UsageSourceStates("fake")
		if err != nil {
			t.Fatal(err)
		}
		return states[key].Role
	}
	if _, err := recorder.Pass(context.Background(), 1000); err != nil || role("kid") != "guardian" {
		t.Fatalf("a read's role is stored: %q err=%v", role("kid"), err)
	}
	for range 2 {
		if _, err := recorder.Pass(context.Background(), 1000); err != nil || role("kid") != "guardian" {
			t.Fatalf("an unchanged source keeps its role: %q err=%v", role("kid"), err)
		}
	}
	// A re-read that learns no role (a malformed cursor state) keeps it too.
	source.roles = nil
	source.refs[0].Marker = "2"
	if _, err := recorder.Pass(context.Background(), 1000); err != nil || role("kid") != "guardian" {
		t.Fatalf("an empty read role never overwrites: %q err=%v", role("kid"), err)
	}
	// The listing now states a role for an up-to-date source: stored, unread.
	source.refs[1].Role = "Explore"
	source.reads = nil
	result, err := recorder.Pass(context.Background(), 0)
	if err != nil || result.Roles != 1 || len(source.reads) != 0 || role("sub") != "Explore" {
		t.Fatalf("metadata write: %+v reads=%v role=%q err=%v", result, source.reads, role("sub"), err)
	}
}
