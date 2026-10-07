package daemon

// HTTP coverage for the Agents pages (agents-settings-redesign plan §12): the
// roster read model, section-patch batches that never overwrite a place's
// other settings, state-only turn-off of a broken place, the runs page,
// drafts from blank to published, and the facts the editors read.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

type rosterFixture struct {
	root, second string
	owner        *profilefs.Owner
	ix           *store.Index
	host         *orchestrationManagedHost
	mux          *http.ServeMux
	preview      profilefs.Preview
}

func newRosterFixture(t *testing.T) rosterFixture {
	t.Helper()
	root := t.TempDir()
	second := filepath.Join(root, "second-repo")
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	preview := selectManagedProfile(t, owner, helperAgentProfileSource())
	ix, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(ix, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = ix.Close() })
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"managed-fixture": managedFixtureDriver{}}
	t.Cleanup(func() { chatDrivers = original })
	mux := http.NewServeMux()
	pinned := func(id string) profilefs.PinSource {
		return func() ([]profilefs.RevisionRef, error) { return pinnedRevisions(host.ix, id) }
	}
	registerOrchestrationManagedRoutes(mux, host, owner)
	registerOrchestrationProfileRoutes(mux, owner, pinned)
	registerOrchestrationDraftRoutes(mux, owner, pinned)
	registerOrchestrationRosterRoutes(mux, rosterSources{profiles: owner, managed: host})
	return rosterFixture{root: root, second: second, owner: owner, ix: ix, host: host, mux: mux, preview: preview}
}

func (fixture rosterFixture) call(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	response := httptest.NewRecorder()
	fixture.mux.ServeHTTP(response, httptest.NewRequest(method, path, reader))
	return response.Code, response.Body.Bytes()
}

func (fixture rosterFixture) place(model string, projectRoot string) map[string]any {
	return map[string]any{"profile_id": fixture.preview.ProfileID, "profile_source_digest": fixture.preview.SourceDigest,
		"profile_bundle_digest": fixture.preview.BundleDigest, "project_root": projectRoot,
		"route_id": testRouteID(fixture.host, "managed-fixture", model, nil), "mode": "", "granted_authority": []string{"reply"}, "declared_tags": []string{"needs-review"}}
}

func (fixture rosterFixture) binding(t *testing.T, id string) store.ManagedBinding {
	t.Helper()
	binding, found, err := fixture.ix.ManagedBinding(id)
	if err != nil || !found {
		t.Fatalf("binding %s: %v %v", id, found, err)
	}
	return binding
}

// twoPlaces creates the agent in two repositories with different models.
func (fixture rosterFixture) twoPlaces(t *testing.T) (string, string) {
	t.Helper()
	first := fixture.place("model-a", fixture.root)
	first["expected_state_token"] = store.ManagedBindingAbsentToken("agent-x")
	first["confirmed"] = true
	if status, body := fixture.call(t, http.MethodPut, "/api/orchestration/agents/agent-x", first); status != http.StatusOK {
		t.Fatalf("first place: %d %s", status, body)
	}
	status, body := fixture.call(t, http.MethodPost, "/api/orchestration/agents/binding-id",
		map[string]any{"profile_id": fixture.preview.ProfileID, "project_root": fixture.second})
	if status != http.StatusOK {
		t.Fatalf("binding id: %d %s", status, body)
	}
	var id newBindingIDResponse
	if err := json.Unmarshal(body, &id); err != nil {
		t.Fatal(err)
	}
	if id.BindingID != "design-helper--second-repo" {
		t.Fatalf("derived id = %q", id.BindingID)
	}
	status, body = fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{{"binding_id": id.BindingID, "expected_state_token": id.ExpectedStateToken,
			"op": "create", "place": fixture.place("model-b", fixture.second)}}})
	if status != http.StatusOK {
		t.Fatalf("second place: %d %s", status, body)
	}
	return "agent-x", id.BindingID
}

func TestRosterGroupsPlacesAndBatchPatchesOnlyNamedSections(t *testing.T) {
	fixture := newRosterFixture(t)
	first, second := fixture.twoPlaces(t)
	status, body := fixture.call(t, http.MethodGet, "/api/orchestration/roster", nil)
	if status != http.StatusOK {
		t.Fatalf("roster: %d %s", status, body)
	}
	var roster rosterResponse
	if err := json.Unmarshal(body, &roster); err != nil || roster.Outages == nil {
		t.Fatalf("roster = %v %s (outages must always be a list)", err, body)
	}
	if len(roster.Agents) != 1 || len(roster.Agents[0].Places) != 2 || roster.Agents[0].AgentType != "helper" ||
		roster.Agents[0].Lane != "managed" || !roster.Agents[0].Compatible {
		t.Fatalf("two places must render as one agent row: %+v", roster.Agents)
	}
	one, two := fixture.binding(t, first), fixture.binding(t, second)
	priority := 9
	patch := func(id, token string) map[string]any {
		return map[string]any{"binding_id": id, "expected_state_token": token, "op": "update", "set": map[string]any{"priority": priority}}
	}
	// One stale token: nothing is written.
	status, _ = fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{patch(first, one.StateToken), patch(second, "stale")}})
	if status != http.StatusConflict {
		t.Fatalf("stale batch status = %d", status)
	}
	if fixture.binding(t, first).Priority == 9 {
		t.Fatal("a conflicting batch wrote its first change")
	}
	status, body = fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{patch(first, one.StateToken), patch(second, two.StateToken)}})
	if status != http.StatusOK {
		t.Fatalf("priority batch: %d %s", status, body)
	}
	one, two = fixture.binding(t, first), fixture.binding(t, second)
	if one.Priority != 9 || two.Priority != 9 || one.Model != "model-a" || two.Model != "model-b" ||
		len(one.DeclaredTags) != 1 || one.ProjectRoot != fixture.root || two.ProjectRoot != fixture.second {
		t.Fatalf("a priority edit changed other sections: %+v / %+v", one, two)
	}
	// validate_only reports an invalid change and writes nothing.
	status, body = fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true, "validate_only": true,
		"changes": []map[string]any{{"binding_id": first, "expected_state_token": one.StateToken, "op": "update",
			"set": map[string]any{"permissions": map[string]any{"granted_authority": []string{"launch-profile"}}}}}})
	var validation bindingBatchResponse
	if err := json.Unmarshal(body, &validation); err != nil || status != http.StatusOK || validation.Written ||
		len(validation.Results) != 1 || validation.Results[0].Valid || validation.Results[0].Problem == "" {
		t.Fatalf("validate_only = %d %s", status, body)
	}
	if status, _ := fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{patch(first, one.StateToken), patch(first, one.StateToken)}}); status != http.StatusBadRequest {
		t.Fatalf("duplicate ids status = %d", status)
	}
	// Turn the second place off; a later edit keeps it off.
	status, _ = fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{{"binding_id": second, "expected_state_token": two.StateToken, "op": "disable"}}})
	if status != http.StatusOK {
		t.Fatalf("disable status = %d", status)
	}
	two = fixture.binding(t, second)
	priority = 3
	status, body = fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{patch(second, two.StateToken)}})
	if status != http.StatusOK || fixture.binding(t, second).State != "disabled" || fixture.binding(t, second).Priority != 3 {
		t.Fatalf("an edit re-enabled a place: %d %s %+v", status, body, fixture.binding(t, second))
	}
}

func TestRosterFlagsARevisionGoneAndStillTurnsItOff(t *testing.T) {
	fixture := newRosterFixture(t)
	gone := store.ManagedBinding{BindingID: "agent-gone", State: "enabled", Role: "helper", ProjectRoot: fixture.root,
		ProfileID: fixture.preview.ProfileID, ProfileSourceDigest: "sha256-v1:" + strings.Repeat("a", 64),
		ProfileBundleDigest: "sha256-v1:" + strings.Repeat("b", 64), Runtime: "managed-fixture",
		Authority: []string{}, AllowedProfiles: []store.ManagedProfileRef{}}
	saved, err := fixture.ix.PutManagedBinding(gone, store.ManagedBindingAbsentToken("agent-gone"), 1)
	if err != nil {
		t.Fatal(err)
	}
	_, body := fixture.call(t, http.MethodGet, "/api/orchestration/roster", nil)
	var roster rosterResponse
	if err := json.Unmarshal(body, &roster); err != nil {
		t.Fatal(err)
	}
	if len(roster.Agents) != 1 || roster.Agents[0].Attention != attentionRevisionUnavailable ||
		roster.Agents[0].Places[0].RevisionAvailable {
		t.Fatalf("revision-gone place = %+v", roster.Agents)
	}
	status, body := fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{{"binding_id": "agent-gone", "expected_state_token": saved.StateToken, "op": "disable"}}})
	if status != http.StatusOK || fixture.binding(t, "agent-gone").State != "disabled" {
		t.Fatalf("turning off a broken place: %d %s", status, body)
	}
}

func TestRosterDetailAndRunsPages(t *testing.T) {
	fixture := newRosterFixture(t)
	first, _ := fixture.twoPlaces(t)
	binding := fixture.binding(t, first)
	group := store.ManagedGroup{GroupID: "grp_roster", BindingID: first, State: "active", RootTaskID: "task_root",
		RootRuntime: "managed-fixture", RootCatalogSessionID: "session-one", ProjectRoot: fixture.root, CreatedAt: 1, UpdatedAt: 1}
	for index := 1; index <= 5; index++ {
		id := "orun_" + string(rune('a'+index))
		run := store.ManagedRun{RunID: id, IdempotencyKey: "idem_" + id, GroupID: group.GroupID, BindingID: first,
			BindingStateToken: binding.StateToken, Role: "helper", ProfileID: binding.ProfileID,
			ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
			SourceTaskID: "task_root", SourceEventID: int64(index), AdmittedAt: int64(1000 + index),
			Citations: []string{}, Detail: map[string]any{"signal": "task.completed"}}
		if _, _, err := fixture.ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 100, MaxActive: 100}); err != nil {
			t.Fatal(err)
		}
		action := "no_action"
		if index%2 == 0 {
			action = "draft_reply"
		}
		if err := fixture.ix.CompleteManagedRun(id, "completed", action, "said "+id, []string{}, run.Detail, "", "", int64(1001+index)); err != nil {
			t.Fatal(err)
		}
	}
	status, body := fixture.call(t, http.MethodGet, "/api/orchestration/roster/"+fixture.preview.ProfileID+"/runs?limit=2", nil)
	var page rosterRunsResponse
	if err := json.Unmarshal(body, &page); err != nil || status != http.StatusOK || len(page.Runs) != 2 || page.Next == "" ||
		page.Runs[0].RunID != "orun_f" || page.Runs[0].Version != "1.0.0" || page.Runs[0].Repository == "" ||
		page.Runs[0].Session.CatalogID != "session-one" {
		t.Fatalf("first runs page = %d %s", status, body)
	}
	_, body = fixture.call(t, http.MethodGet, "/api/orchestration/roster/"+fixture.preview.ProfileID+"/runs?limit=10&before="+page.Next, nil)
	var rest rosterRunsResponse
	if err := json.Unmarshal(body, &rest); err != nil || len(rest.Runs) != 3 || rest.Next != "" || rest.Runs[0].RunID != "orun_d" {
		t.Fatalf("second runs page = %s", body)
	}
	_, body = fixture.call(t, http.MethodGet, "/api/orchestration/roster/"+fixture.preview.ProfileID+"/runs?outcome=acted", nil)
	var acted rosterRunsResponse
	if err := json.Unmarshal(body, &acted); err != nil || len(acted.Runs) != 2 {
		t.Fatalf("acted filter = %s", body)
	}
	if status, _ := fixture.call(t, http.MethodGet, "/api/orchestration/roster/"+fixture.preview.ProfileID+"/runs?before=junk", nil); status != http.StatusBadRequest {
		t.Fatalf("a malformed cursor must be refused, got %d", status)
	}
	status, body = fixture.call(t, http.MethodGet, "/api/orchestration/roster/"+fixture.preview.ProfileID, nil)
	var detail rosterDetailResponse
	if err := json.Unmarshal(body, &detail); err != nil || status != http.StatusOK {
		t.Fatalf("detail = %d %s", status, body)
	}
	if detail.Current == nil || len(detail.History) != 1 || len(detail.History[0].Places) != 2 || detail.Draft != nil ||
		detail.Totals.Runs != 5 || detail.Totals.Acted != 2 || strings.Join(detail.EnforcedLimits, ",") != "max_total,loop_budget" ||
		len(detail.ContextKinds) == 0 || len(detail.ContextAlways) != 1 || detail.ContextAlways[0].Label == "" ||
		detail.DraftToken != profilefs.DraftAbsentToken(fixture.preview.ProfileID) ||
		detail.SelectionToken == "" || detail.ManagedOption == nil {
		t.Fatalf("detail = %s", body)
	}
	if status, _ := fixture.call(t, http.MethodGet, "/api/orchestration/roster/nobody", nil); status != http.StatusNotFound {
		t.Fatalf("an unknown agent must 404, got %d", status)
	}
}

func TestDraftRoutesFromBlankToPublishedAndEditedAgain(t *testing.T) {
	fixture := newRosterFixture(t)
	status, body := fixture.call(t, http.MethodPost, "/api/orchestration/drafts", map[string]any{"start": "blank",
		"id": "decision-recall", "name": "Decision recall", "description": "Recalls decisions.", "type": "helper"})
	var created draftResponse
	if err := json.Unmarshal(body, &created); err != nil || status != http.StatusOK || created.Draft.Problem != nil {
		t.Fatalf("new draft = %d %s", status, body)
	}
	if status, _ := fixture.call(t, http.MethodPost, "/api/orchestration/drafts", map[string]any{"start": "blank",
		"id": "decision-recall", "name": "Again", "description": "d", "type": "helper"}); status != http.StatusBadRequest {
		t.Fatalf("a taken id must be refused, got %d", status)
	}
	_, body = fixture.call(t, http.MethodGet, "/api/orchestration/roster", nil)
	var roster rosterResponse
	if err := json.Unmarshal(body, &roster); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, agent := range roster.Agents {
		if agent.ProfileID == "decision-recall" && agent.DraftOnly && agent.HasDraft && agent.AgentType == "helper" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a never-published draft must be a roster row: %s", body)
	}
	instructions := "Speak only when a recorded decision conflicts with the plan."
	status, body = fixture.call(t, http.MethodPut, "/api/orchestration/profiles/decision-recall/draft", map[string]any{
		"expected_state_token": created.Draft.StateToken, "edit": map[string]any{"instructions": instructions}})
	var edited draftResponse
	if err := json.Unmarshal(body, &edited); err != nil || status != http.StatusOK ||
		edited.Draft.Normalized == nil || !strings.Contains(edited.Draft.Normalized.Instructions, "conflicts with the plan") {
		t.Fatalf("edit = %d %s", status, body)
	}
	status, body = fixture.call(t, http.MethodPost, "/api/orchestration/profiles/decision-recall/draft/publish", map[string]any{
		"expected_draft_token": edited.Draft.StateToken, "expected_selection_token": profilefs.SelectionAbsentToken("decision-recall"), "confirmed": true})
	var published draftPublishResponse
	if err := json.Unmarshal(body, &published); err != nil || status != http.StatusOK || published.Profile.Current.Version != "1.0.0" {
		t.Fatalf("publish = %d %s", status, body)
	}
	// Edits made against a version the author no longer sees are refused, not
	// applied on top of the newer one.
	if status, _ := fixture.call(t, http.MethodPut, "/api/orchestration/profiles/decision-recall/draft", map[string]any{
		"expected_state_token":     profilefs.DraftAbsentToken("decision-recall"),
		"expected_selection_token": profilefs.SelectionAbsentToken("decision-recall"),
		"edit":                     map[string]any{"description": "Stale."}}); status != http.StatusConflict {
		t.Fatalf("a draft started from a replaced version must conflict, got %d", status)
	}
	// A first edit of a published agent starts from it and bumps the minor version.
	status, body = fixture.call(t, http.MethodPut, "/api/orchestration/profiles/decision-recall/draft", map[string]any{
		"expected_state_token":     profilefs.DraftAbsentToken("decision-recall"),
		"expected_selection_token": published.Profile.StateToken, "edit": map[string]any{"description": "Second."}})
	var second draftResponse
	if err := json.Unmarshal(body, &second); err != nil || status != http.StatusOK || second.Draft.Normalized == nil ||
		second.Draft.Normalized.Version != "1.1.0" || second.Draft.BaseSourceDigest != published.Profile.Current.SourceDigest {
		t.Fatalf("second draft = %d %s", status, body)
	}
	if status, _ := fixture.call(t, http.MethodPost, "/api/orchestration/profiles/decision-recall/draft/publish", map[string]any{
		"expected_draft_token": second.Draft.StateToken, "expected_selection_token": "stale", "confirmed": true}); status != http.StatusConflict {
		t.Fatalf("a stale selection token must conflict, got %d", status)
	}
	// Publishing a version number that is already stored is a conflict, not a storage error.
	status, body = fixture.call(t, http.MethodPut, "/api/orchestration/profiles/decision-recall/draft", map[string]any{
		"expected_state_token": second.Draft.StateToken, "edit": map[string]any{"version": "1.0.0"}})
	var taken draftResponse
	if err := json.Unmarshal(body, &taken); err != nil || status != http.StatusOK {
		t.Fatalf("version edit = %d %s", status, body)
	}
	if status, body := fixture.call(t, http.MethodPost, "/api/orchestration/profiles/decision-recall/draft/publish", map[string]any{
		"expected_draft_token": taken.Draft.StateToken, "expected_selection_token": published.Profile.StateToken,
		"confirmed": true}); status != http.StatusConflict || !strings.Contains(string(body), "version_taken") {
		t.Fatalf("a taken version must be a 409 version_taken, got %d %s", status, body)
	}
	second = taken
	status, body = fixture.call(t, http.MethodGet, "/api/orchestration/profiles/decision-recall/revision?source_digest="+
		published.Profile.Current.SourceDigest+"&bundle_digest="+published.Profile.Current.BundleDigest, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "conflicts with the plan") {
		t.Fatalf("revision read = %d %s", status, body)
	}
	if status, _ := fixture.call(t, http.MethodDelete, "/api/orchestration/profiles/decision-recall/draft",
		map[string]any{"expected_state_token": second.Draft.StateToken}); status != http.StatusOK {
		t.Fatalf("discard status = %d", status)
	}
}

func TestReviewerDraftEditsAreLimitedToWhatTheLaneAllows(t *testing.T) {
	fixture := newRosterFixture(t)
	status, body := fixture.call(t, http.MethodPost, "/api/orchestration/drafts", map[string]any{"start": "blank",
		"id": "new-reviewer", "name": "New reviewer", "description": "Reviews actions.", "type": "reviewer"})
	var created draftResponse
	if err := json.Unmarshal(body, &created); err != nil || status != http.StatusOK {
		t.Fatalf("reviewer draft = %d %s", status, body)
	}
	if status, _ := fixture.call(t, http.MethodPut, "/api/orchestration/profiles/new-reviewer/draft", map[string]any{
		"expected_state_token": created.Draft.StateToken, "edit": map[string]any{"trigger_event": "task.completed"}}); status != http.StatusBadRequest {
		t.Fatalf("a reviewer trigger edit must be refused, got %d", status)
	}
	if status, _ := fixture.call(t, http.MethodPut, "/api/orchestration/profiles/new-reviewer/draft", map[string]any{
		"expected_state_token": created.Draft.StateToken, "edit": map[string]any{"instructions": "Be conservative."}}); status != http.StatusOK {
		t.Fatalf("a reviewer instructions edit must be allowed, got %d", status)
	}
}

func TestTemplatesPassTheirLaneAndReadOnlySuppliedContext(t *testing.T) {
	supplied := map[string]bool{}
	for _, kind := range managedContextKinds() {
		supplied[kind] = true
	}
	served := map[string]bool{}
	for _, signal := range servedSignalCatalog(true) {
		if signal.Served {
			served[signal.Kind] = true
		}
	}
	for _, kind := range []string{"helper", "follower"} {
		source, err := profilefs.NewSource(profilefs.NewProfile{ID: "t-" + kind, Name: "T", Description: "d", Type: kind})
		if err != nil {
			t.Fatal(err)
		}
		document, err := profilefs.Parse("PROFILE.md", source)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateManagedProfile(document.Profile); err != nil {
			t.Fatalf("%s template fails the managed lane: %v", kind, err)
		}
		if !served[document.Profile.Trigger.Event] {
			t.Fatalf("%s template trigger %q is not served", kind, document.Profile.Trigger.Event)
		}
		for _, context := range document.Profile.Context {
			if !supplied[context.Kind] {
				t.Fatalf("%s template asks for unsupplied context %q", kind, context.Kind)
			}
		}
	}
	source, err := profilefs.NewSource(profilefs.NewProfile{ID: "t-reviewer", Name: "T", Description: "d", Type: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	document, err := profilefs.Parse("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sliceCProfile(document.Profile, document.SourceDigest, document.BundleDigest); err != nil {
		t.Fatalf("reviewer template fails the review lane: %v", err)
	}
}

func TestManagedContextKindsMatchWhatTheReaderSupplies(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	reader := &managedContextReader{ix: ix}
	for _, kind := range managedContextKinds() {
		_, _, _, _, err := reader.readSelection(orchestration.ContextRequest{}, orchestration.ContextSelection{Kind: kind, MaxBytes: 1024})
		if err != nil && strings.HasPrefix(err.Error(), "context provider unavailable") {
			t.Fatalf("listed kind %q is not supplied by the reader", kind)
		}
	}
	for _, option := range managedAlwaysContext() {
		_, _, _, _, err := reader.readSelection(orchestration.ContextRequest{}, orchestration.ContextSelection{Kind: option.Kind, MaxBytes: 1024})
		if err != nil && strings.HasPrefix(err.Error(), "context provider unavailable") {
			t.Fatalf("always-read kind %q is not supplied by the reader", option.Kind)
		}
	}
	_, _, _, _, err = reader.readSelection(orchestration.ContextRequest{}, orchestration.ContextSelection{Kind: "project-files", MaxBytes: 1024})
	if err == nil || !strings.HasPrefix(err.Error(), "context provider unavailable") {
		t.Fatalf("an unlisted kind must stay unavailable, got %v", err)
	}
}

func TestBootstrapFiresOnlyWhenABindingNewlyWatches(t *testing.T) {
	watching := store.ManagedBinding{State: "enabled", WatchNatural: true, ProjectRoot: "/r"}
	edited := watching
	edited.Model, edited.Priority = "other", 5
	off := watching
	off.State = "disabled"
	moved := watching
	moved.ProjectRoot = "/elsewhere"
	notWatching := watching
	notWatching.WatchNatural = false
	for name, test := range map[string]struct {
		prior *store.ManagedBinding
		saved store.ManagedBinding
		want  bool
	}{
		"created watching":     {nil, watching, true},
		"model-only edit":      {&watching, edited, false},
		"re-enabled":           {&off, watching, true},
		"disabled":             {&watching, off, false},
		"newly watching":       {&notWatching, watching, true},
		"moved to new root":    {&watching, moved, true},
		"created not watching": {nil, notWatching, false},
	} {
		if got := bootstrapTransition(test.prior, test.saved); got != test.want {
			t.Fatalf("%s: bootstrap = %v, want %v", name, got, test.want)
		}
	}
}

func TestFreeBindingIDIsBoundedAndSanitized(t *testing.T) {
	taken := map[string]bool{"agent--repo": true, "agent--repo-2": true}
	id, err := freeBindingID("agent", "/work/Repo", func(candidate string) (bool, error) { return taken[candidate], nil })
	if err != nil || id != "agent--repo-3" {
		t.Fatalf("id = %q %v", id, err)
	}
	id, err = freeBindingID("agent", "/work/My Repo!!", func(string) (bool, error) { return false, nil })
	if err != nil || id != "agent--my-repo" {
		t.Fatalf("sanitized id = %q %v", id, err)
	}
	long := strings.Repeat("p", 60)
	id, err = freeBindingID(long, "/work/"+strings.Repeat("r", 40), func(string) (bool, error) { return false, nil })
	if err != nil || len(id) > 64 || !managedBindingIDPattern.MatchString(id) {
		t.Fatalf("long id = %q %v", id, err)
	}
	if _, err := freeBindingID("Bad Id", "/work/repo", func(string) (bool, error) { return false, nil }); err == nil {
		t.Fatal("an unstable profile id must be refused")
	}
}

func TestSectionEditsKeepAHelpersChildRevisions(t *testing.T) {
	kept := []store.ManagedProfileRef{{ProfileID: "child", SourceDigest: "sha256-v1:old-src", BundleDigest: "sha256-v1:old-bundle"}}
	// No profile owner: keeping the stored refs must not look children up again.
	host := &orchestrationManagedHost{}
	got, err := host.resolveAllowedProfiles([]string{"child"}, kept)
	if err != nil || len(got) != 1 || got[0] != kept[0] {
		t.Fatalf("an unchanged allowlist must keep the pinned child, got %v %v", got, err)
	}
	if sameProfileIDs([]string{"child", "other"}, kept) || sameProfileIDs([]string{"child"}, nil) {
		t.Fatal("a changed allowlist or no stored refs must re-resolve")
	}
	binding := store.ManagedBinding{BindingID: "b", ProfileID: "parent", AllowedProfiles: kept}
	command := commandFromBinding(binding, "token")
	priority := int64(5)
	applyBindingPatch(&command, bindingPatch{Priority: &priority})
	if len(command.AllowedProfiles) != 1 || command.AllowedProfiles[0] != kept[0] {
		t.Fatalf("a priority edit must carry the child refs, got %v", command.AllowedProfiles)
	}
	applyBindingPatch(&command, bindingPatch{Revision: &bindingRevisionPatch{SourceDigest: "s", BundleDigest: "b"}})
	if command.AllowedProfiles != nil {
		t.Fatalf("a new version must re-pin its children, got %v", command.AllowedProfiles)
	}
}
