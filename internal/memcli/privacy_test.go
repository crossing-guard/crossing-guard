package memcli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Pin removal of the old unconditional debug tap without creating, changing, or
// deleting the user's predictable temporary file during the test.
func TestMainSourceHasNoLegacyInvocationTrace(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"traceInvocation", "crossing-guard-invocations.log"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("production memcli source retained legacy invocation trace marker %q", forbidden)
		}
	}
}
