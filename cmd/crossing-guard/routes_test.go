package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crossing-guard/internal/daemon"
)

// routeStub records what the route verbs send and answers like the daemon's route API.
type routeStub struct {
	requests []string
	bodies   []map[string]any
}

func (stub *routeStub) serve(t *testing.T) daemon.Location {
	t.Helper()
	mux := http.NewServeMux()
	record := func(r *http.Request) map[string]any {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		stub.requests = append(stub.requests, r.Method+" "+r.URL.Path)
		stub.bodies = append(stub.bodies, body)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("%s %s carried no bearer token", r.Method, r.URL.Path)
		}
		return body
	}
	answer := func(w http.ResponseWriter, value any) { _ = json.NewEncoder(w).Encode(value) }
	route := map[string]any{"route_id": "rte_01J00000000000000000000000", "name": "Fast One", "family": "runtime-model",
		"fields": map[string]any{"runtime": "alpha", "model": "model-a"}, "locality": "leaves this machine", "state_token": "token-1"}
	mux.HandleFunc("GET /api/model-routes", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		answer(w, map[string]any{"routes": []any{route}})
	})
	mux.HandleFunc("POST /api/model-routes/preview", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		answer(w, map[string]any{"preview_digest": "digest-1", "state_token": "token-1", "problems": []any{}})
	})
	mux.HandleFunc("POST /api/model-routes/select", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		answer(w, map[string]any{"route": route})
	})
	mux.HandleFunc("DELETE /api/model-routes/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusConflict)
		answer(w, map[string]any{"error": map[string]any{"code": "route_in_use", "message": "This model route is still used by: helper in repo."}})
	})
	mux.HandleFunc("GET /api/orchestration/roster/{profile}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		answer(w, map[string]any{"agent": map[string]any{"lane": "managed", "compatible": true},
			"current":      map[string]any{"current": map[string]any{"source_digest": "src", "bundle_digest": "bundle"}},
			"capabilities": []any{map[string]any{"runtime": "alpha", "modes": []any{map[string]any{"id": "write", "risk": "elevated"}, map[string]any{"id": "read", "risk": "normal"}}}}})
	})
	mux.HandleFunc("POST /api/orchestration/agents/binding-id", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		answer(w, map[string]any{"binding_id": "helper--repo", "expected_state_token": "absent"})
	})
	mux.HandleFunc("POST /api/orchestration/agents/batch", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		answer(w, map[string]any{"written": true, "results": []any{map[string]any{"valid": true}}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return daemon.Location{Addr: strings.TrimPrefix(server.URL, "http://"), Token: "test-token"}
}

func TestRoutesCreatePreviewsThenSelectsWithTheDigestAndToken(t *testing.T) {
	stub := &routeStub{}
	loc := stub.serve(t)
	if err := routesCreate(loc, []string{"--name", "Fast One", "--family", "runtime-model", "--runtime", "alpha", "--model", "model-a", "--effort", "high"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 2 || stub.requests[0] != "POST /api/model-routes/preview" || stub.requests[1] != "POST /api/model-routes/select" {
		t.Fatalf("requests = %v", stub.requests)
	}
	selected := stub.bodies[1]
	if selected["preview_digest"] != "digest-1" || selected["expected_state_token"] != "token-1" || selected["confirmed"] != true ||
		selected["name"] != "Fast One" || selected["family"] != "runtime-model" {
		t.Fatalf("select body = %v", selected)
	}
	fields, _ := selected["fields"].(map[string]any)
	effort, _ := fields["thinking_effort"].(map[string]any)
	if fields["runtime"] != "alpha" || fields["model"] != "model-a" || effort["kind"] != "level" || effort["value"] != "high" {
		t.Fatalf("select fields = %v", fields)
	}
	if err := routesCreate(loc, []string{"--name", "No family"}); err != errRoutesUsage {
		t.Fatalf("a create with no family: %v", err)
	}
}

func TestRoutesRenameKeepsTheIDAndDeleteShowsTheDaemonsReason(t *testing.T) {
	stub := &routeStub{}
	loc := stub.serve(t)
	if err := routesRename(loc, []string{"  fast   ONE ", "Quick"}); err != nil {
		t.Fatal(err)
	}
	renamed := stub.bodies[len(stub.bodies)-1]
	if renamed["route_id"] != "rte_01J00000000000000000000000" || renamed["name"] != "Quick" {
		t.Fatalf("rename must select a new revision of the same id: %v", renamed)
	}
	err := routesDelete(loc, []string{"rte_01J00000000000000000000000"})
	if err == nil || !strings.Contains(err.Error(), "route_in_use") || !strings.Contains(err.Error(), "helper in repo") {
		t.Fatalf("delete of a used route: %v", err)
	}
	deleted := stub.bodies[len(stub.bodies)-1]
	if deleted["expected_state_token"] != "token-1" || deleted["confirmed"] != true {
		t.Fatalf("delete body = %v", deleted)
	}
	if _, err := findRoute(loc, "no such route"); err == nil {
		t.Fatal("an unknown route resolved")
	}
}

// A place is written by route: the verb never sends a runtime, a model or an endpoint.
func TestAgentsPlaceSendsARouteAndNoTypedModelField(t *testing.T) {
	stub := &routeStub{}
	loc := stub.serve(t)
	root := t.TempDir()
	if err := placeAgent(loc, "helper", "fast one", root, "", false); err != nil {
		t.Fatal(err)
	}
	batch := stub.bodies[len(stub.bodies)-1]
	changes, _ := batch["changes"].([]any)
	change, _ := changes[0].(map[string]any)
	place, _ := change["place"].(map[string]any)
	if place["route_id"] != "rte_01J00000000000000000000000" || place["mode"] != "read" || change["op"] != "create" ||
		change["expected_state_token"] != "absent" || place["profile_source_digest"] != "src" {
		t.Fatalf("place = %v change = %v", place, change)
	}
	for _, typed := range []string{"runtime", "model", "endpoint", "thinking_effort"} {
		if _, sent := place[typed]; sent {
			t.Errorf("the verb sent %s on a binding write", typed)
		}
	}
}
