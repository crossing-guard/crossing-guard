package guardcli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func shellInput(t *testing.T, tool, cwd string, command any) hookInput {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return hookInput{ToolName: tool, Cwd: cwd, RawToolInput: raw}
}

func TestShellResourceClaimsCommonCodingCommands(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	tests := []struct {
		name, command             string
		wantRaw, wantKind, wantOp string
	}{
		{"sed read", `sed -n '1,20p' docs/design/README.md`, "docs/design/README.md", "file", "read"},
		{"sed write", `sed -i.bak -e 's/a/b/' src/a.go`, "src/a.go", "file", "write"},
		{"ripgrep root", `rg --glob '*.go' needle internal/guardcli`, "internal/guardcli", "path", "search"},
		{"sqlite database", `sqlite3 state/index.sqlite 'select 1'`, "state/index.sqlite", "file", "unknown"},
		{"checksum", `shasum -a 256 bin/crossing-guard`, "bin/crossing-guard", "file", "read"},
		{"go package", `go test -run TestShell ./internal/guardcli`, "./internal/guardcli", "package-pattern", "execute"},
		{"git explicit pathspec", `git diff -- docs/design/README.md`, "docs/design/README.md", "path", "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := shellResourceClaims(shellInput(t, "Bash", repo, tc.command))
			if len(claims) != 1 {
				t.Fatalf("claims=%+v", claims)
			}
			got := claims[0]
			if got.RawIdentity != tc.wantRaw || got.Kind != tc.wantKind || got.Operation != tc.wantOp || got.EvidenceClass != "declared" {
				t.Fatalf("claim=%+v", got)
			}
			if tc.wantKind != "package-pattern" && got.Identity != filepath.Join(repo, tc.wantRaw) {
				t.Fatalf("identity=%q", got.Identity)
			}
		})
	}
}

func TestShellResourceClaimsPreserveArgvBoundaries(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	claims := shellResourceClaims(shellInput(t, "exec_command", repo,
		[]string{"cat", "a file; still one argument.txt"}))
	if len(claims) != 1 || claims[0].RawIdentity != "a file; still one argument.txt" ||
		claims[0].SourceField != "tool_input.command.segment[0].argv[1]" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestShellResourceClaimsSegmentOrderAndRedirects(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	claims := shellResourceClaims(shellInput(t, "local_shell", repo,
		`cat input.txt | rg needle src > matches.txt; touch done.flag`))
	if len(claims) != 4 {
		t.Fatalf("claims=%+v", claims)
	}
	wantRaw := []string{"input.txt", "src", "matches.txt", "done.flag"}
	wantOps := []string{"read", "search", "write", "write"}
	for i := range wantRaw {
		if claims[i].RawIdentity != wantRaw[i] || claims[i].Operation != wantOps[i] {
			t.Fatalf("claim[%d]=%+v", i, claims[i])
		}
	}
}

func TestShellResourceClaimsLiteralHeredocOutput(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	tests := []struct {
		name, command, target string
	}{
		{"single quoted delimiter", "cat > app/Console/Commands/SearchHealth.php <<'PHP'\n<?php\ncat body.txt\nPHP\ncat after.txt", "app/Console/Commands/SearchHealth.php"},
		{"double quoted delimiter", "cat > \"docs/a b.md\" <<\"DOC\"\nbody\nDOC", "docs/a b.md"},
		{"bare delimiter", "cat > output.txt <<EOF\nbody\nEOF\n", "output.txt"},
		{"crlf delimiter", "cat > output.txt <<EOF\r\nbody\r\nEOF\r\n", "output.txt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := shellResourceClaims(shellInput(t, "Bash", repo, tc.command))
			if len(claims) != 1 || claims[0].RawIdentity != tc.target ||
				claims[0].Operation != "write" || claims[0].EvidenceClass != "declared" ||
				claims[0].SourceField != "tool_input.command.segment[0].redirect[0]" {
				t.Fatalf("claims=%+v", claims)
			}
		})
	}
}

func TestShellResourceClaimsLiteralHeredocKeepsSafePrefixOrder(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	claims := shellResourceClaims(shellInput(t, "Bash", repo,
		"touch before.flag; cat input.txt > output.txt <<EOF\nbody.txt\nEOF\ntouch after.flag"))
	if len(claims) != 3 {
		t.Fatalf("claims=%+v", claims)
	}
	wantRaw := []string{"before.flag", "input.txt", "output.txt"}
	wantOps := []string{"write", "read", "write"}
	for index := range wantRaw {
		if claims[index].RawIdentity != wantRaw[index] || claims[index].Operation != wantOps[index] {
			t.Fatalf("claim[%d]=%+v", index, claims[index])
		}
	}
}

func TestShellResourceClaimsRefuseUnsafeHeredocs(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	commands := []string{
		"cat > \"$FILE\" <<EOF\nbody\nEOF",
		"cat <<EOF\nbody\nEOF",
		"cat > output.txt <<<value",
		"cat > output.txt <<-EOF\nbody\nEOF",
		"cat > output.txt <<EOF\nbody",
		"cat > output.txt <<EOF suffix\nbody\nEOF",
		"cat > output.txt <<EOF\nbody\nEOF suffix",
		"cat > output.txt <<A <<B\nbody\nA\nB",
		"echo $(cat <<EOF\nbody\nEOF\n); cat visible.txt",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			if claims := shellResourceClaims(shellInput(t, "Bash", repo, command)); len(claims) != 0 {
				t.Fatalf("unsafe command %q claims=%+v", command, claims)
			}
		})
	}
}

func TestShellResourceClaimsDoNotMistakeHeredocTextContexts(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	commands := []string{
		"echo '<<EOF'; cat visible.txt",
		"echo nope # <<EOF\ncat visible.txt",
		"printf \\<<; cat visible.txt",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			claims := shellResourceClaims(shellInput(t, "Bash", repo, command))
			if len(claims) != 1 || claims[0].RawIdentity != "visible.txt" || claims[0].Operation != "read" {
				t.Fatalf("command %q claims=%+v", command, claims)
			}
		})
	}
}

func TestShellResourceClaimsIgnoreShellComments(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	claims := shellResourceClaims(shellInput(t, "Bash", repo,
		"cat before.txt # not-a-file.txt\ncat after.txt"))
	if len(claims) != 2 || claims[0].RawIdentity != "before.txt" || claims[1].RawIdentity != "after.txt" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestShellResourceClaimsFailClosed(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	commands := []string{
		`cat "$FILE"`,
		`cat src/*.go`,
		`rg --unknown value needle src`,
		`cp -t destination source`,
		`cat <<EOF`,
		`bash -c 'cat secret.txt'`,
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			if claims := shellResourceClaims(shellInput(t, "Bash", repo, command)); len(claims) != 0 {
				t.Fatalf("unsafe command %q claims=%+v", command, claims)
			}
		})
	}
}

func TestShellResourceClaimsStopAfterStateChange(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	claims := shellResourceClaims(shellInput(t, "Bash", repo,
		`cat before.txt; cd "$OTHER"; cat after.txt`))
	if len(claims) != 1 || claims[0].RawIdentity != "before.txt" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestShellResourceClaimsDoNotChangeNativeRoute(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	in := hookInput{ToolName: "Edit", Cwd: repo, ToolInput: toolInput{FilePath: "src/a.go"},
		RawToolInput: json.RawMessage(`{"file_path":"src/a.go"}`)}
	claims := structuredResourceClaims(in)
	if len(claims) != 1 || claims[0].RawIdentity != "src/a.go" || claims[0].Operation != "write" || claims[0].SourceField != "tool_input.file_path" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestShellResourceClaimsRespectParserBounds(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	command := "cat " + strings.Repeat("a", maxTypedProjectionBytes)
	if claims := shellResourceClaims(shellInput(t, "Bash", repo, command)); len(claims) != 0 {
		t.Fatalf("oversized claims=%+v", claims)
	}
}

func FuzzLexShellCommand(f *testing.F) {
	for _, seed := range []string{"cat a", `sed -n '1p' "a b"`, "cat <<EOF", "cat > out <<EOF\nbody\nEOF", "cd x; cat y", "rg x src > out"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, command string) {
		if len(command) > maxTypedProjectionBytes+1 {
			t.Skip()
		}
		_, _ = literalHeredocPrefix(command)
		_, _ = lexShellCommand(command)
	})
}
