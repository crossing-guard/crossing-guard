package guardcli

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestStructuredFilePathsFromApplyPatch(t *testing.T) {
	var in hookInput
	in.ToolName = "apply_patch"
	in.Cwd = filepath.Join(string(filepath.Separator), "repo")
	in.ToolInput.Command = commandField("*** Begin Patch\n*** Update File: a.go\n@@\n+content\n*** Delete File: old.go\n*** Add File: sub/new.go\n*** Move to: moved.go\n*** Update File: a.go\n*** End Patch")
	want := []string{
		filepath.Join(in.Cwd, "a.go"), filepath.Join(in.Cwd, "old.go"),
		filepath.Join(in.Cwd, "sub", "new.go"), filepath.Join(in.Cwd, "moved.go"),
	}
	if got := structuredFilePaths(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %#v, want %#v", got, want)
	}
}

func TestStructuredFilePathsRejectsUngroundedInputs(t *testing.T) {
	var in hookInput
	in.ToolName = "apply_patch"
	in.Cwd = filepath.Join(string(filepath.Separator), "repo")
	in.ToolInput.Command = commandField("*** Begin Patch\n *** Update File: prose.go\n+*** Update File: ../escape.go\n+not a header *** Add File: fake.go\n*** Update File:\n*** End Patch")
	if got := structuredFilePaths(in); len(got) != 0 {
		t.Fatalf("ungrounded paths accepted: %#v", got)
	}
	in.Cwd = ""
	in.ToolInput.Command = commandField("*** Begin Patch\n*** Update File: relative.go\n*** End Patch")
	if got := structuredFilePaths(in); len(got) != 0 {
		t.Fatalf("relative path accepted without cwd: %#v", got)
	}
}

func TestStructuredFilePathsDoesNotParseShell(t *testing.T) {
	var in hookInput
	in.ToolName = "Bash"
	in.Cwd = filepath.Join(string(filepath.Separator), "repo")
	in.ToolInput.Command = commandField("apply_patch <<'PATCH'\n*** Update File: hidden.go\nPATCH")
	if got := structuredFilePaths(in); len(got) != 0 {
		t.Fatalf("shell text was parsed as structured targets: %#v", got)
	}
}
