package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"crossing-guard/internal/vendorpaths"
	"time"
)

func init() { registerSkillsProvider(codexSkills{}) }

// codexSkills is the Codex skills coverage provider. Codex reads the neutral
// .agents dirs plus its own; it exposes a live app-server enumeration seam.
type codexSkills struct{}

func (codexSkills) Name() string            { return "codex" }
func (codexSkills) CanProbe() bool          { return true }
func (codexSkills) Manages(dir string) bool { return strings.Contains(dir, ".system") }

func (codexSkills) Roots(home, repo string) []skillRoot {
	roots := []skillRoot{
		{filepath.Join(home, ".agents", "skills"), "user", "agents"},
		{filepath.Join(home, ".codex", "skills"), "user", "codex"},
	}
	if repo != "" {
		roots = append(roots, skillRoot{filepath.Join(repo, ".agents", "skills"), "repo", "agents"})
	}
	return roots
}

func (codexSkills) Lint(se *SkillEntry, _ string) []string {
	if se.Frontmatter["disable-model-invocation"] == "true" {
		return []string{"disable-model-invocation: true — Codex validator hard-rejects"}
	}
	return nil
}

func (codexSkills) Coverage(sk *Skill) CoverageCell {
	es := entriesIn(sk, "agents", "codex")
	if len(es) == 0 {
		return CoverageCell{State: "not-synced", Grade: "fs", Why: "not present in ~/.agents/skills, .agents/skills, or ~/.codex/skills"}
	}
	for _, e := range es {
		if e.Frontmatter["disable-model-invocation"] == "true" {
			return CoverageCell{State: "would-reject", Grade: "config", Why: "disable-model-invocation: true — Codex validator hard-rejects",
				Fix: "remove `disable-model-invocation: true` (or keep a Codex-excluded copy)"}
		}
		if len(e.ExtraFields) > 0 {
			return CoverageCell{State: "would-reject", Grade: "config",
				Why: "non-spec frontmatter fields: " + strings.Join(e.ExtraFields, ", ") + " — Codex installer accepts only the spec-5",
				Fix: "move extras into `metadata:` or a sidecar"}
		}
	}
	if b, err := os.ReadFile(filepath.Join(homeDir(), filepath.FromSlash(vendorpaths.CodexConfigRelative))); err == nil {
		if m := regexp.MustCompile(`disabled_skill_names\s*=\s*\[([^\]]*)\]`).FindStringSubmatch(string(b)); m != nil {
			if strings.Contains(m[1], `"`+sk.Name+`"`) {
				return CoverageCell{State: "disabled", Grade: "config", Why: "listed in disabled_skill_names (config.toml)"}
			}
		}
	}
	return CoverageCell{State: "visible", Grade: "config", Why: "in a Codex-read dir, passes validator rules (probe to confirm live)"}
}

// Probe spawns codex app-server and calls skills/list over newline-delimited
// JSON-RPC. Seconds-scale — only ever user-triggered.
func (codexSkills) Probe() *ProbeInfo {
	pi := &ProbeInfo{At: time.Now().UTC().Format(time.RFC3339)}
	bin := "codex"
	if _, err := exec.LookPath(bin); err != nil {
		bundled := "/Applications/ChatGPT.app/Contents/Resources/codex"
		if _, err := os.Stat(bundled); err == nil {
			bin = bundled
		} else {
			pi.Err = "codex binary not found"
			return pi
		}
	}
	cmd := exec.Command(bin, "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		pi.Err = err.Error()
		return pi
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		pi.Err = err.Error()
		return pi
	}
	if err := cmd.Start(); err != nil {
		pi.Err = err.Error()
		return pi
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(stdin, "%s\n", b) }
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{"name": "consoleprobe", "title": "consoleprobe", "version": "0.0.1"}}})

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	deadline := time.After(8 * time.Second)
	lines := make(chan string, 64)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-deadline:
			pi.Err = "timeout waiting for app-server response"
			return pi
		case line, ok := <-lines:
			if !ok {
				pi.Err = "app-server closed without a skills/list response"
				return pi
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line), &obj) != nil {
				continue
			}
			switch obj["id"] {
			case float64(1):
				send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
				send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "skills/list", "params": map[string]any{}})
			case float64(2):
				if errObj, isErr := obj["error"].(map[string]any); isErr {
					pi.Err = "skills/list error: " + anyString(errObj["message"])
					return pi
				}
				collectNames(obj["result"], pi)
				pi.OK = true
				return pi
			}
		}
	}
}
