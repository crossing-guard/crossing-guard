package guardcli

// Codex registration of the recall server (recall-mcp-v1-plan §3.7): a
// marker-delimited [mcp_servers.crossing-guard] table in <CODEX_HOME>/config.toml.
// Codex refuses its WHOLE config on one invalid value (measured on
// 0.154.0-alpha.6.2), so every spliced result is parsed before it is written,
// and a server of our name defined outside our markers is never touched.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"crossing-guard/internal/recallmcp"
	"crossing-guard/internal/vendorconfig"
)

const (
	codexRecallBegin = "# >>> crossing-guard recall (crossing-guard init --recall) >>>"
	codexRecallEnd   = "# <<< crossing-guard recall <<<"
	// codexRecallApproval pre-approves the server's tools. Codex's exec runs
	// with approval never, so an unapproved tool call is refused; the tools'
	// readOnlyHint also carries this, the key makes it independent of that.
	codexRecallApproval = "approve"
)

func (codexInstaller) RecallConfig(codexHome string) string {
	return filepath.Join(codexHome, "config.toml")
}

// tomlString is a TOML basic string: JSON's string escapes are TOML's.
func tomlString(value string) string {
	encoded, _ := json.Marshal(value)
	return strings.NewReplacer(`<`, "<", `>`, ">", `&`, "&").Replace(string(encoded))
}

func codexRecallBlock(self string) string {
	args := recallArgs(codexVendor)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = tomlString(arg)
	}
	return strings.Join([]string{
		codexRecallBegin,
		"[mcp_servers." + recallmcp.ServerName + "]",
		"command = " + tomlString(self),
		"args = [" + strings.Join(quoted, ", ") + "]",
		"default_tools_approval_mode = " + tomlString(codexRecallApproval),
		codexRecallEnd,
	}, "\n")
}

// codexInlineServers finds mcp_servers written as an inline table. Codex
// refuses to load a config whose inline table a later [mcp_servers.x] header
// extends ("cannot extend value of type inline table", measured on
// 0.154.0-alpha.6.2), while the Go TOML parser accepts it, so the shape is
// refused by name rather than by a parse.
var codexInlineServers = regexp.MustCompile("(?m)^\uFEFF?[ \t]*[\"']?mcp_servers[\"']?[ \t]*=")

// codexBlockIsOurs checks that the text between our markers is only what we
// write: the one server table and its three keys. Codex edits this file
// itself, so anything else found there is the user's or Codex's, and a
// repair or removal of the span would delete it.
func codexBlockIsOurs(block, path string) error {
	var parsed map[string]any
	if _, err := toml.Decode(block, &parsed); err != nil {
		return fmt.Errorf("%s: the Crossing Guard recall block does not parse (%w); left alone", path, err)
	}
	foreign := fmt.Errorf("%s: the Crossing Guard recall block holds settings Crossing Guard did not write; "+
		"move them outside the markers, then re-run crossing-guard init --recall", path)
	servers, ok := parsed["mcp_servers"].(map[string]any)
	if len(parsed) != 1 || !ok || len(servers) != 1 {
		return foreign
	}
	table, ok := servers[recallmcp.ServerName].(map[string]any)
	if !ok {
		return foreign
	}
	for key := range table {
		if key != "command" && key != "args" && key != "default_tools_approval_mode" {
			return foreign
		}
	}
	return nil
}

type codexRecallServer struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
}

// codexRecallServerIn parses content and returns our server's table, if any.
func codexRecallServerIn(content, path string) (codexRecallServer, bool, error) {
	var parsed struct {
		MCPServers map[string]toml.Primitive `toml:"mcp_servers"`
	}
	meta, err := toml.Decode(content, &parsed)
	if err != nil {
		return codexRecallServer{}, false, fmt.Errorf("%s is not valid TOML (%w) — fix it first; refusing to edit it", path, err)
	}
	primitive, ok := parsed.MCPServers[recallmcp.ServerName]
	if !ok {
		return codexRecallServer{}, false, nil
	}
	var server codexRecallServer
	if err := meta.PrimitiveDecode(primitive, &server); err != nil {
		return codexRecallServer{}, true, nil
	}
	return server, true, nil
}

func (installer codexInstaller) RecallStatus(codexHome, self string) (string, error) {
	path := installer.RecallConfig(codexHome)
	snapshot, err := vendorconfig.Read(path)
	if err != nil {
		return "", err
	}
	content := string(snapshot.Data)
	outside, _, err := vendorconfig.RemoveMarkedBlock(content, codexRecallBegin, codexRecallEnd)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if _, foreign, err := codexRecallServerIn(outside, path); err != nil {
		return "", err
	} else if foreign {
		return RecallForeign, nil
	}
	block := vendorconfig.MarkedBlock(content, codexRecallBegin, codexRecallEnd)
	if block == "" {
		return RecallAbsent, nil
	}
	if err := codexBlockIsOurs(block, path); err != nil {
		return "", err
	}
	server, found, err := codexRecallServerIn(block, path)
	if err != nil || !found {
		return RecallStale, nil
	}
	state := recallEntryState(server.Command, server.Args, codexVendor, self)
	if state == RecallCurrent && block != codexRecallBlock(self) {
		return RecallStale, nil
	}
	return state, nil
}

func (installer codexInstaller) RegisterRecall(codexHome, self string) error {
	path := installer.RecallConfig(codexHome)
	snapshot, err := vendorconfig.Read(path)
	if err != nil {
		return err
	}
	content := string(snapshot.Data)
	outside, _, err := vendorconfig.RemoveMarkedBlock(content, codexRecallBegin, codexRecallEnd)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, foreign, err := codexRecallServerIn(outside, path); err != nil {
		return err
	} else if foreign {
		return fmt.Errorf("%s already defines mcp_servers.%s outside Crossing Guard's block; left alone", path, recallmcp.ServerName)
	}
	if block := vendorconfig.MarkedBlock(content, codexRecallBegin, codexRecallEnd); block != "" {
		if err := codexBlockIsOurs(block, path); err != nil {
			return err
		}
	}
	if codexInlineServers.MatchString(outside) {
		return fmt.Errorf("%s writes mcp_servers as an inline table, which Codex cannot extend; add the %s server to it by hand", path, recallmcp.ServerName)
	}
	spliced, changed, err := vendorconfig.UpsertMarkedBlock(content, codexRecallBegin, codexRecallEnd, codexRecallBlock(self))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return nil
	}
	// The whole result must parse, and our table must read back exactly as
	// written (an inline mcp_servers table or dotted keys elsewhere would make
	// the appended header invalid or merge into it).
	server, found, err := codexRecallServerIn(spliced, path)
	if err != nil {
		return fmt.Errorf("adding the recall block would break %s; left unchanged: %w", path, err)
	}
	if !found || server.Command != self || !slices.Equal(server.Args, recallArgs(codexVendor)) {
		return fmt.Errorf("adding the recall block to %s would not read back as written; left unchanged", path)
	}
	_, err = vendorconfig.Replace(path, snapshot, []byte(spliced))
	return err
}

func (installer codexInstaller) UnregisterRecall(codexHome, _ string) (bool, error) {
	path := installer.RecallConfig(codexHome)
	snapshot, err := vendorconfig.Read(path)
	if err != nil || !snapshot.Exists {
		return false, err
	}
	if block := vendorconfig.MarkedBlock(string(snapshot.Data), codexRecallBegin, codexRecallEnd); block != "" {
		if err := codexBlockIsOurs(block, path); err != nil {
			return false, err
		}
	}
	out, removed, err := vendorconfig.RemoveMarkedBlock(string(snapshot.Data), codexRecallBegin, codexRecallEnd)
	if err != nil || !removed {
		return false, err
	}
	_, err = vendorconfig.Replace(path, snapshot, []byte(out))
	return err == nil, err
}

// A registrar that stops satisfying the port would silently drop out of RecallRuntimes.
var _ RecallRegistrar = codexInstaller{}
