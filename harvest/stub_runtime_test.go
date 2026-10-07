package harvest

import (
	"errors"
	"testing"
)

// stubRuntime is a fake vendor registered ONLY from this test. It is the
// executable proof of ADR 0020's core claim: adding a vendor is closed to
// modification. Nothing in any generic file is edited — this new type + its
// registration are all it takes for the generic dispatchers (RuntimeNames,
// CanonicalID, MatchID) to route to it. If someone reintroduces a
// `switch runtime { case "claude": ... }`, this test still passes but the
// vendor-lint (scripts/vendor-lint.sh) catches the regression.
type stubRuntime struct{}

func (stubRuntime) Name() string                             { return "stubvendor" }
func (stubRuntime) CanonicalID(s SessionSummary) string      { return "stub:" + s.ID }
func (stubRuntime) MatchID(s SessionSummary, id string) bool { return s.ID == id }
func (stubRuntime) Collect() []fileJob                       { return nil }
func (stubRuntime) Summarize(fileJob) (SessionSummary, bool) {
	return SessionSummary{}, false
}
func (stubRuntime) Normalize(string) ([]CanonicalEvent, int, *SessionUsage, error) {
	return nil, 0, nil, nil
}
func (stubRuntime) ThreadTitle(SessionSummary) string { return "" }

func TestAddingAVendorIsClosedToModification(t *testing.T) {
	register(stubRuntime{})
	defer delete(runtimes, "stubvendor")

	// the registry surfaces it — this is what replaced []string{"claude","codex"}
	var found bool
	for _, n := range RuntimeNames() {
		if n == "stubvendor" {
			found = true
		}
	}
	if !found {
		t.Fatal("RuntimeNames did not surface the newly-registered vendor")
	}

	// generic identity dispatch routes to it with no switch to edit
	s := SessionSummary{Runtime: "stubvendor", ID: "abc"}
	if got := CanonicalID(s); got != "stub:abc" {
		t.Fatalf("CanonicalID did not dispatch to the new vendor: got %q", got)
	}
	if !MatchID(s, "abc") {
		t.Fatal("MatchID did not dispatch to the new vendor")
	}
}

// failingStoreRuntime is a store-backed vendor (a SessionSource) whose store
// cannot be listed, registered only from this test.
type failingStoreRuntime struct{ stubRuntime }

func (failingStoreRuntime) Name() string { return "stubstore" }
func (failingStoreRuntime) ListSessionRecords() ([]SessionRecord, error) {
	return nil, errors.New("store is locked")
}
func (failingStoreRuntime) NormalizeSession(SessionRef, bool) ([]CanonicalEvent, int, *SessionUsage, error) {
	return nil, 0, nil, nil
}

// An unreadable session store is reported, never turned into "no match":
// FindAllChecked returns the listing error, and FindAll keeps its
// error-free shape for the callers that only want matches.
func TestFindAllCheckedReportsAnUnreadableStore(t *testing.T) {
	register(failingStoreRuntime{})
	defer delete(runtimes, "stubstore")
	matches, err := FindAllChecked("stubstore", "ses-any")
	if err == nil || err.Error() != "store is locked" || len(matches) != 0 {
		t.Fatalf("the listing error must surface: matches=%v err=%v", matches, err)
	}
	if got := FindAll("stubstore", "ses-any"); len(got) != 0 {
		t.Fatalf("FindAll still answers no match: %v", got)
	}
}
