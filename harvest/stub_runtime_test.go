package harvest

import "testing"

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
func (stubRuntime) Summarize(fileJob) (SessionSummary, *SessionUsage, map[string]*DayBucket, bool) {
	return SessionSummary{}, nil, nil, false
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
