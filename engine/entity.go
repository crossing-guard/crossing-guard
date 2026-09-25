package engine

import (
	"path/filepath"
	"regexp"
	"strings"
)

// EntityID returns the canonical governance identity of a resource:
// "<kind>:<canonical>". This is the stable key entity state accretes under, and it
// is vendor-neutral — a file, db, mcp tool, or url is the SAME entity whichever
// agent (Claude, Codex, …) touches it, which is what makes cross-vendor governance
// possible (governance-model.md rev. 3).
//
// Canonicalization here is PURE (no filesystem I/O) so it is deterministic and
// testable. For files, the caller resolves the absolute + symlink-cleaned path at
// observe time (where the I/O belongs); this function only lexically cleans it.
// Cross-machine file identity is out of scope — this is a local-first tool.
func EntityID(kind, identity string) string {
	switch kind {
	case "file":
		return "file:" + filepath.Clean(strings.TrimSpace(identity))
	case "url":
		return "url:" + urlHost(identity)
	case "mcp":
		return "mcp:" + BareTool(strings.TrimSpace(identity))
	default: // session (vendor/id), memory (id), db (source-label), …
		return kind + ":" + strings.TrimSpace(identity)
	}
}

// BareTool strips an MCP transport prefix (mcp__server__tool -> tool) so state keys
// on the capability, not the wire name. This is the ONE canonical home; the copies
// in guardcli and daemon are duplication to retire onto this (tracked DRY cleanup).
func BareTool(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		if i := strings.LastIndex(name, "__"); i >= 0 {
			return name[i+2:]
		}
	}
	return name
}

var urlSchemeRe = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://`)

// urlHost reduces a URL to its lowercased host (scheme://user:pass@host:port/path
// -> host). A bare host or host/path is handled too, so a source-map host and an
// observed URL resolve to the same entity. Userinfo is stripped (else the ':' in
// user:pass would be read as the port delimiter and collapse every credentialed URL
// to one entity), and an IPv6 literal keeps its brackets as the host identity.
func urlHost(u string) string {
	h := strings.ToLower(strings.TrimSpace(u))
	h = urlSchemeRe.ReplaceAllString(h, "")
	h = strings.TrimPrefix(h, "//")
	// strip userinfo (user:pass@) only when the '@' precedes any path/query
	if at := strings.LastIndex(h, "@"); at >= 0 {
		if sep := strings.IndexAny(h, "/?#"); sep < 0 || at < sep {
			h = h[at+1:]
		}
	}
	// IPv6 literal: [::1]:port/path -> [::1]
	if strings.HasPrefix(h, "[") {
		if end := strings.Index(h, "]"); end >= 0 {
			return h[:end+1]
		}
	}
	if i := strings.IndexAny(h, "/:?#"); i >= 0 {
		h = h[:i]
	}
	return h
}
