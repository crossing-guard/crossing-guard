package store

import (
	"path/filepath"
	"testing"
)

// Un-adopt turns off the places an adoption governs and no others (team
// rest-of-release plan §4.1 decision 5, criteria 53 and 55).
func TestDisableAdoptionPlacesTurnsOffOnlyThatAdoptionsPlaces(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	put := func(id, root string) ManagedBinding {
		t.Helper()
		binding := testManagedBinding()
		binding.BindingID, binding.ProjectRoot = id, root
		saved, err := ix.PutManagedBinding(binding, ManagedBindingAbsentToken(id), 1)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	mark := func(table, id, key string) {
		t.Helper()
		if _, err := ix.db.Exec(`UPDATE `+table+` SET adoption_key=? WHERE binding_id=?`, key, id); err != nil {
			t.Fatal(err)
		}
	}
	acme := AdoptionKey("org_acme", "organization")
	other := AdoptionKey("org_acme", "repository:repo_1")
	put("place-shared", "/repo/a")
	put("place-other-scope", "/repo/b")
	own := put("place-own", "/repo/c")
	mark("orchestration_managed_binding", "place-shared", acme)
	mark("orchestration_managed_binding", "place-other-scope", other)
	review, err := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 1)
	if err != nil {
		t.Fatal(err)
	}
	mark("orchestration_review_binding", review.BindingID, acme)

	if off, err := ix.DisableAdoptionPlaces("", 2); err != nil || off.Count() != 0 {
		t.Fatalf("an empty key turns nothing off: %+v %v", off, err)
	}
	off, err := ix.DisableAdoptionPlaces(acme, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(off.Managed) != 1 || off.Managed[0] != "place-shared" || !off.Review || off.Count() != 2 {
		t.Fatalf("off = %+v", off)
	}
	shared, _, _ := ix.ManagedBinding("place-shared")
	if shared.State != "disabled" || shared.StateToken != ManagedBindingStateToken(shared) {
		t.Fatalf("the adoption's place must be off with a valid token: %+v", shared)
	}
	if scoped, _, _ := ix.ManagedBinding("place-other-scope"); scoped.State != "enabled" {
		t.Fatalf("another scope's place must stay on: %+v", scoped)
	}
	// The member's or lead's own place is byte-identical: same token, same time.
	if kept, _, _ := ix.ManagedBinding("place-own"); kept.State != "enabled" || kept.StateToken != own.StateToken || kept.UpdatedAt != own.UpdatedAt {
		t.Fatalf("a place with no adoption key must be untouched: %+v", kept)
	}
	if after, _, _ := ix.ReviewBinding(); after.State != "disabled" {
		t.Fatalf("the adoption's review place must be off: %+v", after)
	}
	// A second call finds nothing enabled: adopting again never turns a place back on,
	// and un-adopting again changes nothing.
	if again, err := ix.DisableAdoptionPlaces(acme, 3); err != nil || again.Count() != 0 {
		t.Fatalf("again = %+v %v", again, err)
	}
}
