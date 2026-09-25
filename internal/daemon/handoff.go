package daemon

// handoff.go — the product verb: generate a handoff from a session's
// canonical events, let the human edit, publish files-first.
//
// Anchors: every extracted statement carries [runtime/session#seq] — this
// deliberately prototypes the source-reference format the memory track
// needs.
//
// Honesty: extraction here is MECHANICAL (first user ask, files touched,
// commands run, last agent text). It is labeled observed-with-anchors and
// makes no claim to be the smart synthesis (that is Stage 3 of the
// smarter-memory roadmap). The human edit step is the quality gate — per
// the PRD, handoffs permit manual editing before publication.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type HandoffDraft struct {
	Markdown     string `json:"markdown"`
	SuggestedDir string `json:"suggested_dir"`
	SessionRef   string `json:"session_ref"`
}

type PublishRequest struct {
	Dir         string `json:"dir"`
	Markdown    string `json:"markdown"`
	WriteAgents bool   `json:"write_agents"` // delimited block in AGENTS.md
	WriteClaude bool   `json:"write_claude"` // delimited block in CLAUDE.md
	GitIgnore   bool   `json:"gitignore"`    // ensure .crossing-guard/ ignored
}

type PublishResult struct {
	Written []string `json:"written"`
	Notes   []string `json:"notes"`
}

const markerStart = "<!-- crossing-guard:handoff:start -->"
const markerEnd = "<!-- crossing-guard:handoff:end -->"

func handleHandoffGenerate(w http.ResponseWriter, r *http.Request) {
	runtime, id := r.URL.Query().Get("runtime"), r.URL.Query().Get("id")
	d, err := LoadSession(runtime, id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, generateHandoff(d))
}

var fileRe = regexp.MustCompile(`"file_path"\s*:\s*"([^"]+)"`)
var cmdRe = regexp.MustCompile(`"command"\s*:\s*"((?:[^"\\]|\\.)*)"`)

func generateHandoff(d *SessionDetail) HandoffDraft {
	ref := d.Runtime + "/" + d.ID
	anchor := func(seq int) string { return fmt.Sprintf("[%s#%d]", ref, seq) }

	var objective, objAnchor string
	var lastAgent, lastAgentAnchor string
	files := map[string]int{} // path -> first seq
	cmds := []string{}        // "cmd  [anchor]"
	toolCounts := map[string]int{}

	for _, ev := range d.Events {
		switch ev.Kind {
		case "user":
			if objective == "" && len(ev.Text) > 10 {
				objective, objAnchor = ev.Text, anchor(ev.Seq)
			}
		case "assistant":
			if len(ev.Text) > 40 {
				lastAgent, lastAgentAnchor = ev.Text, anchor(ev.Seq)
			}
		case "tool_call":
			toolCounts[ev.Name]++
			if m := fileRe.FindStringSubmatch(ev.Text); m != nil && (ev.Name == "Edit" || ev.Name == "Write" || ev.Name == "edit") {
				if _, seen := files[m[1]]; !seen {
					files[m[1]] = ev.Seq
				}
			}
			if m := cmdRe.FindStringSubmatch(ev.Text); m != nil && len(cmds) < 12 {
				c := strings.ReplaceAll(m[1], `\"`, `"`)
				cmds = append(cmds, "`"+truncate(c, 90)+"`  "+anchor(ev.Seq))
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Handoff — %s\n\n", truncate(strings.Split(objective, "\n")[0], 80))
	fmt.Fprintf(&b, "- source-session: %s (%s)\n- generated: %s\n- generator: consoleprobe mechanical-extract v0 (not smart synthesis — edit before publishing)\n\n",
		ref, d.Project, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "## Objective (observed %s)\n\n%s\n\n", objAnchor, truncate(objective, 1200))

	if len(files) > 0 {
		b.WriteString("## Files touched (observed)\n\n")
		for p, seq := range files {
			fmt.Fprintf(&b, "- `%s`  %s\n", p, anchor(seq))
		}
		b.WriteString("\n")
	}
	if len(cmds) > 0 {
		b.WriteString("## Commands run (observed, first 12)\n\n")
		for _, c := range cmds {
			b.WriteString("- " + c + "\n")
		}
		b.WriteString("\n")
	}
	if len(toolCounts) > 0 {
		parts := []string{}
		for t, n := range toolCounts {
			parts = append(parts, fmt.Sprintf("%s×%d", t, n))
		}
		fmt.Fprintf(&b, "## Tool activity (observed)\n\n%s\n\n", strings.Join(parts, ", "))
	}
	if lastAgent != "" {
		fmt.Fprintf(&b, "## Last agent state (observed %s)\n\n%s\n\n", lastAgentAnchor, truncate(lastAgent, 1500))
	}
	b.WriteString("## Next actions (user-asserted — EDIT ME)\n\n- [ ] …\n\n")
	if d.Unparsed > 0 {
		fmt.Fprintf(&b, "> Coverage: %d of %d source lines were not normalized; this handoff may be missing events.\n", d.Unparsed, d.Lines)
	}

	return HandoffDraft{Markdown: b.String(), SuggestedDir: d.Project, SessionRef: ref}
}

func handleHandoffPublish(w http.ResponseWriter, r *http.Request) {
	var req PublishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Dir == "" || req.Markdown == "" {
		http.Error(w, "dir and markdown are required", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(req.Dir)
	if err != nil || !info.IsDir() {
		http.Error(w, "target directory does not exist: "+req.Dir, http.StatusBadRequest)
		return
	}
	res := PublishResult{}

	cpDir := filepath.Join(req.Dir, ".crossing-guard")
	if err := os.MkdirAll(cpDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hoPath := filepath.Join(cpDir, "handoff.md")
	if err := os.WriteFile(hoPath, []byte(req.Markdown), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res.Written = append(res.Written, hoPath)

	if req.GitIgnore {
		if note := ensureGitignore(req.Dir, ".crossing-guard/"); note != "" {
			res.Notes = append(res.Notes, note)
		} else {
			res.Written = append(res.Written, filepath.Join(req.Dir, ".gitignore")+" (+.crossing-guard/)")
		}
	}
	block := markerStart + "\n" + req.Markdown + "\n" + markerEnd
	if req.WriteClaude {
		p := filepath.Join(req.Dir, "CLAUDE.md")
		if err := upsertDelimitedBlock(p, block); err == nil {
			res.Written = append(res.Written, p)
			res.Notes = append(res.Notes, "CLAUDE.md is NOT git-ignored — committed handoffs are effectively undeletable (git history). Review before committing.")
		}
	}
	if req.WriteAgents {
		p := filepath.Join(req.Dir, "AGENTS.md")
		if err := upsertDelimitedBlock(p, block); err == nil {
			res.Written = append(res.Written, p)
			res.Notes = append(res.Notes, "AGENTS.md is NOT git-ignored — same caution applies.")
		}
	}
	writeJSON(w, res)
}

// upsertDelimitedBlock replaces an existing marker block or appends one —
// idempotent, marker-delimited edits per the deployment doc's rules.
func upsertDelimitedBlock(path, block string) error {
	existing, _ := os.ReadFile(path)
	s := string(existing)
	if i := strings.Index(s, markerStart); i >= 0 {
		if j := strings.Index(s, markerEnd); j > i {
			s = s[:i] + block + s[j+len(markerEnd):]
		} else {
			s = s[:i] + block
		}
	} else {
		if len(s) > 0 && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += "\n" + block + "\n"
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

func ensureGitignore(dir, entry string) string {
	p := filepath.Join(dir, ".gitignore")
	b, _ := os.ReadFile(p)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == entry {
			return "" // already present
		}
	}
	s := string(b)
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	s += entry + "\n"
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		return "could not update .gitignore: " + err.Error()
	}
	return ""
}
