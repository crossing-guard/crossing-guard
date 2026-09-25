package daemon

// claimRefResolver resolves model-authored claim refs AFTER run completion, on
// its own goroutine — never on the event pump, which owns stream-position
// writes and must not block on a cold ref-index build (plan §5, refindex build
// can take 4s cold). Refs are a CONTAINED input class: file/doc resolve through
// the existing refindex owner jailed to the binding's project root, session
// resolves against the harvest catalog in one batched scan, and event refs
// require their session identity (shape-enforced by the claim owner). Failures
// are recorded, never re-targeted.

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"crossing-guard/internal/orchestration"
	"crossing-guard/store"
)

const claimRefQueueDepth = 64

type claimRefJob struct {
	runID       string
	projectRoot string
	findings    []orchestration.Finding
}

type claimRefStore interface {
	MergeManagedRunDetail(runID string, patch map[string]any) error
	EventForSession(sessionID string, eventID int64) (store.EventRecord, error)
}

type claimRefResolver struct {
	store claimRefStore
	// resolvePath and catalogSessionIDs are injectable so tests exercise the
	// component without a real ref index or harvest scan.
	resolvePath       func(root, token string) (*RefTarget, error)
	catalogSessionIDs func() map[string]bool
	now               func() int64

	jobs   chan claimRefJob
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newClaimRefResolver(store claimRefStore) *claimRefResolver {
	ctx, cancel := context.WithCancel(context.Background())
	resolver := &claimRefResolver{
		store:       store,
		resolvePath: ResolveRef,
		catalogSessionIDs: func() map[string]bool {
			ids := map[string]bool{}
			for _, session := range ScanSessions() {
				ids[session.ID] = true
			}
			return ids
		},
		now:    func() int64 { return time.Now().Unix() },
		jobs:   make(chan claimRefJob, claimRefQueueDepth),
		ctx:    ctx,
		cancel: cancel,
	}
	resolver.wg.Add(1)
	go resolver.run()
	return resolver
}

func (resolver *claimRefResolver) close() {
	if resolver == nil {
		return
	}
	resolver.cancel()
	resolver.wg.Wait()
}

// enqueue hands one completed run's findings to the resolver. It never blocks
// the caller: a full queue drops the job and reports false, leaving the refs
// with no resolution field — honestly unresolved, never guessed.
func (resolver *claimRefResolver) enqueue(job claimRefJob) bool {
	if len(job.findings) == 0 {
		return true
	}
	select {
	case resolver.jobs <- job:
		return true
	case <-resolver.ctx.Done():
		return false
	default:
		return false
	}
}

func (resolver *claimRefResolver) run() {
	defer resolver.wg.Done()
	for {
		select {
		case <-resolver.ctx.Done():
			return
		case job := <-resolver.jobs:
			resolver.resolveJob(job)
		}
	}
}

func (resolver *claimRefResolver) resolveJob(job claimRefJob) {
	// Batch: collect every session identity the claim references, then do one
	// catalog scan — never a per-ref O(n) lookup (plan §5).
	var sessionIDs map[string]bool
	needsCatalog := false
	for _, finding := range job.findings {
		for _, ref := range finding.Refs {
			if ref.Kind == "session" {
				needsCatalog = true
			}
		}
	}
	if needsCatalog {
		sessionIDs = resolver.catalogSessionIDs()
	}
	findings := make([]map[string]any, 0, len(job.findings))
	for _, finding := range job.findings {
		refs := make([]map[string]any, 0, len(finding.Refs))
		for _, ref := range finding.Refs {
			status, path := resolver.resolveRef(job.projectRoot, ref, sessionIDs)
			row := map[string]any{"kind": ref.Kind, "path": ref.Path, "resolution": status}
			if ref.Line > 0 {
				row["line"] = ref.Line
			}
			if ref.Session != "" {
				row["session"] = ref.Session
			}
			if ref.Anchor != "" {
				row["anchor"] = ref.Anchor
			}
			if path != "" {
				row["resolved_path"] = path
			}
			refs = append(refs, row)
		}
		row := map[string]any{"severity": finding.Severity, "statement": finding.Statement}
		if len(refs) > 0 {
			row["refs"] = refs
		}
		findings = append(findings, row)
	}
	// A merge failure is recorded nowhere better than the log line the store
	// already emits; the run keeps its unresolved findings rather than lying.
	_ = resolver.store.MergeManagedRunDetail(job.runID,
		map[string]any{"findings": findings, "refs_resolved_at": resolver.now()})
}

func (resolver *claimRefResolver) resolveRef(projectRoot string, ref orchestration.Ref, sessions map[string]bool) (status, path string) {
	switch ref.Kind {
	case "file", "doc":
		target, err := resolver.resolvePath(projectRoot, ref.Path)
		if err != nil || target == nil {
			return string(RefUnresolved), ""
		}
		return string(target.State), target.Path
	case "session":
		identity := ref.Session
		if identity == "" {
			identity = ref.Path
		}
		if sessions[identity] {
			return string(RefResolved), ""
		}
		return string(RefMissing), ""
	case "event":
		// The claim owner enforces that event refs carry session identity; the
		// event id rides Path as decimal digits (or Line when Path is empty).
		eventID, err := strconv.ParseInt(strings.TrimSpace(ref.Path), 10, 64)
		if err != nil && ref.Line > 0 {
			eventID, err = int64(ref.Line), nil
		}
		if err != nil || eventID < 1 {
			return string(RefUnresolved), ""
		}
		if _, lookupErr := resolver.store.EventForSession(ref.Session, eventID); lookupErr != nil {
			return string(RefMissing), ""
		}
		return string(RefResolved), ""
	}
	return string(RefUnresolved), ""
}
