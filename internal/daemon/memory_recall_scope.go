package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/memory"
	"crossing-guard/store"
)

// Recall scope (team item 5 decision 12, C-6): where a session is, resolved by the DAEMON
// from the cwd the SessionStart hook (or the recall MCP) sends — the hook forks no git
// and makes no network call (invariant 5: this is the loopback daemon). Resolution reuses
// the place resolution the peers route already owns (resolvePlace), bounded by the recall
// git timeout and cached per folder for recall.scope_cache_seconds, so a burst of session
// starts in one checkout costs one resolution. When the folder cannot be resolved in
// time, the scope falls back to the folder name alone: weak records still match by name,
// and remote-identified repository records are simply not recalled.

type recallScopeResponse struct {
	Label        string `json:"label"`
	RepositoryID string `json:"repository_id,omitempty"`
	Resolution   string `json:"resolution"` // git | folder | unresolved
}

type cachedRecallScope struct {
	scope recallScopeResponse
	at    time.Time
}

var recallScopes = struct {
	sync.Mutex
	byFolder map[string]cachedRecallScope
}{byFolder: map[string]cachedRecallScope{}}

// resolveRecallScope answers the scope for cwd. An empty or non-absolute cwd is no scope.
func resolveRecallScope(ctx context.Context, cwd string) recallScopeResponse {
	if cwd == "" || !filepath.IsAbs(cwd) || len(cwd) > maxPeerPlaceBytes {
		return recallScopeResponse{Resolution: placeUnresolved}
	}
	cwd = filepath.Clean(cwd)
	config, _ := consoleConfig()
	ttl := time.Duration(config.Recall.ScopeCacheSeconds) * time.Second
	recallScopes.Lock()
	if c, ok := recallScopes.byFolder[cwd]; ok && time.Since(c.at) < ttl {
		recallScopes.Unlock()
		return c.scope
	}
	recallScopes.Unlock()
	one, cancel := context.WithTimeout(ctx, time.Duration(config.Recall.PeerGitTimeoutMS)*time.Millisecond)
	defer cancel()
	place := resolvePlace(one, cwd)
	scope := recallScopeResponse{Label: filepath.Base(cwd), Resolution: place.kind}
	switch place.kind {
	case placeGit:
		scope.Label = memory.ProjectFromCommonDir(place.repo.CommonDir, place.repo.Root)
		if place.repo.IdentityKind == "remote-sha256" {
			scope.RepositoryID = place.repo.ID
		}
	case placeFolder:
		scope.Label = memory.ProjectFromCommonDir("", place.folder)
	default:
		return scope // not cached: the next call may resolve
	}
	recallScopes.Lock()
	if len(recallScopes.byFolder) >= config.Recall.ScopeCacheMax {
		recallScopes.byFolder = map[string]cachedRecallScope{}
	}
	recallScopes.byFolder[cwd] = cachedRecallScope{scope, time.Now()}
	recallScopes.Unlock()
	return scope
}

// memoryInRecallScope applies recall's one predicate (memory.RecordInScope) to a store
// record.
func memoryInRecallScope(r store.MemoryRecord, scope recallScopeResponse) bool {
	return memory.RecordInScope(memory.Record{ScopeType: string(r.ScopeType), ScopeID: r.ScopeID,
		RepositoryIdentity: r.RepositoryIdentity, Shadowed: r.Collision == "shadowed"},
		memory.RecallScope{Label: scope.Label, RepositoryID: scope.RepositoryID})
}

// mintMemoryScope resolves a folder for a repository-scoped record being CREATED by a
// daemon door (synthesis, the MCP propose door): changeenv.MemoryRepositoryScope, bounded
// by the recall git timeout. A var so a test can pin the answer without a git checkout.
var mintMemoryScope = func(dir, label string) (scopeID, identity, note string) {
	config, _ := consoleConfig()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.Recall.PeerGitTimeoutMS)*time.Millisecond)
	defer cancel()
	return changeenv.MemoryRepositoryScope(ctx, dir, label)
}
