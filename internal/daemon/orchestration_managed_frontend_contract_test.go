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
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("innerHTML")) || bytes.Contains(body, []byte("insertAdjacentHTML")) {
			t.Fatalf("%s uses raw HTML injection", entry.Name())
		}
		for _, forbidden := range []string{"managed-work-drawer", "workdrawer", "cg:managed-work-open",
			"coordinator", "course-corrector"} {
			if bytes.Contains(body, []byte(forbidden)) {
				t.Fatalf("%s still speaks the retired surface/vocabulary %q", entry.Name(), forbidden)
			}
		}
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
	// The Agents page is agent-first (G-4): the roster is the page, one card
	// per agent joins profile+binding (agentPageModel), the importer creates
	// the card, and creation runs only through the daemon-published ABSENT
	// token so a collided binding id CAS-conflicts instead of rebinding.
	agentsPage, err := os.ReadFile(filepath.Join(dir, "settings-agents.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"renderProfileImporter(main", "agentPageModel",
		"absent_state_tokens", "saveAgent", "saveReviewBinding", "declared_tags", "priority",
		"view=rail", "granted_authority"} {
		if !bytes.Contains(agentsPage, []byte(needle)) {
			t.Fatalf("Agents page lost %q", needle)
		}
	}
	for _, retired := range []string{"reviewerBindingCard", "managedBindingCard", "Deploy agents",
		"renderOrchestrationProfiles"} {
		if bytes.Contains(agentsPage, []byte(retired)) {
			t.Fatalf("Agents page still carries the retired type-first composition %q", retired)
		}
	}
	for _, deleted := range []string{"settings-managed.js", "settings-reviews.js"} {
		if _, err := os.Stat(filepath.Join(dir, deleted)); !os.IsNotExist(err) {
			t.Fatalf("%s must stay deleted; settings-agents.js owns its function", deleted)
		}
	}
}
