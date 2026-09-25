package daemon

// Daemon adapters for the shared reference-index core. HTTP status mapping and
// handler naming stay here; parsing/resolution/indexing live in internal/refindex.

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	core "crossing-guard/internal/refindex"
)

type RefState = core.RefState
type RefKind = core.RefKind
type RefDef = core.RefDef
type RefTarget = core.RefTarget
type RefManifest = core.RefManifest
type RefDoc = core.RefDoc
type refIndex = core.Index

const (
	RefResolved   = core.RefResolved
	RefMissing    = core.RefMissing
	RefStale      = core.RefStale
	RefUnresolved = core.RefUnresolved
	RefExternal   = core.RefExternal
	RefKindPath   = core.RefKindPath
	RefKindDoc    = core.RefKindDoc
	RefKindID     = core.RefKindID
	RefKindURL    = core.RefKindURL
)

var ErrRefRequest = core.ErrRefRequest

const (
	refIndexTTL      = 5 * time.Second
	refIndexCacheMax = 8
	refBuildTimeout  = 4 * time.Second
)

type refCacheEntry struct {
	index   *refIndex
	builtAt time.Time
}

type refBuild struct {
	done  chan struct{}
	index *refIndex
	err   error
}

var (
	refIndexMu     sync.Mutex
	refIndexCached = map[string]refCacheEntry{}
	refBuilds      = map[string]*refBuild{}
)

func RefIndexManifest(root string) (*RefManifest, error) {
	idx, err := loadRefIndex(root)
	if err != nil {
		return nil, err
	}
	return idx.Manifest(), nil
}

func ResolveRef(root, token string) (*RefTarget, error) {
	idx, err := loadRefIndex(root)
	if err != nil {
		return nil, err
	}
	return idx.Resolve(token), nil
}

func ReadRefDoc(root, rel string) (*RefDoc, error) { return core.ReadRefDoc(root, rel) }
func projectRoot(root string) (string, error)      { return core.ProjectRoot(root) }

// loadRefIndex is daemon-only caching/timeout policy around the shared parser.
func loadRefIndex(root string) (*refIndex, error) {
	root, err := core.ProjectRoot(root)
	if err != nil {
		return nil, err
	}
	refIndexMu.Lock()
	if cached, ok := refIndexCached[root]; ok && time.Since(cached.builtAt) < refIndexTTL {
		refIndexMu.Unlock()
		return cached.index, nil
	}
	build := refBuilds[root]
	if build == nil {
		build = &refBuild{done: make(chan struct{})}
		refBuilds[root] = build
		go runRefBuild(root, build)
	}
	refIndexMu.Unlock()
	select {
	case <-build.done:
		return build.index, build.err
	case <-time.After(refBuildTimeout):
		return nil, fmt.Errorf("%w: indexing %s timed out after %s — the daemon may not have permission to read this directory or the tree is unusually large", ErrRefRequest, root, refBuildTimeout)
	}
}

func runRefBuild(root string, build *refBuild) {
	idx, err := core.Build(root)
	refIndexMu.Lock()
	if err == nil {
		if len(refIndexCached) >= refIndexCacheMax {
			refIndexCached = map[string]refCacheEntry{}
		}
		refIndexCached[root] = refCacheEntry{index: idx, builtAt: time.Now()}
	}
	delete(refBuilds, root)
	refIndexMu.Unlock()
	build.index, build.err = idx, err
	close(build.done)
}

func refRequestStatus(err error) int {
	if errors.Is(err, ErrRefRequest) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
