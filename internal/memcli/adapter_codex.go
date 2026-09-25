package memcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"crossing-guard/internal/vendorconfig"
	"crossing-guard/internal/vendorpaths"
)

func init() { registerAdapter(codexAdapter{}) }

const codexMarkBegin = "# >>> crossing-guard memory (cpmem attach codex) >>>"
const codexMarkEnd = "# <<< crossing-guard memory <<<"

// codexAdapter integrates memory hooks into a Codex config.toml.
type codexAdapter struct{}

func (codexAdapter) Name() string       { return "codex" }
func (codexAdapter) ConfigFlag() string { return "config" }
func (codexAdapter) DefaultConfigPath() string {
	return filepath.Join(home(), filepath.FromSlash(vendorpaths.CodexConfigRelative))
}

// MemoryHookBinary scans config.toml for our SessionStart memory hook. It reads
// the whole file rather than only our marker block: a hook installed by an older
// build (or by hand) still injects, and reporting it as absent would tell the user
// to attach something that is already attached.
func (codexAdapter) MemoryHookBinary(configPath string) string {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	for _, m := range codexHookCommand.FindAllStringSubmatch(string(raw), -1) {
		command := m[1]
		if command == "" {
			command = m[2]
		}
		if bin := memoryHookBinaryFromCommand(command); bin != "" {
			return bin
		}
	}
	return ""
}

// codexHookCommand matches both TOML basic strings (double quotes) and literal
// strings (single quotes). Our writer uses the basic form, but hand-written valid
// TOML must not make an installed hook look absent.
var codexHookCommand = regexp.MustCompile(`(?m)^\s*command\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// Attach appends a marker-delimited [[hooks.SessionStart]] block to config.toml
// (registration format probed live 2026-07-14). Execution is TRUST-GATED by
// Codex: until the user approves via /hooks, the hook is parsed but SILENTLY
// SKIPPED — hence the printed onboarding step.
func (codexAdapter) Attach(configPath, selfPath string) error {
	snapshot, err := vendorconfig.Read(configPath)
	if err != nil {
		return err
	}
	content := string(snapshot.Data)
	block := fmt.Sprintf(`%s
[[hooks.SessionStart]]
[[hooks.SessionStart.hooks]]
type = "command"
command = "%s memory index"
%s`, codexMarkBegin, selfPath, codexMarkEnd)
	if i := strings.Index(content, codexMarkBegin); i >= 0 {
		if j := strings.Index(content, codexMarkEnd); j > i {
			old := content[i : j+len(codexMarkEnd)]
			if old == block {
				fmt.Println("already installed:", configPath)
				return nil
			}
			content = content[:i] + block + content[j+len(codexMarkEnd):]
		} else {
			return fmt.Errorf("%s: begin marker without end marker — fix by hand", configPath)
		}
	} else {
		content = content + "\n" + block + "\n"
	}
	backup, err := vendorconfig.Replace(configPath, snapshot, []byte(content))
	if err != nil {
		return err
	}
	fmt.Println("installed [[hooks.SessionStart]] →", configPath)
	if backup != "" {
		fmt.Println("backup (immediate prior) →", backup)
	}
	fmt.Println("REQUIRED next step (trust gate): open codex, run /hooks, approve this hook.")
	fmt.Println("Until approved, Codex parses but SILENTLY SKIPS it. Then verify: crossing-guard doctor")
	return nil
}

// InjectedText recognizes Codex's injected-context line: a developer-role
// response_item.
func (codexAdapter) InjectedText(line []byte) (string, bool) {
	var row struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &row) != nil {
		return "", false
	}
	if row.Type == "response_item" && row.Payload.Role == "developer" {
		var b strings.Builder
		for _, c := range row.Payload.Content {
			b.WriteString(c.Text)
		}
		return b.String(), true
	}
	return "", false
}
