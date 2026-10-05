package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
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
	bin, err := codexBinary("")
	if err != nil {
		pi.Err = err.Error()
		return pi
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

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	lines := make(chan []byte, 64)
	go func() {
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(lines)
	}()
	rpc := &codexAppServer{stdin: stdin, lines: lines, deadline: time.After(8 * time.Second)}
	result, err := rpc.initializeAndCall("consoleprobe", "skills/list", map[string]any{})
	switch {
	case errors.Is(err, errCodexAppServerTimeout):
		pi.Err = "timeout waiting for app-server response"
		return pi
	case errors.Is(err, errCodexAppServerClosed):
		pi.Err = "app-server closed without a skills/list response"
		return pi
	case err != nil:
		pi.Err = "skills/list error: " + err.Error()
		return pi
	}
	var decoded any
	_ = json.Unmarshal(result, &decoded)
	collectNames(decoded, pi)
	pi.OK = true
	return pi
}

// codexAppServer speaks the app-server's newline-delimited JSON-RPC over a
// stdin writer and a line stream. The caller owns the process: the skills
// probe its own pipes, model discovery the framework's bounded runner session.
type codexAppServer struct {
	stdin    io.Writer
	lines    <-chan []byte
	deadline <-chan time.Time // nil: the line stream's own end bounds the exchange
	done     <-chan struct{}  // optional: the caller's context
	nextID   int
}

var (
	errCodexAppServerClosed  = errors.New("app-server closed")
	errCodexAppServerTimeout = errors.New("app-server timed out")
)

func (c *codexAppServer) send(message map[string]any) error {
	message["jsonrpc"] = "2.0"
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(encoded, '\n'))
	return err
}

// call sends one request and returns its result; notifications and replies to
// other ids are skipped. A JSON-RPC error returns its message.
func (c *codexAppServer) call(method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, errCodexAppServerClosed
	}
	for {
		select {
		case <-c.deadline:
			return nil, errCodexAppServerTimeout
		case <-c.done:
			return nil, errCodexAppServerTimeout
		case line, ok := <-c.lines:
			if !ok {
				return nil, errCodexAppServerClosed
			}
			var reply struct {
				ID     any             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &reply) != nil || reply.ID != float64(id) {
				continue
			}
			if reply.Error != nil {
				return nil, errors.New(reply.Error.Message)
			}
			return reply.Result, nil
		}
	}
}

// initializeAndCall performs the initialize handshake, then one call.
func (c *codexAppServer) initializeAndCall(client, method string, params any) (json.RawMessage, error) {
	if _, err := c.call("initialize", map[string]any{"clientInfo": map[string]any{
		"name": client, "title": client, "version": "0.0.1"}}); err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, errCodexAppServerClosed
	}
	return c.call(method, params)
}
