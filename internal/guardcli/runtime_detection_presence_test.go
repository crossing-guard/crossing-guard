package guardcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// A runtime's Installed is only a presence fact when its installer reports
// presence; the console shows "not reported" for the rest rather than a guess.
func TestDetectRuntimesSaysWhoReportsPresence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reporters := 0
	for _, runtime := range DetectRuntimes() {
		_, reports := hookInstallers[runtime.Name].(RuntimePresenceReporter)
		if runtime.PresenceReported != reports {
			t.Fatalf("%s: PresenceReported = %v, installer reports presence = %v", runtime.Name, runtime.PresenceReported, reports)
		}
		if reports {
			reporters++
		}
	}
	if reporters == 0 {
		t.Fatal("no installer reports presence; the field would be dead")
	}
	fresh, memoized := DetectRuntimes(), DetectRuntimesMemoized()
	if len(fresh) != len(memoized) {
		t.Fatalf("memoized detection lists %d runtimes, fresh lists %d", len(memoized), len(fresh))
	}
	for index := range fresh {
		if fresh[index].Name != memoized[index].Name || fresh[index].PresenceReported != memoized[index].PresenceReported {
			t.Fatalf("row %d differs: %+v vs %+v", index, fresh[index], memoized[index])
		}
	}
}

// The version is the link-time revision when the build script passed one, and never
// that revision joined with Go's stamp: the two can name different commits.
func TestVersionFromPrefersTheLinkTimeRevision(t *testing.T) {
	const linked, stamped = "1111111111111111111111111111111111111111+dirty", "2222222222222222222222222222222222222222"
	for _, c := range []struct{ name, linked, module, stamped, want string }{
		{"link revision alone", linked, "(devel)", "", linked},
		{"link revision beats a stamp", linked, "(devel)", stamped, linked},
		{"link revision beats a module version and a stamp", linked, "v1.2.3", stamped, linked},
		{"a stamp over devel", "", "(devel)", stamped, stamped},
		{"a stamp over no version", "", "", stamped, stamped},
		{"a module version and a stamp", "", "v1.2.3", stamped, "v1.2.3+" + stamped},
		{"a module version alone", "", "v1.2.3", "", "v1.2.3"},
		{"neither", "", "(devel)", "", "(devel)"},
	} {
		if got := versionFrom(c.linked, c.module, c.stamped); got != c.want {
			t.Fatalf("%s: versionFrom = %q, want %q", c.name, got, c.want)
		}
	}
}

// The device report and the team server's enrollment check bound daemon.version; the
// form the build script passes must fit, or a linked device could not report.
func TestLinkTimeVersionFitsTheDeviceReportSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "device-report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Daemon struct {
				Properties struct {
					Version struct {
						MaxLength int    `json:"maxLength"`
						Pattern   string `json:"pattern"`
					} `json:"version"`
				} `json:"properties"`
			} `json:"daemon"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	bound := schema.Properties.Daemon.Properties.Version
	if bound.MaxLength == 0 || bound.Pattern == "" {
		t.Fatalf("the schema no longer bounds daemon.version here: %+v", bound)
	}
	version := versionFrom(strings.Repeat("a", 40)+"+dirty", "(devel)", strings.Repeat("b", 40))
	if len(version) > bound.MaxLength || !regexp.MustCompile(bound.Pattern).MatchString(version) {
		t.Fatalf("%q (%d chars) does not fit maxLength %d, pattern %s", version, len(version), bound.MaxLength, bound.Pattern)
	}
}

// The build script names the variable by its full path; -X on a path that names
// nothing is silent, so the path is pinned to this package.
func TestReloadScriptStampsThisPackagesRevision(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "reload.sh"))
	if err != nil {
		t.Fatal(err)
	}
	type marker struct{}
	symbol := "-X " + reflect.TypeOf(marker{}).PkgPath() + ".buildRevision="
	for _, pin := range []string{symbol, "-buildvcs=false", "git rev-parse --verify HEAD", "git status --porcelain --untracked-files=normal"} {
		if !strings.Contains(string(script), pin) {
			t.Errorf("scripts/reload.sh lost %q", pin)
		}
	}
	var _ *string = &buildRevision // -X sets a string variable: this fails to compile if it becomes a constant or moves
}
