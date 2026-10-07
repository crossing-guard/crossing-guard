package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The session-agents panel and the Agents settings page are the ONE managed
// orchestration surface (plan §9a): no managed-work drawer, no workdrawer
// host, no innerHTML anywhere in the orchestration frontend directory.
func TestManagedOrchestrationFrontendUsesOnePanelSurfaceAndTypedAPIs(t *testing.T) {
	const dir = "static/js/orchestration"
	// Walk the whole tree: the Agents pages live in orchestration/agents/, and
	// a flat read would let them escape every ban below (agents redesign RT-9).
	agentsSurface := []byte{}
	sawAgentsPage := false
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".js") {
			return walkErr
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte("innerHTML")) || bytes.Contains(body, []byte("insertAdjacentHTML")) {
			t.Fatalf("%s uses raw HTML injection", path)
		}
		for _, forbidden := range []string{"managed-work-drawer", "workdrawer", "cg:managed-work-open",
			"coordinator", "course-corrector"} {
			if bytes.Contains(body, []byte(forbidden)) {
				t.Fatalf("%s still speaks the retired surface/vocabulary %q", path, forbidden)
			}
		}
		if filepath.Dir(path) == filepath.Join(dir, "agents") {
			agentsSurface = append(agentsSurface, body...)
			sawAgentsPage = sawAgentsPage || filepath.Base(path) == "agents-page.js"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAgentsPage {
		t.Fatal("the walk did not reach orchestration/agents/agents-page.js")
	}
	if _, err := os.Stat(filepath.Join(dir, "managed-work-drawer.js")); !os.IsNotExist(err) {
		t.Fatal("the managed work drawer must stay deleted; the session-agents panel is the one surface")
	}
	index, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(index, []byte("workdrawer")) {
		t.Fatal("index.html still carries the retired workdrawer host")
	}
	appJS, err := os.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(appJS, []byte("managed-work-drawer")) {
		t.Fatal("app.js still imports the retired drawer")
	}
	if !bytes.Contains(appJS, []byte("session-agents-panel.js")) {
		t.Fatal("app.js lost the session-agents panel registration")
	}
	sessions, err := os.ReadFile("static/js/views/sessions.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range [][]byte{[]byte("mountSessionOrchestration"), []byte("session-orchestration.js")} {
		if !bytes.Contains(sessions, needle) {
			t.Fatalf("sessions integration missing %q", needle)
		}
	}
	// The Agents pages (agents-settings-redesign plan §3.2): an index of
	// agents, one detail page per agent, the importer in its dialog, places
	// created through the daemon-issued id and token, and every place change
	// through the one all-or-nothing batch.
	for _, needle := range []string{"renderProfileImporter(", "/api/orchestration/roster",
		"/api/orchestration/agents/batch", "/api/orchestration/agents/binding-id", "saveReviewBinding",
		"declared_tags", "priority", "view=rail", "granted_authority"} {
		if !bytes.Contains(agentsSurface, []byte(needle)) {
			t.Fatalf("Agents pages lost %q", needle)
		}
	}
	for _, retired := range []string{"reviewerBindingCard", "managedBindingCard", "Deploy agents",
		"renderOrchestrationProfiles", "agentPageModel"} {
		if bytes.Contains(agentsSurface, []byte(retired)) {
			t.Fatalf("Agents pages still carry the retired card composition %q", retired)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "settings-agents.js")); !os.IsNotExist(err) {
		t.Fatal("settings-agents.js must stay deleted; orchestration/agents/ owns the Agents pages")
	}
	for _, deleted := range []string{"settings-managed.js", "settings-reviews.js"} {
		if _, err := os.Stat(filepath.Join(dir, deleted)); !os.IsNotExist(err) {
			t.Fatalf("%s must stay deleted; orchestration/agents/ owns its function", deleted)
		}
	}
}
