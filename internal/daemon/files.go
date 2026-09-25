package daemon

// File search for '@' mentions in the fallback chat composer. Bounded walk
// (no dead air: caps on entries visited and results returned), fuzzy
// subsequence match, repo-noise directories skipped.

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "__pycache__": true,
	"dist": true, "build": true, ".next": true, ".venv": true,
}

const (
	maxWalk    = 20000 // entries visited
	maxResults = 20
)

// SearchFiles returns workspace-relative paths under root fuzzy-matching q.
func SearchFiles(root, q string) []string {
	if root == "" {
		root, _ = os.Getwd()
	}
	q = strings.ToLower(q)
	var out []string
	visited := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > maxWalk || len(out) >= maxResults*4 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if q == "" || fuzzyMatch(strings.ToLower(rel), q) {
			out = append(out, rel)
		}
		return nil
	})
	// shorter paths first — the file you meant is rarely six directories deep
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}
		return out[i] < out[j]
	})
	if len(out) > maxResults {
		out = out[:maxResults]
	}
	return out
}

// fuzzyMatch: every rune of q appears in s, in order (subsequence).
func fuzzyMatch(s, q string) bool {
	i := 0
	for _, c := range s {
		if i < len(q) && byte(c) == q[i] {
			i++
		}
	}
	return i == len(q)
}
