package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stateKeyDoc is a detector document declaring one source detector with the given tag key.
func stateKeyDoc(id, key string, disabled bool) string {
	d := `"disabled":false`
	if disabled {
		d = `"disabled":true`
	}
	return `{"detectors":[{"id":"` + id + `","kind":"source","match":{"tool":["Bash"]},` +
		`"tag":{"key":"` + key + `","value":"x"},` + d + `,"coverage":{"enumerable":true}}]}`
}

// TestDetectorStateNamespaceRejected pins that a detector cannot emit a key in a state
// namespace through any load path — one row per prefix so dropping any one prefix from
// IsStateTag fails here, and a tombstone is not an exemption (Classify ignores Disabled).
func TestDetectorStateNamespaceRejected(t *testing.T) {
	for _, key := range []string{"session:uncommitted", "target:sensitivity", "agent:helper:done"} {
		for _, disabled := range []bool{false, true} {
			doc := stateKeyDoc("custom.state", key, disabled)
			path := filepath.Join(t.TempDir(), "detectors.json")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			check := func(via string, err error) {
				t.Helper()
				if err == nil {
					t.Fatalf("%s accepted state key %q (disabled=%t)", via, key, disabled)
				}
				for _, want := range []string{"custom.state", key, "state namespace"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("%s error %q does not name %q", via, err, want)
					}
				}
			}
			_, err := LoadDetectors(path)
			check("LoadDetectors", err)
			_, err = LoadLayered(path)
			check("LoadLayered overlay", err)
			_, err = parseDetectors([]byte(doc))
			check("parseDetectors", err)
		}
	}
}

// TestDetectorNearStateKeysLoad pins exact-byte prefix matching: the shipped bare key
// "agent" and near misses are ordinary detector keys.
func TestDetectorNearStateKeysLoad(t *testing.T) {
	for _, key := range []string{"agent", "session", "target", "Session:x", "sessionx", "agent.apology"} {
		if _, err := parseDetectors([]byte(stateKeyDoc("custom.near", key, false))); err != nil {
			t.Fatalf("key %q rejected: %v", key, err)
		}
	}
}

// TestShippedDetectorsHaveNoStateKeys pins that neither embedded document emits a state key.
func TestShippedDetectorsHaveNoStateKeys(t *testing.T) {
	for name, load := range map[string]func() ([]Detector, error){
		"default": DefaultDetectors, "structural": StructuralDetectors,
	} {
		dets, err := load()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, d := range dets {
			if IsStateTag(d.Tag.Key) {
				t.Errorf("%s detector %s emits state key %q", name, d.ID, d.Tag.Key)
			}
		}
	}
}
