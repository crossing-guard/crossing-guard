package rulebook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"crossing-guard/engine"
	"crossing-guard/ruledoc"
)

// InvocationPolicy is the deprecated event-local compatibility tier. Keeping its
// resolution here makes rulebook the one policy-document owner without pretending
// this invocation file is the selected durable rulebook.
type InvocationPolicy struct {
	Policy    *engine.Policy `json:"-"`
	Path      string         `json:"path"`
	Origin    string         `json:"origin"`
	Digest    string         `json:"digest,omitempty"`
	Available bool           `json:"available"`
}

func InvocationPolicyPath() string {
	if path := os.Getenv("CG_POLICY"); path != "" {
		return path
	}
	if executable, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(executable), "policy.json")
		if fileExists(path) {
			return path
		}
	}
	return "policy.json"
}

func LoadInvocationPolicy() (*InvocationPolicy, error) {
	path := InvocationPolicyPath()
	origin := "relative-compatibility-file"
	if os.Getenv("CG_POLICY") != "" {
		origin = "environment-invocation"
	} else if filepath.IsAbs(path) {
		origin = "executable-compatibility-file"
	}
	return loadInvocationPolicyPath(path, origin)
}

// LoadInvocationPolicyPath is used by the daemon's explicit flag/environment/legacy
// data-root tier. Absence is a typed unavailable result; malformed presence is loud.
func LoadInvocationPolicyPath(path, origin string) (*InvocationPolicy, error) {
	return loadInvocationPolicyPath(path, origin)
}

func loadInvocationPolicyPath(path, origin string) (*InvocationPolicy, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &InvocationPolicy{Path: path, Origin: origin, Available: false}, nil
	}
	if err != nil {
		return nil, err
	}
	policy, err := ruledoc.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &InvocationPolicy{Policy: policy, Path: path, Origin: origin,
		Digest: ruledoc.ContentDigest(raw), Available: true}, nil
}
