// Command detectscan runs detector sets over every harvested session and prints
// tag-frequency reports. Development harness for the detection library;
// not part of the shipped CLI.
//
// Batch 3: ROLE-SCOPED text detectors. Agent-behavior detectors run only on
// assistant-role events, user-behavior detectors only on user-role events —
// the Event.Kind (role) this batch adds. Plus the compound "unverified-claim":
// the agent claimed done/verified in a session that never ran a test.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"crossing-guard/engine"
	"crossing-guard/harvest"
)

var shellTools = map[string]bool{"Bash": true, "shell": true, "local_shell": true, "exec_command": true}

var (
	hostRe    = regexp.MustCompile(`^[a-z]+://([^/:?#]+)`)
	kvRe      = regexp.MustCompile(`"(command|file_path|path|notebook_path|url|pattern)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	kvArrayRe = regexp.MustCompile(`"command"\s*:\s*\[([^\]]*)\]`)
	quotedRe  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
)

func parseToolInput(text string) map[string]string {
	out := map[string]string{}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) == nil {
		for k, v := range m {
			out[k] = flatten(v)
		}
		return out
	}
	for _, mm := range kvArrayRe.FindAllStringSubmatch(text, -1) {
		var toks []string
		for _, p := range quotedRe.FindAllStringSubmatch(mm[1], -1) {
			toks = append(toks, p[1])
		}
		out["command"] = strings.Join(toks, " ")
	}
	for _, mm := range kvRe.FindAllStringSubmatch(text, -1) {
		if out[mm[1]] == "" {
			out[mm[1]] = mm[2]
		}
	}
	return out
}

func flatten(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var toks []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				toks = append(toks, s)
			}
		}
		return strings.Join(toks, " ")
	default:
		return ""
	}
}

func host(u string) string {
	if m := hostRe.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

func deriveActionEvent(name, rawInput string) engine.Event {
	tool := engine.BareTool(name)
	in := parseToolInput(rawInput)
	ev := engine.Event{Tool: tool}
	if p := in["file_path"]; p != "" {
		ev.Path = p
	} else if p := in["notebook_path"]; p != "" {
		ev.Path = p
	} else if p := in["path"]; p != "" {
		ev.Path = p
	}
	if u := in["url"]; u != "" {
		ev.Destination = host(u)
	}
	if shellTools[tool] {
		ev.Text = in["command"]
	}
	return ev
}

type stat struct {
	fires    int
	sessions map[string]bool
}

func bump(m map[string]*stat, k, sid string) {
	s := m[k]
	if s == nil {
		s = &stat{sessions: map[string]bool{}}
		m[k] = s
	}
	s.fires++
	s.sessions[sid] = true
}

func main() {
	agentDets, err := engine.LoadDetectors("experiments/detect/library-batch3-agent.json")
	must(err)
	userDets, err := engine.LoadDetectors("experiments/detect/library-batch3-user.json")
	must(err)
	actionDets, err := engine.LoadDetectors("experiments/detect/library-batch2.json")
	must(err)

	sessions := harvest.ScanSessions()

	agent := map[string]*stat{}
	user := map[string]*stat{}
	var nAssistant, nUser int
	// compound counters
	var claimSessions, unverifiedClaim, pushbackThenComply int

	for _, s := range sessions {
		events, _, _, err := harvest.Normalize(s.Runtime, s.Path)
		if err != nil {
			continue
		}
		sid := s.Runtime + "/" + s.ID
		hadTest, hadClaim, hadPushback, pushbackIdx, complyAfter := false, false, false, -1, false
		i := 0
		for _, ce := range events {
			switch ce.Kind {
			case "assistant":
				nAssistant++
				for _, t := range engine.Classify(engine.Event{Text: ce.Text}, agentDets) {
					bump(agent, t.Value, sid)
					switch t.Detector {
					case "agent.claim-done", "agent.claim-verified":
						hadClaim = true
					case "agent.pushback":
						hadPushback, pushbackIdx = true, i
					}
				}
			case "user":
				nUser++
				for _, t := range engine.Classify(engine.Event{Text: ce.Text}, userDets) {
					bump(user, t.Value, sid)
				}
			case "tool_call":
				ev := deriveActionEvent(ce.Name, ce.Text)
				for _, t := range engine.Classify(ev, actionDets) {
					if t.Detector == "test.run" {
						hadTest = true
					}
				}
				if hadPushback && pushbackIdx >= 0 && i > pushbackIdx {
					complyAfter = true // a tool action followed a pushback statement
				}
			}
			i++
		}
		if hadClaim {
			claimSessions++
			if !hadTest {
				unverifiedClaim++
			}
		}
		if hadPushback && complyAfter {
			pushbackThenComply++
		}
	}

	nSess := len(sessions)
	fmt.Printf("Sessions: %d   assistant turns: %d   user turns: %d\n\n", nSess, nAssistant, nUser)

	fmt.Println("AGENT chat behaviors  (behavior : fires : sessions : % of sessions)")
	fmt.Println(strings.Repeat("-", 62))
	printStats(agent, nSess)

	fmt.Println("\nUSER chat behaviors  (behavior : fires : sessions : % of sessions)")
	fmt.Println(strings.Repeat("-", 62))
	printStats(user, nSess)

	fmt.Println("\nCOMPOUND signals (derived over the session)")
	fmt.Println(strings.Repeat("-", 62))
	fmt.Printf("  sessions with a done/verified claim : %d (%.0f%%)\n", claimSessions, pct(claimSessions, nSess))
	fmt.Printf("  of those, ran NO test  (unverified-claim) : %d (%.0f%% of claims)\n", unverifiedClaim, pct(unverifiedClaim, claimSessions))
	fmt.Printf("  pushback-then-acted (hedge-then-comply)   : %d (%.0f%%)\n", pushbackThenComply, pct(pushbackThenComply, nSess))
}

func printStats(m map[string]*stat, nSess int) {
	type row struct {
		k           string
		fires, sess int
	}
	rows := make([]row, 0, len(m))
	for k, s := range m {
		rows = append(rows, row{k, s.fires, len(s.sessions)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].sess != rows[j].sess {
			return rows[i].sess > rows[j].sess
		}
		return rows[i].k < rows[j].k
	})
	for _, r := range rows {
		fmt.Printf("  %-18s %8d   %6d   %4.0f%%\n", r.k, r.fires, r.sess, pct(r.sess, nSess))
	}
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
