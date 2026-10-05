package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func routeTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func putRouteBinding(t *testing.T, ix *Index, id, routeID string, chain ...ManagedRoute) ManagedBinding {
	t.Helper()
	binding := testManagedBinding()
	binding.BindingID, binding.Runtime, binding.Model = id, "alpha", "model-a"
	binding.RouteID, binding.RouteRevisionDigest, binding.Routes = routeID, "sha256-v1:one", chain
	saved, err := ix.PutManagedBinding(binding, ManagedBindingAbsentToken(id), 1)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// testReviewPath stands in for the reviewer host's request-path builder: an identity
// that depends on exactly what the real one depends on — endpoint, model, limits.
func testReviewPath(binding ReviewBinding) (string, string, error) {
	return "local-ollama-v1", "sha256-v1:path:" + binding.Endpoint + "|" + binding.Model + "|" +
		string(rune('0'+binding.MaxConcurrency)), nil
}

// Criterion 57: a route edit moves every place that uses it in one step. Criterion 81's
// fail half: a sheet holding the old state token is refused afterwards.
func TestApplyRouteRevisionMovesEveryReferenceAndRetokens(t *testing.T) {
	ix := routeTestIndex(t)
	primary := putRouteBinding(t, ix, "uses-primary", "rte_A")
	chained := putRouteBinding(t, ix, "uses-chain", "rte_B",
		ManagedRoute{RouteID: "rte_A", Runtime: "alpha", Model: "model-a", Mode: "read"},
		ManagedRoute{RouteID: "rte_C", Runtime: "gamma", Model: "model-c", Mode: "read"})
	other := putRouteBinding(t, ix, "unrelated", "rte_B")

	effort := &ThinkingEffort{Kind: "level", Value: "high"}
	revision := RouteRevision{RouteID: "rte_A", RevisionDigest: "sha256-v1:two",
		Managed: &ManagedRouteCopy{Runtime: "beta", Model: "model-b", ThinkingEffort: effort}}
	applied, err := ix.ApplyRouteRevision(revision, nil, 5)
	if err != nil || len(applied.ManagedBindingIDs) != 2 {
		t.Fatalf("applied = %+v, %v", applied, err)
	}
	movedPrimary, _, _ := ix.ManagedBinding("uses-primary")
	if movedPrimary.Runtime != "beta" || movedPrimary.Model != "model-b" || movedPrimary.ThinkingEffort == nil ||
		*movedPrimary.ThinkingEffort != *effort || movedPrimary.RouteRevisionDigest != "sha256-v1:two" ||
		movedPrimary.StateToken == primary.StateToken || movedPrimary.RouteID != "rte_A" {
		t.Fatalf("primary reference did not follow the route: %+v", movedPrimary)
	}
	movedChain, _, _ := ix.ManagedBinding("uses-chain")
	if movedChain.Runtime != "alpha" || movedChain.Routes[0].Runtime != "beta" || movedChain.Routes[0].Model != "model-b" ||
		movedChain.Routes[0].Mode != "read" || movedChain.Routes[1].Runtime != "gamma" || movedChain.StateToken == chained.StateToken {
		t.Fatalf("chain entry did not follow the route, or its mode or neighbour changed: %+v", movedChain)
	}
	untouched, _, _ := ix.ManagedBinding("unrelated")
	if untouched.StateToken != other.StateToken || untouched.UpdatedAt != other.UpdatedAt {
		t.Fatalf("a binding that does not reference the route was rewritten: %+v", untouched)
	}
	// The old token no longer writes.
	stale := primary
	stale.Priority = 7
	if _, err := ix.PutManagedBinding(stale, primary.StateToken, 6); !errors.Is(err, ErrManagedBindingConflict) {
		t.Fatalf("a write holding the pre-edit state token was accepted: %v", err)
	}
	// Idempotent: the same revision again changes nothing and mints no token.
	again, err := ix.ApplyRouteRevision(revision, nil, 9)
	if err != nil || again.Changed() {
		t.Fatalf("second apply = %+v, %v", again, err)
	}
	same, _, _ := ix.ManagedBinding("uses-primary")
	if same.StateToken != movedPrimary.StateToken || same.UpdatedAt != movedPrimary.UpdatedAt {
		t.Fatalf("an idempotent apply rewrote the binding: %+v", same)
	}
}

// §5.3: a route edit recomputes the review binding's request-path kind and digest from
// the new endpoint and model and that binding's own limits.
func TestApplyRouteRevisionRecomputesReviewRequestPath(t *testing.T) {
	ix := routeTestIndex(t)
	binding := testReviewBinding()
	binding.RouteID, binding.RouteRevisionDigest = "rte_R", "sha256-v1:one"
	binding.RequestPathKind, binding.RequestPathDigest, _ = testReviewPath(binding)
	saved, err := ix.PutReviewBinding(binding, ReviewBindingAbsentToken(), 1)
	if err != nil {
		t.Fatal(err)
	}
	revision := RouteRevision{RouteID: "rte_R", RevisionDigest: "sha256-v1:two",
		Review: &ReviewRouteCopy{Endpoint: "http://127.0.0.1:22222", Model: "other-model"}}
	applied, err := ix.ApplyRouteRevision(revision, testReviewPath, 5)
	if err != nil || !applied.ReviewChanged {
		t.Fatalf("applied = %+v, %v", applied, err)
	}
	moved, _, _ := ix.ReviewBinding()
	_, wantDigest, _ := testReviewPath(moved)
	if moved.Endpoint != "http://127.0.0.1:22222" || moved.Model != "other-model" || moved.RequestPathDigest != wantDigest ||
		moved.RequestPathDigest == saved.RequestPathDigest || moved.RouteRevisionDigest != "sha256-v1:two" {
		t.Fatalf("request-path identity was not recomputed from the new endpoint and model: %+v", moved)
	}
	if moved.StateToken == saved.StateToken || moved.StateToken != ReviewBindingStateToken(moved) {
		t.Fatalf("review binding kept its state token: %+v", moved)
	}
	if moved.TimeoutMS != saved.TimeoutMS || moved.MaxTokens != saved.MaxTokens || moved.ProfileID != saved.ProfileID {
		t.Fatalf("a route edit changed the binding's own settings: %+v", moved)
	}
	if _, err := ix.DisableReviewBinding(saved.StateToken, 6); !errors.Is(err, ErrReviewBindingConflict) {
		t.Fatalf("the pre-edit state token still writes: %v", err)
	}
	again, err := ix.ApplyRouteRevision(revision, testReviewPath, 9)
	if err != nil || again.Changed() {
		t.Fatalf("second apply = %+v, %v", again, err)
	}
	// A revision for another route leaves the reviewer alone.
	elsewhere := revision
	elsewhere.RouteID = "rte_X"
	if applied, err := ix.ApplyRouteRevision(elsewhere, testReviewPath, 10); err != nil || applied.Changed() {
		t.Fatalf("unrelated route moved the reviewer: %+v, %v", applied, err)
	}
	// With a reference to recompute, the identity function is required: a binding is
	// never written with an identity nobody derived.
	newer := revision
	newer.RevisionDigest = "sha256-v1:three"
	if _, err := ix.ApplyRouteRevision(newer, nil, 11); err == nil {
		t.Fatal("a review revision was applied with no request-path identity function")
	}
}

func TestApplyRouteRevisionRefusesAShapelessRevision(t *testing.T) {
	ix := routeTestIndex(t)
	for _, revision := range []RouteRevision{
		{RouteID: "rte_A", RevisionDigest: "d"},
		{RouteID: "rte_A", RevisionDigest: "d", Managed: &ManagedRouteCopy{}, Review: &ReviewRouteCopy{}},
		{RevisionDigest: "d", Managed: &ManagedRouteCopy{}},
	} {
		if _, err := ix.ApplyRouteRevision(revision, testReviewPath, 1); !errors.Is(err, ErrRouteRevisionShape) {
			t.Errorf("revision %+v: %v", revision, err)
		}
	}
}

// §5.6: the migration's reference write is a compare-and-swap that leaves the resolved
// copy exactly as the binding ran with.
func TestAttachManagedRoutesIsACompareAndSwapOnUnchangedFields(t *testing.T) {
	ix := routeTestIndex(t)
	legacy := putRouteBinding(t, ix, "legacy", "", ManagedRoute{Runtime: "gamma", Model: "model-c", Mode: "read"})
	primary := &RouteRevision{RouteID: "rte_A", RevisionDigest: "sha256-v1:a", Managed: &ManagedRouteCopy{Runtime: "alpha", Model: "model-a"}}
	chain := map[int]RouteRevision{0: {RouteID: "rte_C", RevisionDigest: "sha256-v1:c", Managed: &ManagedRouteCopy{Runtime: "gamma", Model: "model-c"}}}

	wrong := &RouteRevision{RouteID: "rte_Z", RevisionDigest: "sha256-v1:z", Managed: &ManagedRouteCopy{Runtime: "alpha", Model: "another"}}
	if _, err := ix.AttachManagedRoutes("legacy", legacy.StateToken, wrong, nil, 2); !errors.Is(err, ErrManagedBindingConflict) {
		t.Fatalf("a route with other fields was attached: %v", err)
	}
	if _, err := ix.AttachManagedRoutes("legacy", "sha256-v1:stale", primary, chain, 2); !errors.Is(err, ErrManagedBindingConflict) {
		t.Fatalf("a stale token attached routes: %v", err)
	}
	attached, err := ix.AttachManagedRoutes("legacy", legacy.StateToken, primary, chain, 3)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, _ := ix.ManagedBinding("legacy")
	if stored.RouteID != "rte_A" || stored.RouteRevisionDigest != "sha256-v1:a" || stored.Routes[0].RouteID != "rte_C" ||
		stored.Runtime != legacy.Runtime || stored.Model != legacy.Model || stored.Routes[0].Mode != "read" ||
		stored.StateToken != attached.StateToken || stored.StateToken == legacy.StateToken {
		t.Fatalf("attached binding = %+v", stored)
	}
	if _, err := ix.AttachManagedRoutes("legacy", stored.StateToken, primary, nil, 4); !errors.Is(err, ErrManagedBindingConflict) {
		t.Fatalf("a binding that already has a route was re-attached: %v", err)
	}
	references, err := ix.RouteReferences()
	if err != nil || len(references) != 2 || !references[1].Fallback || references[1].Position != 1 {
		t.Fatalf("references = %+v, %v", references, err)
	}
}

func TestAttachReviewRouteRecomputesIdentityAndRouteProblemIsIdempotent(t *testing.T) {
	ix := routeTestIndex(t)
	saved, err := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 1)
	if err != nil {
		t.Fatal(err)
	}
	revision := RouteRevision{RouteID: "rte_R", RevisionDigest: "sha256-v1:r",
		Review: &ReviewRouteCopy{Endpoint: saved.Endpoint, Model: saved.Model}}
	attached, err := ix.AttachReviewRoute(saved.StateToken, revision, testReviewPath, 2)
	_, wantDigest, _ := testReviewPath(attached)
	if err != nil || attached.RouteID != "rte_R" || attached.RequestPathDigest != wantDigest {
		t.Fatalf("attached = %+v, %v", attached, err)
	}
	stored, _, _ := ix.ReviewBinding()
	if stored.StateToken != attached.StateToken {
		t.Fatalf("returned token %s is not the stored one %s", attached.StateToken, stored.StateToken)
	}
	changed, err := ix.SetReviewRouteProblem(RouteProblemMissing, 3)
	if err != nil || !changed {
		t.Fatalf("set problem: %v %v", changed, err)
	}
	if changed, err := ix.SetReviewRouteProblem(RouteProblemMissing, 4); err != nil || changed {
		t.Fatalf("setting the same problem twice rewrote the row: %v %v", changed, err)
	}
	marked, _, _ := ix.ReviewBinding()
	if marked.RouteProblem != RouteProblemMissing || marked.StateToken == stored.StateToken {
		t.Fatalf("marked = %+v", marked)
	}
	// The route coming back clears the mark.
	if applied, err := ix.ApplyRouteRevision(revision, testReviewPath, 5); err != nil || !applied.ReviewChanged {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	cleared, _, _ := ix.ReviewBinding()
	if cleared.RouteProblem != "" {
		t.Fatalf("route problem survived the route's return: %+v", cleared)
	}
}

func TestSetManagedRouteProblemIsIdempotentAndMigrationFailedKeepsValues(t *testing.T) {
	ix := routeTestIndex(t)
	legacy := putRouteBinding(t, ix, "legacy", "")
	changed, err := ix.SetManagedRouteProblem("legacy", RouteProblemMigrationFailed, 2)
	if err != nil || !changed {
		t.Fatalf("set: %v %v", changed, err)
	}
	marked, _, _ := ix.ManagedBinding("legacy")
	if marked.RouteProblem != RouteProblemMigrationFailed || marked.Runtime != legacy.Runtime ||
		marked.Model != legacy.Model || marked.State != "enabled" || marked.RouteID != "" {
		t.Fatalf("a failed migration changed more than the problem: %+v", marked)
	}
	if changed, err := ix.SetManagedRouteProblem("legacy", RouteProblemMigrationFailed, 3); err != nil || changed {
		t.Fatalf("second set rewrote the row: %v %v", changed, err)
	}
	if _, err := ix.SetManagedRouteProblem("legacy", "not-a-problem", 4); err == nil {
		t.Fatal("the store accepted a route problem outside its vocabulary")
	}
}
