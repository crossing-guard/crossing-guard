package main

import (
	"fmt"
	"os"
	"path/filepath"

	"crossing-guard/internal/guardcli"
)

// recallHealth is one runtime's recall-tools registration as doctor reports it.
type recallHealth struct {
	Runtime   string `json:"runtime"`
	File      string `json:"file,omitempty"`
	State     string `json:"state"` // registered | gone | stale | foreign | unconsented-entry | not-consented | not-attached | error
	Consented bool   `json:"consented"`
	Detail    string `json:"detail"`
}

// recallSection reads each runtime's registration. Whether a recall tool has
// actually been called is not reported yet: the ledger records an MCP call's
// entity without its server name (recall-mcp-v1-plan §12).
func recallSection() []recallHealth {
	self, err := os.Executable()
	if err == nil {
		self, _ = filepath.Abs(self)
	}
	consent := guardcli.LoadConsent()
	var out []recallHealth
	for _, name := range guardcli.RecallRuntimes() {
		vendor, hookConsented := consent.Vendors[name]
		hookConfig := guardcli.RecallHookConfig(name)
		health := recallHealth{Runtime: name, Consented: hookConsented && vendor.Recall != nil}
		if hookConfig == "" {
			health.State, health.Detail = "not-attached", "runtime not installed on this machine"
			out = append(out, health)
			continue
		}
		health.File = guardcli.RecallConfigFor(name, hookConfig)
		state, err := guardcli.RecallStatusFor(name, hookConfig, self)
		switch {
		case err != nil:
			health.State, health.Detail = "error", err.Error()
		case health.Consented && state == guardcli.RecallCurrent:
			health.State, health.Detail = "registered", "registered in "+health.File
		case health.Consented && state == guardcli.RecallForeign:
			health.State, health.Detail = "foreign", "another program owns the entry in "+health.File+" — left alone"
		case health.Consented && state == guardcli.RecallStale:
			health.State, health.Detail = "stale", "OUT OF DATE in "+health.File+" — repair: crossing-guard init --recall"
		case health.Consented:
			health.State, health.Detail = "gone", "GONE from "+health.File+" — repair: crossing-guard init --recall"
		case state != guardcli.RecallAbsent && state != guardcli.RecallForeign:
			health.State, health.Detail = "unconsented-entry", "registered without consent — remove: crossing-guard uninstall"
		case !hookConsented:
			health.State, health.Detail = "not-attached", "hooks not attached; recall tools are offered after attaching"
		default:
			health.State, health.Detail = "not-consented", "not registered — add: crossing-guard init --recall"
		}
		out = append(out, health)
	}
	return out
}

func printRecall(rows []recallHealth) {
	fmt.Println("\nRECALL TOOLS   (a separate yes from the hooks; agents search sessions, memories and tags)")
	for _, row := range rows {
		fmt.Printf("  %-8s %s\n", row.Runtime, row.Detail)
	}
}
