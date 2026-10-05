package modelroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testOwner(t *testing.T) *Owner {
	t.Helper()
	owner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func runtimeDraft(name, runtime, model string) Draft {
	return Draft{Name: name, Family: FamilyRuntimeModel, Fields: Fields{Runtime: runtime, Model: model}}
}

// mustSelect previews then selects a draft, the way every caller must.
func mustSelect(t *testing.T, owner *Owner, draft Draft) Route {
	t.Helper()
	preview, err := owner.Preview(draft)
	if err != nil {
		t.Fatalf("preview %q: %v", draft.Name, err)
	}
	result, err := owner.Select(SelectCommand{Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest,
		ExpectedStateToken: preview.StateToken, Confirmed: true})
	if err != nil {
		t.Fatalf("select %q: %v", draft.Name, err)
	}
	return result.Route
}

func TestDefaultConfigDocumentIsValid(t *testing.T) {
	if err := DefaultConfig().validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewWritesNothing(t *testing.T) {
	owner := testOwner(t)
	if _, err := owner.Preview(runtimeDraft("Fast", "alpha", "model-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(owner.DataDir(), "models")); !os.IsNotExist(err) {
		t.Fatalf("preview created storage: %v", err)
	}
}

func TestCreateLayoutAndGet(t *testing.T) {
	owner := testOwner(t)
	route := mustSelect(t, owner, runtimeDraft("  Fast   one ", "alpha", "model-a"))
	if !ValidID(route.RouteID) || !strings.HasPrefix(route.RouteID, "rte_") {
		t.Fatalf("route id %q is not rte_<ulid>", route.RouteID)
	}
	if route.Name != "Fast one" || route.Kind != KindRuntimeModel {
		t.Fatalf("route = %+v", route)
	}
	for _, path := range []string{"mutation.lock", filepath.Join("selections", route.RouteID+".json"),
		filepath.Join("documents", digestKey(route.RevisionDigest), "route.json")} {
		if _, err := os.Lstat(filepath.Join(owner.Root(), path)); err != nil {
			t.Errorf("layout lacks %s: %v", path, err)
		}
	}
	read, err := owner.Get(route.RouteID)
	if err != nil || read.RevisionDigest != route.RevisionDigest || read.StateToken != route.StateToken {
		t.Fatalf("get = %+v, %v", read, err)
	}
	raw, err := os.ReadFile(filepath.Join(owner.Root(), "documents", digestKey(route.RevisionDigest), "route.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"format_version"`, `"route_id"`, `"name"`, `"family"`, `"kind"`, `"fields"`, `"created_at"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("revision document lacks %s", key)
		}
	}
}

func TestSelectRequiresPreviewDigestTokenAndConfirmation(t *testing.T) {
	owner := testOwner(t)
	draft := runtimeDraft("Fast", "alpha", "model-a")
	preview, err := owner.Preview(draft)
	if err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]SelectCommand{
		"unconfirmed":  {Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest, ExpectedStateToken: preview.StateToken},
		"wrong digest": {Draft: draft, ExpectedPreviewDigest: strings.Replace(preview.PreviewDigest, "sha256-v1:", "sha256-v1:0", 1)[:len(preview.PreviewDigest)], ExpectedStateToken: preview.StateToken, Confirmed: true},
		"wrong token":  {Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest, ExpectedStateToken: "sha256-v1:stale", Confirmed: true},
		"no token":     {Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest, Confirmed: true},
	} {
		if _, err := owner.Select(command); err == nil {
			t.Errorf("%s: select succeeded", name)
		}
	}
	listed, err := owner.List()
	if err != nil || len(listed.Routes) != 0 {
		t.Fatalf("a refused select left a route: %+v, %v", listed, err)
	}
}

// Criterion 82: two routes cannot share a name after trimming and case folding; the
// duplicate is refused by name; rename keeps the id.
func TestNamesAreUniqueAfterTrimAndCaseFold(t *testing.T) {
	owner := testOwner(t)
	first := mustSelect(t, owner, runtimeDraft("Fast Model", "alpha", "model-a"))
	for _, clash := range []string{"fast model", "  FAST   MODEL  ", "Fast Model"} {
		_, err := owner.Preview(runtimeDraft(clash, "beta", "model-b"))
		if !IsCode(err, CodeNameTaken) || !strings.Contains(err.Error(), "Fast Model") {
			t.Errorf("%q: duplicate was not refused by name: %v", clash, err)
		}
	}
	second := mustSelect(t, owner, runtimeDraft("Slow Model", "beta", "model-b"))
	rename := Draft{RouteID: second.RouteID, Name: "FAST model", Family: second.Family, Fields: second.Fields}
	if _, err := owner.Preview(rename); !IsCode(err, CodeNameTaken) {
		t.Fatalf("rename onto a taken name was not refused: %v", err)
	}
	renamed := mustSelect(t, owner, Draft{RouteID: first.RouteID, Name: "Quick Model", Family: first.Family, Fields: first.Fields})
	if renamed.RouteID != first.RouteID || renamed.Name != "Quick Model" || renamed.RevisionDigest == first.RevisionDigest {
		t.Fatalf("rename must be a new revision of the same id: %+v", renamed)
	}
	if len(renamed.History) != 1 || renamed.History[0] != first.RevisionDigest {
		t.Fatalf("rename history = %v", renamed.History)
	}
	// A route may keep its own name across an edit.
	if _, err := owner.Preview(Draft{RouteID: renamed.RouteID, Name: "quick model", Family: renamed.Family, Fields: renamed.Fields}); err != nil {
		t.Fatalf("a route clashed with itself: %v", err)
	}
}

func TestEditRequiresCurrentStateTokenAndKeepsFamily(t *testing.T) {
	owner := testOwner(t)
	route := mustSelect(t, owner, runtimeDraft("Fast", "alpha", "model-a"))
	edit := Draft{RouteID: route.RouteID, Name: "Fast", Family: FamilyRuntimeModel, Fields: Fields{Runtime: "alpha", Model: "model-b"}}
	preview, err := owner.Preview(edit)
	if err != nil || !preview.Changed || !preview.Exists {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	edited := mustSelect(t, owner, edit)
	if edited.Fields.Model != "model-b" || edited.StateToken == route.StateToken {
		t.Fatalf("edit = %+v", edited)
	}
	// The first preview's token is stale now.
	if _, err := owner.Select(SelectCommand{Draft: edit, ExpectedPreviewDigest: preview.PreviewDigest,
		ExpectedStateToken: preview.StateToken, Confirmed: true}); !IsCode(err, CodeStateConflict) {
		t.Fatalf("a stale state token was accepted: %v", err)
	}
	if _, err := owner.Preview(Draft{RouteID: route.RouteID, Name: "Fast", Family: FamilyInference,
		Fields: Fields{Endpoint: "http://127.0.0.1:11434", Model: "m"}}); !IsCode(err, CodeInvalid) {
		t.Fatalf("a family change was accepted: %v", err)
	}
	same, err := owner.Preview(Draft{RouteID: edited.RouteID, Name: edited.Name, Family: edited.Family, Fields: edited.Fields})
	if err != nil || same.Changed {
		t.Fatalf("an unchanged draft previews as changed: %+v, %v", same, err)
	}
}

func TestInferenceRouteIsLoopbackOnlyAndCanonical(t *testing.T) {
	owner := testOwner(t)
	route := mustSelect(t, owner, Draft{Name: "Reviewer", Family: FamilyInference,
		Fields: Fields{Endpoint: "http://127.0.0.1:11434/", Model: "review-model"}})
	if route.Kind != KindLocalOllama || route.Fields.Endpoint != "http://127.0.0.1:11434" {
		t.Fatalf("inference route = %+v", route)
	}
	for _, endpoint := range []string{"http://example.com:11434", "https://127.0.0.1:11434", "http://localhost:11434", "http://user:pw@127.0.0.1:11434"} {
		if _, err := owner.Preview(Draft{Name: "Remote", Family: FamilyInference,
			Fields: Fields{Endpoint: endpoint, Model: "m"}}); !IsCode(err, CodeInvalid) {
			t.Errorf("endpoint %s was accepted: %v", endpoint, err)
		}
	}
}

// No credential is ever in a route: the stored documents decode strictly, so a field
// the two families do not have cannot be written or read back.
func TestRevisionHasNoFieldForACredential(t *testing.T) {
	var fields Fields
	for _, body := range []string{`{"model":"m","api_key":"x"}`, `{"model":"m","token":"x"}`, `{"model":"m","authorization":"x"}`} {
		if err := decodeStrictJSON([]byte(body), &fields); err == nil {
			t.Errorf("fields accepted %s", body)
		}
	}
}

func TestDeleteIsRefusedWhileReferencedAndNamesThePlaces(t *testing.T) {
	owner := testOwner(t)
	route := mustSelect(t, owner, runtimeDraft("Fast", "alpha", "model-a"))
	err := owner.Delete(route.RouteID, route.StateToken, func() ([]string, error) {
		return []string{"helper in repo-one", "the reviewer"}, nil
	})
	if !IsCode(err, CodeInUse) || !strings.Contains(err.Error(), "helper in repo-one") || !strings.Contains(err.Error(), "the reviewer") {
		t.Fatalf("delete of a used route: %v", err)
	}
	if _, getErr := owner.Get(route.RouteID); getErr != nil {
		t.Fatalf("a refused delete removed the route: %v", getErr)
	}
	if err := owner.Delete(route.RouteID, "sha256-v1:stale", nil); !IsCode(err, CodeStateConflict) {
		t.Fatalf("delete with a stale token: %v", err)
	}
	if err := owner.Delete(route.RouteID, route.StateToken, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, getErr := owner.Get(route.RouteID); !IsCode(getErr, CodeNotFound) {
		t.Fatalf("deleted route still reads: %v", getErr)
	}
}

func TestTamperedDocumentIsAnIntegrityConflictNotARoute(t *testing.T) {
	owner := testOwner(t)
	route := mustSelect(t, owner, runtimeDraft("Fast", "alpha", "model-a"))
	path := filepath.Join(owner.Root(), "documents", digestKey(route.RevisionDigest), "route.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "model-a", "model-z", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Get(route.RouteID); !IsCode(err, CodeIntegrity) {
		t.Fatalf("tampered document read as a route: %v", err)
	}
	listed, err := owner.List()
	if err != nil || len(listed.Routes) != 0 || len(listed.Problems) != 1 {
		t.Fatalf("list = %+v, %v", listed, err)
	}
}

func TestFindByFieldsAndMigratedMark(t *testing.T) {
	owner := testOwner(t)
	draft := runtimeDraft("Alpha · model-a", "alpha", "model-a")
	preview, err := owner.Preview(draft)
	if err != nil {
		t.Fatal(err)
	}
	created, err := owner.Select(SelectCommand{Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest,
		ExpectedStateToken: preview.StateToken, Confirmed: true, Migrated: true})
	if err != nil || created.Route.MigratedAt == "" || !created.Created {
		t.Fatalf("migrated create = %+v, %v", created, err)
	}
	found, ok, err := owner.FindByFields(FamilyRuntimeModel, Fields{Runtime: "alpha", Model: "model-a"})
	if err != nil || !ok || found.RouteID != created.Route.RouteID {
		t.Fatalf("find = %+v %v %v", found, ok, err)
	}
	effort := &Effort{Kind: "level", Value: "high"}
	if _, ok, _ := owner.FindByFields(FamilyRuntimeModel, Fields{Runtime: "alpha", Model: "model-a", ThinkingEffort: effort}); ok {
		t.Fatal("a route with another effort matched")
	}
}

func TestHistoryIsBoundedByConfig(t *testing.T) {
	owner := testOwner(t)
	if err := os.MkdirAll(filepath.Join(owner.DataDir(), "models"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"format_version":"crossing-guard-model-routes-config-v1","history_max":2,"name_max_runes":80}`
	if err := os.WriteFile(filepath.Join(owner.DataDir(), "models", "routes.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	route := mustSelect(t, owner, runtimeDraft("Fast", "alpha", "model-0"))
	for _, model := range []string{"model-1", "model-2", "model-3", "model-4"} {
		route = mustSelect(t, owner, Draft{RouteID: route.RouteID, Name: "Fast", Family: FamilyRuntimeModel,
			Fields: Fields{Runtime: "alpha", Model: model}})
	}
	if len(route.History) != 2 {
		t.Fatalf("history = %d entries, want the configured 2", len(route.History))
	}
}
