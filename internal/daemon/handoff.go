package daemon

// handoff.go — the handoff extract: a mechanical draft of a session, made from its
// canonical events, which the person edits before it is sent (team rest-of-release
// plan §6.1). Nothing here writes into a checkout: the publish route and its
// file-writing helpers are gone (§6.6 "nothing in a checkout", criterion 69), and what
// earlier versions left behind is removed by CleanCheckoutHandoff.
//
// Anchors: every extracted statement carries [runtime/session#seq] — the
// source-reference format the memory track needs.
//
// Honesty: extraction is MECHANICAL (first user ask, files touched, commands run, last
// agent text). It is labeled observed-with-anchors and makes no claim to be a
// synthesis. What remains is not extracted at all: it is the sender's own list, which
// the send sheet prefills from the last agent message and the person edits (OD-12).

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// HandoffDraft is the extract offered to the send sheet. Title and Remaining are
// prefills the person edits; Markdown is the body's draft.
type HandoffDraft struct {
	Title      string   `json:"title"`
	Markdown   string   `json:"markdown"`
	Remaining  []string `json:"remaining"`
	SessionRef string   `json:"session_ref"`
}

// The marker pair earlier versions wrote around a handoff block in CLAUDE.md and
// AGENTS.md. Nothing writes it any more; CleanCheckoutHandoff removes it.
const (
	markerStart = "<!-- crossing-guard:handoff:start -->"
	markerEnd   = "<!-- crossing-guard:handoff:end -->"
)

// Bounds of the mechanical extract: how many commands it lists and how much of each
// quoted passage it keeps. They shape a draft a person edits, not a limit on what
// leaves — the wire schema and the server bound that.
const (
	extractCommandsMax    = 12
	extractCommandChars   = 90
	extractTitleChars     = 80
	extractObjectiveChars = 1200
	extractLastAgentChars = 1500
	extractRemainingMax   = 20
	extractRemainingChars = 300
)

func handleHandoffGenerate(w http.ResponseWriter, r *http.Request) {
	runtime, id := r.URL.Query().Get("runtime"), r.URL.Query().Get("id")
	d, err := LoadSession(runtime, id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, generateHandoff(d, handoffCheckout(d).root))
}

var fileRe = regexp.MustCompile(`"file_path"\s*:\s*"([^"]+)"`)
var cmdRe = regexp.MustCompile(`"command"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// generateHandoff builds the extract. Paths under checkoutRoot are emitted
// repository-relative; the send's portable-text check covers every other path shape.
func generateHandoff(d *SessionDetail, checkoutRoot string) HandoffDraft {
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
				path := repositoryRelative(m[1], checkoutRoot)
				if _, seen := files[path]; !seen {
					files[path] = ev.Seq
				}
			}
			if m := cmdRe.FindStringSubmatch(ev.Text); m != nil && len(cmds) < extractCommandsMax {
				c := strings.ReplaceAll(m[1], `\"`, `"`)
				cmds = append(cmds, "`"+truncate(c, extractCommandChars)+"`  "+anchor(ev.Seq))
			}
		}
	}

	title := truncate(strings.Split(objective, "\n")[0], extractTitleChars)
	var b strings.Builder
	fmt.Fprintf(&b, "- source-session: %s\n- generated: %s\n- generator: mechanical extract (not a synthesis — edit before sending)\n\n",
		ref, time.Now().UTC().Format(time.RFC3339))
	// A session whose first prompt was not observed has no objective to quote: the
	// heading is left out, never sent empty.
	if strings.TrimSpace(objective) != "" {
		fmt.Fprintf(&b, "## Objective (observed %s)\n\n%s\n\n", objAnchor, truncate(objective, extractObjectiveChars))
	}

	if len(files) > 0 {
		b.WriteString("## Files touched (observed)\n\n")
		paths := make([]string, 0, len(files))
		for path := range files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			fmt.Fprintf(&b, "- `%s`  %s\n", path, anchor(files[path]))
		}
		b.WriteString("\n")
	}
	if len(cmds) > 0 {
		fmt.Fprintf(&b, "## Commands run (observed, first %d)\n\n", extractCommandsMax)
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
		sort.Strings(parts)
		fmt.Fprintf(&b, "## Tool activity (observed)\n\n%s\n\n", strings.Join(parts, ", "))
	}
	if lastAgent != "" {
		fmt.Fprintf(&b, "## Last agent state (observed %s)\n\n%s\n\n", lastAgentAnchor, truncate(lastAgent, extractLastAgentChars))
	}
	if d.Unparsed > 0 {
		fmt.Fprintf(&b, "> Coverage: %d of %d source lines were not normalized; this handoff may be missing events.\n", d.Unparsed, d.Lines)
	}
	return HandoffDraft{Title: title, Markdown: b.String(), Remaining: remainingPrefill(lastAgent), SessionRef: ref}
}

// repositoryRelative renders a path under root relative to it, with forward slashes;
// any other path is returned as it was.
func repositoryRelative(path, root string) string {
	if root == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(rel)
}

// listItemRe matches one Markdown list line: a bullet, a numbered item, or a checkbox.
var listItemRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?(.+)$`)

// remainingPrefill offers the list items of the session's last agent message as the
// sender's starting list (OD-12). It is read from what the session already said; no
// action asks the session anything, and the person edits or discards every item.
func remainingPrefill(lastAgent string) []string {
	items := []string{}
	for _, line := range strings.Split(lastAgent, "\n") {
		m := listItemRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if item := strings.TrimSpace(m[1]); item != "" {
			items = append(items, truncate(item, extractRemainingChars))
		}
		if len(items) == extractRemainingMax {
			break
		}
	}
	return items
}

// HandoffLeftover is what an earlier version left in one checkout: the handoff file
// and the marker blocks in CLAUDE.md and AGENTS.md.
type HandoffLeftover struct {
	Dir string `json:"dir"`
	// File is .crossing-guard/handoff.md when it exists.
	File string `json:"file,omitempty"`
	// Blocks names the files holding a whole marker block.
	Blocks []string `json:"blocks,omitempty"`
	// Damaged names the files whose marker pair is not one start followed by one end.
	// They are reported and never edited.
	Damaged []string `json:"damaged,omitempty"`
}

// Any reports whether the checkout holds anything an earlier version wrote.
func (l HandoffLeftover) Any() bool { return l.File != "" || len(l.Blocks) > 0 || len(l.Damaged) > 0 }

// handoffBlockFiles are the files earlier versions wrote a marker block into.
var handoffBlockFiles = []string{"CLAUDE.md", "AGENTS.md"}

// markerBlock locates the one whole marker block in text. damaged is true when the
// markers are present but are not exactly one start followed by one end.
func markerBlock(text string) (start, end int, found, damaged bool) {
	starts, ends := strings.Count(text, markerStart), strings.Count(text, markerEnd)
	if starts == 0 && ends == 0 {
		return 0, 0, false, false
	}
	start, end = strings.Index(text, markerStart), strings.Index(text, markerEnd)
	if starts != 1 || ends != 1 || end < start {
		return 0, 0, false, true
	}
	return start, end + len(markerEnd), true, false
}

// InspectCheckoutHandoff reports what an earlier version left in dir, changing nothing.
func InspectCheckoutHandoff(dir string) (HandoffLeftover, error) {
	left := HandoffLeftover{Dir: dir}
	file := filepath.Join(dir, ".crossing-guard", "handoff.md")
	if _, err := os.Stat(file); err == nil {
		left.File = file
	} else if !errors.Is(err, os.ErrNotExist) {
		return left, err
	}
	for _, name := range handoffBlockFiles {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return left, err
		}
		switch _, _, found, damaged := markerBlock(string(raw)); {
		case damaged:
			left.Damaged = append(left.Damaged, path)
		case found:
			left.Blocks = append(left.Blocks, path)
		}
	}
	return left, nil
}

// CleanCheckoutHandoff removes what an earlier version left in dir: the handoff file
// and each whole marker block, with the blank line the block was appended after. A
// damaged marker pair is left untouched and reported in the result's Damaged. The
// result names what was removed.
func CleanCheckoutHandoff(dir string) (HandoffLeftover, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return HandoffLeftover{Dir: dir}, err
	}
	if !info.IsDir() {
		return HandoffLeftover{Dir: dir}, fmt.Errorf("%s is not a directory", dir)
	}
	found, err := InspectCheckoutHandoff(dir)
	if err != nil {
		return found, err
	}
	removed := HandoffLeftover{Dir: dir, Damaged: found.Damaged}
	if found.File != "" {
		if err := os.Remove(found.File); err != nil {
			return removed, err
		}
		removed.File = found.File
	}
	for _, path := range found.Blocks {
		if err := removeMarkerBlock(path); err != nil {
			return removed, fmt.Errorf("%s: %w", path, err)
		}
		removed.Blocks = append(removed.Blocks, path)
	}
	return removed, nil
}

// removeMarkerBlock rewrites path without its marker block, keeping the file's mode.
func removeMarkerBlock(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(raw)
	start, end, found, _ := markerBlock(text)
	if !found {
		return nil
	}
	before, after := strings.TrimRight(text[:start], "\n"), strings.TrimLeft(text[end:], "\n")
	out := before
	if before != "" {
		out += "\n"
	}
	if after != "" {
		if before != "" {
			out += "\n"
		}
		out += after
	}
	tmp := path + ".crossing-guard-clean"
	if err := os.WriteFile(tmp, []byte(out), info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CheckoutsWithHandoffLeftovers inspects each directory and returns those that still
// hold a handoff file or marker block from an earlier version — what doctor names. A
// directory that is gone or unreadable is skipped: it holds nothing to clean.
func CheckoutsWithHandoffLeftovers(dirs []string) []HandoffLeftover {
	var out []HandoffLeftover
	for _, dir := range dirs {
		left, err := InspectCheckoutHandoff(dir)
		if err == nil && left.Any() {
			out = append(out, left)
		}
	}
	return out
}
