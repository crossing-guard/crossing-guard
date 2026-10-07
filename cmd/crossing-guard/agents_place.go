package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"crossing-guard/internal/daemon"
)

// The `agents place` verb: turn an agent on for a repository on a named model route,
// from the terminal (team rest-of-release plan §5.5, §9). It calls the same binding
// routes the Agents page calls; the daemon resolves the route, runs its checks and
// admission, and writes the place.

const agentsUsage = `usage:
  crossing-guard agents place --profile <agent id> --route <route name|id> [--root <repository>] [--mode <mode>]
      turn the agent on for the repository (the current directory by default) on a
      model route. A reviewer takes an inference route and no repository.`

func agentsCmd(args []string) {
	if len(args) == 0 || args[0] != "place" {
		fmt.Fprintln(os.Stderr, agentsUsage)
		os.Exit(2)
	}
	profile, route, root, mode, modeSet := "", "", "", "", false
	rest := args[1:]
	for index := 0; index < len(rest); index++ {
		if index+1 >= len(rest) {
			fmt.Fprintln(os.Stderr, agentsUsage)
			os.Exit(2)
		}
		value := rest[index+1]
		switch rest[index] {
		case "--profile":
			profile = value
		case "--route":
			route = value
		case "--root":
			root = value
		case "--mode":
			mode, modeSet = value, true
		default:
			fmt.Fprintln(os.Stderr, agentsUsage)
			os.Exit(2)
		}
		index++
	}
	if profile == "" || route == "" {
		fmt.Fprintln(os.Stderr, agentsUsage)
		os.Exit(2)
	}
	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no local daemon to ask:", err)
		os.Exit(1)
	}
	if err := placeAgent(loc, profile, route, root, mode, modeSet); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// cliAgentDetail is what `agents place` reads of one agent: its lane, its current
// version, the reviewer's slot, and each runtime's modes.
type cliAgentDetail struct {
	Agent struct {
		Lane       string `json:"lane"`
		Compatible bool   `json:"compatible"`
		Reason     string `json:"incompatible_reason"`
	} `json:"agent"`
	Current *struct {
		Current struct {
			SourceDigest string `json:"source_digest"`
			BundleDigest string `json:"bundle_digest"`
		} `json:"current"`
	} `json:"current"`
	ReviewOption *struct {
		TimeoutCeilingMS int      `json:"timeout_ceiling_ms"`
		Effects          []string `json:"effects"`
	} `json:"review_option"`
	ReviewSlot struct {
		StateToken string `json:"state_token"`
	} `json:"review_slot"`
	Capabilities []struct {
		Runtime string `json:"runtime"`
		Modes   []struct {
			ID   string `json:"id"`
			Risk string `json:"risk"`
		} `json:"modes"`
	} `json:"capabilities"`
}

func placeAgent(loc daemon.Location, profile, routeName, root, mode string, modeSet bool) error {
	var detail cliAgentDetail
	if err := daemonJSON(loc, http.MethodGet, "/api/orchestration/roster/"+url.PathEscape(profile), nil, &detail); err != nil {
		return fmt.Errorf("agent %s could not be read: %w", profile, err)
	}
	if detail.Current == nil {
		return fmt.Errorf("agent %s has no published version to turn on", profile)
	}
	if !detail.Agent.Compatible {
		return fmt.Errorf("agent %s cannot run here: %s", profile, detail.Agent.Reason)
	}
	route, err := findRoute(loc, routeName)
	if err != nil {
		return err
	}
	version := detail.Current.Current
	if detail.Agent.Lane == "review" {
		return placeReviewer(loc, profile, route, detail, version.SourceDigest, version.BundleDigest)
	}
	if root == "" {
		root = "."
	}
	absolute, err := filepath.Abs(root)
	if err == nil {
		absolute, err = filepath.EvalSymlinks(absolute)
	}
	if err != nil {
		return fmt.Errorf("repository %s: %w", root, err)
	}
	if !modeSet {
		mode = firstReadOnlyMode(detail, route.Fields.Runtime)
	}
	var id struct {
		BindingID          string `json:"binding_id"`
		ExpectedStateToken string `json:"expected_state_token"`
	}
	if err := daemonJSON(loc, http.MethodPost, "/api/orchestration/agents/binding-id",
		map[string]string{"profile_id": profile, "project_root": absolute}, &id); err != nil {
		return fmt.Errorf("a place id could not be derived: %w", err)
	}
	place := map[string]any{"profile_id": profile, "profile_source_digest": version.SourceDigest,
		"profile_bundle_digest": version.BundleDigest, "project_root": absolute, "route_id": route.RouteID,
		"mode": mode, "granted_authority": []string{}, "state": "enabled"}
	var written struct {
		Results []struct {
			Valid   bool   `json:"valid"`
			Problem string `json:"problem"`
		} `json:"results"`
		Written bool `json:"written"`
	}
	err = daemonJSON(loc, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{{"binding_id": id.BindingID, "expected_state_token": id.ExpectedStateToken,
			"op": "create", "place": place}}}, &written)
	if err != nil {
		return fmt.Errorf("the place was refused: %w", err)
	}
	if !written.Written {
		return errors.New("the place was refused")
	}
	fmt.Printf("turned on %s for %s on model route %s (%s); place %s\n", profile, absolute, route.Name, route.Locality, id.BindingID)
	return nil
}

// firstReadOnlyMode is the first mode the route's runtime publishes at normal risk —
// the only kind a place may run in. "" when the runtime publishes none; the daemon then
// says so.
func firstReadOnlyMode(detail cliAgentDetail, runtime string) string {
	for _, capability := range detail.Capabilities {
		if capability.Runtime != runtime {
			continue
		}
		for _, mode := range capability.Modes {
			if mode.Risk == "normal" {
				return mode.ID
			}
		}
	}
	return ""
}

// reviewSubdeadlineFloorMS is the smallest approval subdeadline the review binding
// route accepts; the verb halves the timeout and never goes below it.
const reviewSubdeadlineFloorMS = 250

func placeReviewer(loc daemon.Location, profile string, route cliRoute, detail cliAgentDetail, sourceDigest, bundleDigest string) error {
	if detail.ReviewOption == nil || len(detail.ReviewOption.Effects) == 0 {
		return fmt.Errorf("agent %s cannot run as the reviewer here", profile)
	}
	effect, timeout := detail.ReviewOption.Effects[0], detail.ReviewOption.TimeoutCeilingMS
	body := map[string]any{"profile_id": profile, "profile_source_digest": sourceDigest, "profile_bundle_digest": bundleDigest,
		"route_id": route.RouteID, "timeout_ms": timeout, "effect": effect, "runtime_filter": "",
		"expected_state_token": detail.ReviewSlot.StateToken, "confirmed": true}
	if effect == "delegated-first" {
		body["approval_subdeadline_ms"] = max(timeout/2, reviewSubdeadlineFloorMS)
	}
	if err := daemonJSON(loc, http.MethodPut, "/api/orchestration/reviews/binding", body, nil); err != nil {
		return fmt.Errorf("the reviewer was refused: %w", err)
	}
	fmt.Printf("turned on %s as the reviewer on model route %s (%s)\n", profile, route.Name, route.Locality)
	return nil
}
