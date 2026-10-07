package guardcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookWarnChildEnv switches the re-executed test binary into running cmdHook, which
// always ends in os.Exit and so cannot run in the parent test process.
const hookWarnChildEnv = "CG_TEST_HOOK_WARN_CHILD"

func TestHookWarnHelperProcess(t *testing.T) {
	if os.Getenv(hookWarnChildEnv) != "1" {
		t.Skip("helper process for TestHookWarnRuleProceeds")
	}
	cmdHook([]string{"--runtime", "claude"})
	os.Exit(0)
}

// A negation-only state rule is always true in the standalone tier (it never sees
// state), so this warn rule fires on every call — the shape that used to ask/deny.
const hookWarnRules = `{"rules":[
  {"id":"warn-unreviewed","mode":"warn-and-proceed","message":"always warns (a non-state term: the static tier never evaluates state rules)","if":{"not":{"tag":"command","matches":"^never-a-real-command$"}}},
  {"id":"warn-echo","mode":"warn-and-proceed","message":"echo seen","if":{"tag":"command","matches":"^echo warnme"}},
  {"id":"ask-bravo","action":"ask","message":"bravo needs a human","if":{"tag":"command","matches":"bravo"}}
]}`

type hookWarnRun struct {
	stdout string
	lines  []map[string]any
	spool  string // every spooled observation, concatenated (no daemon acknowledged them)
}

// runHookWarnChild runs cmdHook in a child with a sandboxed HOME, no daemon, and
// either no invocation policy (invocation == "") or one holding invocation.
func runHookWarnChild(t *testing.T, command, invocation string) hookWarnRun {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(root, "rules.json")
	if err := os.WriteFile(rules, []byte(hookWarnRules), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "absent-policy.json")
	if invocation != "" {
		policy = filepath.Join(root, "policy.json")
		if err := os.WriteFile(policy, []byte(invocation), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(root, "decisions.jsonl")
	payload, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse",
		"session_id": "ses_warn", "cwd": root, "tool_name": "Bash",
		"tool_input": map[string]any{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHookWarnHelperProcess$")
	cmd.Dir = root // no relative policy.json
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + root,
		hookWarnChildEnv + "=1", "CG_RULES=" + rules, "CG_POLICY=" + policy,
		"CG_LOG=" + log, "CG_GOVERN=", "CG_ASK_BUDGET_MS=200"}
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook child failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	run := hookWarnRun{stdout: stdout.String()}
	_ = filepath.WalkDir(filepath.Join(home, ".crossing-guard"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.Contains(path, "spool") {
			if raw, readErr := os.ReadFile(path); readErr == nil {
				run.spool += string(raw)
			}
		}
		return nil
	})
	if f, err := os.Open(log); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var line map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
				t.Fatalf("decision line is not JSON: %q", scanner.Text())
			}
			run.lines = append(run.lines, line)
		}
	}
	return run
}

func (r hookWarnRun) linesFor(rule string) []map[string]any {
	var out []map[string]any
	for _, line := range r.lines {
		if line["rule"] == rule {
			out = append(out, line)
		}
	}
	return out
}

func TestHookWarnRuleProceeds(t *testing.T) {
	run := runHookWarnChild(t, "ls -la", "")
	if strings.Contains(run.stdout, "deny") {
		t.Fatalf("warn-only match was denied: %s", run.stdout)
	}
	warns := run.linesFor("warn-unreviewed")
	if len(warns) != 1 || len(run.lines) != 1 {
		t.Fatalf("want exactly one warn line, got %v", run.lines)
	}
	if warns[0]["decision"] != "allow" || warns[0]["note"] != "warn-and-proceed" || warns[0]["tier"] != "static" {
		t.Fatalf("warn line does not record a static warn-and-proceed allow: %v", warns[0])
	}
	if !strings.Contains(run.spool, `"reason":"warn-and-proceed: rule warn-unreviewed"`) {
		t.Fatalf("stored observation does not name the warn: %s", run.spool)
	}
}

// Two warn rules on one call: each is logged and named, not only the top one.
func TestHookEveryFiredWarnIsRecorded(t *testing.T) {
	run := runHookWarnChild(t, "echo warnme", "")
	if strings.Contains(run.stdout, "deny") {
		t.Fatalf("warn-only match was denied: %s", run.stdout)
	}
	if len(run.linesFor("warn-unreviewed")) != 1 || len(run.linesFor("warn-echo")) != 1 {
		t.Fatalf("want one line per warned rule, got %v", run.lines)
	}
	if !strings.Contains(run.spool, `"reason":"warn-and-proceed: rule warn-unreviewed, warn-echo"`) {
		t.Fatalf("stored observation does not name both warns: %s", run.spool)
	}
}

// With an invocation policy the engine tier evaluates the user rulebook too; the
// same warn must still be logged once, not once per tier.
func TestHookWarnRuleLoggedOnceAcrossTiers(t *testing.T) {
	run := runHookWarnChild(t, "ls -la", `{"rules":[]}`)
	if strings.Contains(run.stdout, "deny") {
		t.Fatalf("warn-only match was denied: %s", run.stdout)
	}
	if warns := run.linesFor("warn-unreviewed"); len(warns) != 1 {
		t.Fatalf("one warn logged %d times: %v", len(warns), run.lines)
	}
}

func TestHookWarnDoesNotMaskAnAsk(t *testing.T) {
	run := runHookWarnChild(t, "run bravo", "")
	if !strings.Contains(run.stdout, `"permissionDecision":"deny"`) || !strings.Contains(run.stdout, "ask-bravo") {
		t.Fatalf("ask rule firing beside a warn did not gate (no inbox → fail closed): %s", run.stdout)
	}
}
