package daemon

// The live governor (governance-model.md rev. 3, plan Phase 1a): observe a canonical
// action → classify it (frozen tags) → append the event (primary truth) → fold state
// onto the target entity and the session. This is the OBSERVE + FOLD half; it takes no
// decision (enforcement is Phase 3) and does not touch the live hook. Each Observe
// commits atomically via one store transaction. The in-memory hot-path cache + async
// batching is the Phase-1b latency work, deferred until we deploy and measure.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"crossing-guard/codemap"
	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/internal/analyzerhost"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/collectionconfig"
	"crossing-guard/internal/detectorselection"
	"crossing-guard/internal/observation"
	"crossing-guard/internal/platform"
	"crossing-guard/store"
)

const (
	sessionFileDetailLimit     = 100
	sessionFileValidationLimit = 200
	sessionActionResultLimit   = 25
	sessionActionEffectLimit   = 50
)

// Observation is one canonical action, vendor-neutral. It mirrors what the live hook
// already has (tool + input) plus the session it belongs to.
type Observation struct {
	SessionID string `json:"session"` // "<vendor>/<id>" (or the runtime session id, live)
	// Runtime is WHICH agent produced this action — AS REPORTED BY THE HOOK. In the
	// intended wiring it echoes the --runtime flag the installer baked into that
	// vendor's config, but the daemon cannot verify that: like every Observation
	// field it is a claim trusted at the bearer-token boundary, not an observed
	// fact (the observe handler clamps its shape, nothing more). Empty means a hook
	// older than the flag: recorded as unknown, never inferred, because a guess
	// here would attribute one agent's blocked action to another in the only place
	// that action is recorded at all.
	Runtime  string `json:"runtime,omitempty"`
	Tool     string `json:"tool"`    // as the runtime named it (mcp__… prefixes tolerated)
	Command  string `json:"command"` // shell command channel
	Content  string `json:"content"` // write/edit body — so content detectors scan what is written
	FilePath string `json:"file_path"`
	// FilePaths are exact structured resource targets carried by a tool that can
	// name more than one. They supplement FilePath; they are not shell inference.
	FilePaths []string `json:"file_paths,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	URL       string   `json:"url"`
	Skill     string   `json:"skill"` // Skill tool: WHICH skill was invoked (D6)
	TS        int64    `json:"ts"`
	// What the hook DECIDED about this action, not merely that it happened. A denied
	// call never reaches the vendor transcript (governance-model.md §"blocked"), so
	// this log is the only possible record that enforcement fired. Empty means the
	// caller reported no decision — recorded as such, never guessed at.
	Decision string `json:"decision"` // allow | deny | ask
	Reason   string `json:"reason"`
	// Rule is the rule that produced or asked for Decision; empty is unknown, never guessed.
	Rule string `json:"rule,omitempty"`
	// Layer is the distribution tier that rule arrived by (schema 38): user |
	// repository | organization; empty is unknown, never guessed.
	Layer string `json:"layer,omitempty"`
	// LayerReasons is the layered loader's notes for this action (why a team
	// layer did or did not apply); empty is none. Same no-guessing rule.
	LayerReasons string `json:"layer_reasons,omitempty"`
	// Origin is data LINEAGE: "live" (the hook, as it happened), "transcript" (exact
	// post-hoc vendor evidence), or "imported" (the legacy display-event backfill —
	// lossy, timestamp-folded, never confused with live truth). Empty defaults to
	// "live"; the live observe endpoint forces it, so a hook cannot choose lineage.
	Origin string `json:"origin,omitempty"`
	// ResourceClaims is v1's exact declared-resource evidence. Legacy/import callers
	// leave it empty and keep the historical Normalized/FilePaths behavior.
	ResourceClaims []observation.ResourceClaim `json:"-"`
}

var ErrObservationCollision = errors.New("observation identity collision")

type ObservationEvidence struct {
	Delivery store.EventDelivery
	// DigestWithoutRule is the envelope's digest as a daemon from before Envelope.Rule
	// computed it. During an upgrade the new hook binary is in place before the daemon
	// reloads; an observation that old daemon committed, whose acknowledgement was lost,
	// replays here with a digest that differs ONLY by the rule. That is the same
	// observation, so it is a duplicate — never a collision that quarantines the file.
	DigestWithoutRule string
	// DigestWithoutLayer is the same upgrade window one field later (schema 38): a
	// daemon from 33–37 computed the digest WITH the rule but WITHOUT the layer, so a
	// replay carrying a staged layer differs only by it. One window digest per added
	// envelope field — the pattern is the point, and the next field extends it the
	// same way.
	DigestWithoutLayer string
	Input              store.EventInput
	Attachment         *store.SessionCheckpoint
	Activity           *store.SessionActivityObservation
}

type ObserveResult struct {
	EventID    int64
	Duplicate  bool
	Checkpoint store.SessionCheckpoint
}

// Governor observes actions and folds state. Detectors are the shipped library +
// overlay; store is the one index. resourceDets is derived from each detector's
// declared Scope ("resource"), so a new resource-scoped detector folds onto the
// target entity automatically — no second hardcoded key list to drift.
type Governor struct {
	ix           *store.Index
	dets         []engine.Detector
	resourceDets map[string]bool // detector id -> folds onto the target entity
	// chain holds each session's event-chain tail in RAM — the anchor the same-user
	// agent cannot reach (ADR 0016). Advanced only after a transaction commits, so a
	// failed insert can never leave the held tail ahead of the store. Guarded by writeMu.
	chain   map[string]*engine.ChainAnchor
	writeMu sync.Mutex // serializes governor/checkpoint writes to SQLite's one writer
	// D13 liveness. observeFailures counts actions the daemon accepted but could NOT
	// persist (e.g. a full disk making Observe fail wholesale) — the one silent-loss
	// path invisible from the log itself, because the row it would have measured is
	// exactly the row that never landed. startedAt anchors "no capture since boot".
	observeFailures atomic.Int64
	startedAt       int64
	// platform is the capability record the stateful tier gates on (item 8/17). A field
	// rather than a direct runtime.GOOS read so a test can pin it — otherwise the
	// stateful-enforcement tests only pass on the demonstrated GOOS and go vacuously
	// green everywhere else.
	platform platform.Support
	// layerStore is the directory whose adopted team-layer records (layers.json, the
	// repository index, staged documents) the stateful tier reads — the one the team
	// link writes. Empty for a governor initGovernor did not build (import, tests):
	// the stateful tier then evaluates the user layer only.
	layerStore string
	// resultPayloadMode is loaded once from the typed local collection artifact and
	// enforced again at the persistence boundary. Hooks are evidence producers, not
	// trusted retention authorities.
	resultPayloadMode      collectionconfig.Mode
	collectionConfigOrigin string
	settled                settledCheckpointCoordinator
	// lifecycleSlot prevents periodic replay and per-session finalizers from scanning
	// the transcript/store corpus concurrently. Capacity one is deliberate: callers
	// wait only within their existing context deadline rather than creating a worker
	// backlog that outlives the session boundary which requested it.
	lifecycleSlot          chan struct{}
	lifecycle              *lifecycleCoordinator
	checkpointSlot         chan struct{}
	understanding          *understandingScanCoordinator
	analyzerAssembly       *codemap.AnalyzerAssembly
	analyzerSelectionError string
	foldRepairError        string // a failed start-up fold repair (repairLegacyDataClassFolds)
}

// openGovernedStore opens the store AND loads the layered detector library the same way
// the live governor does, resolving dataDir consistently. The importer and memory
// indexer both need this pair; doing it in ONE place is also the fix for a real bug —
// building the detector path as filepath.Join("", …) yielded a cwd-relative path, so a
// user's ~/.crossing-guard/policy/detectors.json overlay was silently ignored on import/
// index while applied live, breaking the R7 "same library" guarantee.
func openGovernedStore(dataDir string) (*store.Index, []engine.Detector, error) {
	ix, err := store.Open(store.IndexPath(dataDir, homeDir()))
	if err != nil {
		return nil, nil, err
	}
	dir := dataDir
	if dir == "" {
		dir = filepath.Join(homeDir(), ".crossing-guard")
	}
	loadedDetectors, err := detectorselection.ResolveDetectors(dir, detectorselection.DetectorSurfaceGovernor, os.Getenv("CG_DETECTORS"))
	if err != nil {
		ix.Close()
		return nil, nil, err
	}
	return ix, loadedDetectors.Detectors, nil
}

func NewGovernor(ix *store.Index, dets []engine.Detector) *Governor {
	res := engine.ResourceDetectors(dets)
	assembly, _ := analyzerhost.Compatibility()
	return &Governor{ix: ix, dets: dets, resourceDets: res, chain: map[string]*engine.ChainAnchor{}, startedAt: time.Now().Unix(), analyzerAssembly: assembly,
		platform: platform.Current(), resultPayloadMode: collectionconfig.CodeEffects,
		collectionConfigOrigin: "builtin-default", settled: settledCheckpointCoordinator{items: map[string]*settledCheckpointState{}},
		lifecycleSlot: make(chan struct{}, 1), checkpointSlot: make(chan struct{}, 1)}
}

// GovernorHealth is the capture-liveness answer (D13). The console reads it to say,
// out loud, whether capture is actually happening — the failure D2 hid for four days
// was silent precisely because no surface asked this question.
type GovernorHealth struct {
	Configured                         bool     `json:"configured"`        // false only when the governor never opened; the handler reports that case itself
	Problem                            string   `json:"problem,omitempty"` // why the governor never opened; set only when Configured is false
	StartedAt                          int64    `json:"started_at"`
	LastEventTS                        int64    `json:"last_event_ts"` // 0 = nothing ever captured
	TotalEvents                        int64    `json:"total_events"`
	ObserveFailures                    int64    `json:"observe_failures"` // accepted-but-not-persisted, since boot
	ResultPayloadMode                  string   `json:"result_payload_mode,omitempty"`
	CollectionConfigOrigin             string   `json:"collection_config_origin,omitempty"`
	AnalyzerBundleDigest               string   `json:"analyzer_bundle_digest,omitempty"`
	AnalyzerLanguages                  []string `json:"analyzer_languages,omitempty"`
	AnalyzerSelectionError             string   `json:"analyzer_selection_error,omitempty"`
	FoldRepairError                    string   `json:"fold_repair_error,omitempty"`
	LifecycleQueued                    int64    `json:"lifecycle_queued,omitempty"`
	LifecycleCoalesced                 int64    `json:"lifecycle_coalesced,omitempty"`
	LifecycleOverflow                  int64    `json:"lifecycle_overflow,omitempty"`
	LifecycleActive                    int64    `json:"lifecycle_active,omitempty"`
	LifecycleQueueDepth                int      `json:"lifecycle_queue_depth,omitempty"`
	LifecycleBytesInspected            int64    `json:"lifecycle_bytes_inspected,omitempty"`
	LifecycleRecordsDecoded            int64    `json:"lifecycle_records_decoded,omitempty"`
	LifecycleAliasesRepaired           int64    `json:"lifecycle_aliases_repaired,omitempty"`
	LifecycleEquivalentActionsRepaired int64    `json:"lifecycle_equivalent_actions_repaired,omitempty"`
	UnderstandingOffered               int64    `json:"understanding_offered"`
	UnderstandingCoalesced             int64    `json:"understanding_coalesced"`
	UnderstandingOverflow              int64    `json:"understanding_overflow"`
	UnderstandingStarted               int64    `json:"understanding_started"`
	UnderstandingActive                int64    `json:"understanding_active"`
	UnderstandingCompleted             int64    `json:"understanding_completed"`
	UnderstandingFailed                int64    `json:"understanding_failed"`
	UnderstandingSuperseded            int64    `json:"understanding_superseded"`
	UnderstandingRecovered             int64    `json:"understanding_recovered"`
	UnderstandingMissingRoot           int64    `json:"understanding_missing_root"`
	UnderstandingQueueDepth            int      `json:"understanding_queue_depth"`
	UnderstandingFactsPruned           int64    `json:"understanding_facts_pruned"`
	UnderstandingPruneSkippedBusy      int64    `json:"understanding_prune_skipped_busy"`
	UnderstandingRemeasured            int64    `json:"understanding_remeasured"`
	UnderstandingPruneCandidates       int64    `json:"understanding_prune_candidates"`
	UnderstandingPruneUnfinished       int64    `json:"understanding_prune_unfinished"`
	UnderstandingRetentionLastPass     int64    `json:"understanding_retention_last_pass,omitempty"`
	// UnderstandingRetentionEnabled is whether a pass would run now. The candidate
	// count means "backlog" only when this is true and a last pass is recorded;
	// before the first pass, or with retention off, it is 0 without having measured.
	UnderstandingRetentionEnabled bool `json:"understanding_retention_enabled"`
}

// Health reports capture liveness. It reads the log's newest row (the detector for
// every drop-shaped loss) and the persist-failure counter (the one loss the log
// cannot show).
func (g *Governor) Health() (GovernorHealth, error) {
	h := GovernorHealth{Configured: true, StartedAt: g.startedAt,
		ObserveFailures: g.observeFailures.Load(), ResultPayloadMode: string(g.resultPayloadMode),
		CollectionConfigOrigin: g.collectionConfigOrigin, AnalyzerSelectionError: g.analyzerSelectionError,
		FoldRepairError: g.foldRepairError}
	if g.analyzerAssembly != nil {
		h.AnalyzerBundleDigest = g.analyzerAssembly.Digest()
		h.AnalyzerLanguages = g.analyzerAssembly.Languages()
	}
	if g.lifecycle != nil {
		h.LifecycleQueued = g.lifecycle.stats.queued.Load()
		h.LifecycleCoalesced = g.lifecycle.stats.coalesced.Load()
		h.LifecycleOverflow = g.lifecycle.stats.overflow.Load()
		h.LifecycleActive = g.lifecycle.stats.active.Load()
		h.LifecycleQueueDepth = len(g.lifecycle.queue)
		h.LifecycleBytesInspected = g.lifecycle.stats.bytesInspected.Load()
		h.LifecycleRecordsDecoded = g.lifecycle.stats.recordsDecoded.Load()
		h.LifecycleAliasesRepaired = g.lifecycle.stats.aliasesRepaired.Load()
		h.LifecycleEquivalentActionsRepaired = g.lifecycle.stats.equivalentActionsRepaired.Load()
	}
	if g.understanding != nil {
		h.UnderstandingOffered = g.understanding.stats.offered.Load()
		h.UnderstandingCoalesced = g.understanding.stats.coalesced.Load()
		h.UnderstandingOverflow = g.understanding.stats.overflow.Load()
		h.UnderstandingStarted = g.understanding.stats.started.Load()
		h.UnderstandingActive = g.understanding.stats.active.Load()
		h.UnderstandingCompleted = g.understanding.stats.completed.Load()
		h.UnderstandingFailed = g.understanding.stats.failed.Load()
		h.UnderstandingSuperseded = g.understanding.stats.superseded.Load()
		h.UnderstandingRecovered = g.understanding.stats.recovered.Load()
		h.UnderstandingMissingRoot = g.understanding.stats.missingRoot.Load()
		h.UnderstandingFactsPruned = g.understanding.stats.factsPruned.Load()
		h.UnderstandingPruneSkippedBusy = g.understanding.stats.pruneSkippedBusy.Load()
		h.UnderstandingRemeasured = g.understanding.stats.remeasured.Load()
		h.UnderstandingPruneCandidates = g.understanding.stats.pruneCandidates.Load()
		h.UnderstandingPruneUnfinished = g.understanding.stats.pruneUnfinished.Load()
		h.UnderstandingRetentionLastPass = g.understanding.stats.retentionLastPass.Load()
		_, h.UnderstandingRetentionEnabled = g.understanding.retentionSettings()
		g.understanding.mu.Lock()
		h.UnderstandingQueueDepth = len(g.understanding.items)
		g.understanding.mu.Unlock()
	}
	stat, err := g.ix.EventLogStat()
	if err != nil {
		return h, err
	}
	h.LastEventTS, h.TotalEvents = stat.LastEventTS, stat.Total
	return h, nil
}

type frozenTag struct {
	Key        string `json:"key"`
	Value      string `json:"value"`
	Detector   string `json:"detector"`
	Provenance string `json:"provenance"`
	Evidence   string `json:"evidence,omitempty"`
}

// Observe classifies one action, appends it to the log with FROZEN tags, and folds the
// resulting state onto the target entity (resource facts) and the session (all facts) —
// all in one transaction, so PRIMARY TRUTH and its fold can never diverge on a failure.
// Observe-only: no decision is made here.
func (g *Governor) Observe(o Observation) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	_, err := g.observe(o, nil)
	return err
}

func (g *Governor) ObserveV1(o Observation, evidence ObservationEvidence) (ObserveResult, error) {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	return g.observe(o, &evidence)
}

func (g *Governor) observe(o Observation, evidence *ObservationEvidence) (ObserveResult, error) {
	// ONE normalizer (R7) — see normalize.go. The live path and the Phase 4 importer
	// must agree here, or live state and replayed state diverge permanently.
	n := Normalize(o)
	tags := engine.Classify(n.Event, g.dets)
	origin := o.Origin
	if origin == "" {
		origin = "live"
	}

	frozen := make([]frozenTag, len(tags))
	for i, t := range tags {
		frozen[i] = frozenTag{t.Key, t.Value, t.Detector, string(t.Provenance), t.Evidence}
	}
	// The frozen tags ARE the determinism contract of the append-only log; a marshal
	// failure must fail the observe, never write empty tags into primary truth.
	tagsJSON, err := json.Marshal(frozen)
	if err != nil {
		return ObserveResult{}, err
	}

	tx, err := g.ix.BeginGov()
	if err != nil {
		return ObserveResult{}, err
	}
	defer tx.Rollback() // no-op after a successful Commit
	if evidence != nil {
		eventID, digest, found, err := tx.ObservationIdentity(evidence.Delivery.ObservationID)
		if err != nil {
			return ObserveResult{}, err
		}
		if found {
			if digest != evidence.Delivery.EnvelopeDigest &&
				(evidence.DigestWithoutRule == "" || digest != evidence.DigestWithoutRule) &&
				(evidence.DigestWithoutLayer == "" || digest != evidence.DigestWithoutLayer) {
				return ObserveResult{}, ErrObservationCollision
			}
			if err := tx.TouchEventDelivery(evidence.Delivery.ObservationID, evidence.Delivery.DeliveryAttempts); err != nil {
				return ObserveResult{}, fmt.Errorf("touch duplicate observation delivery: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return ObserveResult{}, fmt.Errorf("commit duplicate observation delivery: %w", err)
			}
			checkpoint, _, err := g.ix.SessionCheckpointForObservation(evidence.Delivery.ObservationID)
			if err != nil {
				return ObserveResult{}, fmt.Errorf("read duplicate observation checkpoint: %w", err)
			}
			return ObserveResult{EventID: eventID, Duplicate: true, Checkpoint: checkpoint}, err
		}
		if evidence.Activity != nil {
			if err := tx.EnsureSessionRoot(o.Runtime, o.SessionID,
				evidence.Activity.SourceRef, o.Cwd); err != nil {
				return ObserveResult{}, err
			}
			hasActivity, err := tx.SessionHasOpenActivity(o.Runtime, o.SessionID)
			if err != nil {
				return ObserveResult{}, err
			}
			if !hasActivity {
				if _, err := tx.AppendSessionActivity(*evidence.Activity); err != nil {
					return ObserveResult{}, err
				}
			}
		}
	}

	type resource struct {
		id, kind, identity string
		evidence           store.EventResourceEvidence
	}
	resources := []resource{}
	seenResources := map[string]bool{}
	addResource := func(id, kind, identity string, ev store.EventResourceEvidence, retainRepeat bool) {
		if id != "" && (retainRepeat || !seenResources[id]) {
			seenResources[id] = true
			resources = append(resources, resource{id, kind, identity, ev})
		}
	}
	if len(o.ResourceClaims) > 0 {
		for _, claim := range o.ResourceClaims {
			kind, identity := claim.Kind, claim.Identity
			if identity == "" {
				kind = "unresolved-" + claim.Kind
				identity = claim.RawIdentity
			}
			id := engine.EntityID(kind, identity+"\x00"+claim.SourceField)
			if claim.Identity != "" {
				id = engine.EntityID(kind, identity)
			}
			addResource(id, kind, identity, store.EventResourceEvidence{RawIdentity: claim.RawIdentity,
				Operation: claim.Operation, EvidenceClass: claim.EvidenceClass, Source: claim.SourceField,
				SourceField: claim.SourceField, Completeness: claim.Completeness}, true)
		}
	} else {
		for _, path := range o.FilePaths {
			clean := filepath.Clean(path)
			if clean == "." || !filepath.IsAbs(clean) {
				continue
			}
			addResource(engine.EntityID("file", clean), "file", clean,
				store.EventResourceEvidence{Source: "tool_input.command", Operation: "unknown", EvidenceClass: "unknown", Completeness: "unknown"}, false)
		}
		// A structured absolute file set supersedes Normalize's legacy single file target.
		// This matters when file_path was relative: retaining both would manufacture two
		// identities for one resource. Non-file targets and old hooks keep the legacy path.
		if n.TargetKind != "file" || len(resources) == 0 {
			addResource(n.TargetID, n.TargetKind, n.TargetIdentity,
				store.EventResourceEvidence{Source: "tool_input", Operation: "unknown", EvidenceClass: "unknown", Completeness: "unknown"}, false)
		}
	}
	primaryTarget := n.TargetID
	if len(resources) > 0 {
		primaryTarget = resources[0].id
	}
	// The layered loader's notes ride the reason TEXT (schema 38 is frozen;
	// the reasons are part of "why this decision" — the same honesty the
	// reason field has always carried), deduplicated if already present.
	if o.LayerReasons != "" && !strings.Contains(o.Reason, o.LayerReasons) {
		o.Reason = strings.TrimSpace(o.Reason + "; " + o.LayerReasons)
	}
	anchor, err := g.chainAnchorLocked(o.SessionID)
	if err != nil {
		return ObserveResult{}, err
	}
	work := *anchor // advance a copy; the held anchor moves only after Commit
	eventID, err := tx.AppendEvent(store.EventRecord{
		TS: o.TS, SessionID: o.SessionID, Runtime: o.Runtime, Verb: n.Verb, Tool: o.Tool,
		TargetEntityID: primaryTarget, Tags: string(tagsJSON), Origin: origin,
		Decision: o.Decision, Reason: o.Reason, RuleID: o.Rule, Layer: storableLayer(o.Layer),
	}, &work)
	if err != nil {
		return ObserveResult{}, err
	}
	for ordinal, r := range resources {
		if err := tx.UpsertEntity(r.id, r.kind, r.identity, o.TS); err != nil {
			return ObserveResult{}, err
		}
		r.evidence.EventID, r.evidence.Ordinal, r.evidence.EntityID = eventID, ordinal, r.id
		if err := tx.AppendEventResourceEvidence(r.evidence); err != nil {
			return ObserveResult{}, err
		}
	}
	result := ObserveResult{EventID: eventID}
	if evidence != nil {
		evidence.Delivery.EventID = eventID
		if err := tx.AppendEventDelivery(evidence.Delivery); err != nil {
			return ObserveResult{}, err
		}
		evidence.Input.EventID = eventID
		if err := tx.AppendEventInput(evidence.Input); err != nil {
			return ObserveResult{}, err
		}
		if evidence.Attachment != nil {
			evidence.Attachment.TriggerEventID = eventID
			checkpoint, _, err := tx.EnsureSessionCheckpoint(*evidence.Attachment)
			if err != nil {
				return ObserveResult{}, err
			}
			result.Checkpoint = checkpoint
		}
	}
	for _, t := range tags {
		sr := engine.FactFromTag(t)
		if err := tx.UpsertSessionState(o.SessionID, sr, o.TS); err != nil {
			return ObserveResult{}, err
		}
		if g.resourceDets[t.Detector] {
			for _, r := range resources {
				if err := tx.UpsertEntityState(r.id, sr, o.TS); err != nil {
					return ObserveResult{}, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return ObserveResult{}, err
	}
	*anchor = work // committed: the held tail advances
	return result, nil
}

// chainAnchorLocked returns the held anchor for a session, creating it on first sight.
// If the store already has chained rows — the daemon restarted — the anchor is seeded
// from the store's tail and says so; verification then reports rows at or below that
// seq as a separate, weaker span rather than pretending they were held. Caller holds
// writeMu.
func (g *Governor) chainAnchorLocked(sessionID string) (*engine.ChainAnchor, error) {
	if a, ok := g.chain[sessionID]; ok {
		return a, nil
	}
	seq, tail, ok, err := g.ix.EventChainTail(sessionID)
	if err != nil {
		return nil, err
	}
	var a *engine.ChainAnchor
	if ok {
		a = engine.NewDiskSeededAnchor(seq, tail)
	} else {
		a = engine.NewGenesisAnchor(sessionID)
	}
	g.chain[sessionID] = a
	return a, nil
}

// ChainVerify recomputes a session's chain against the store and, when this daemon has
// held that session's tail since boot, checks the tail too. A session not seen since
// boot is verified for internal consistency only, and the report says so.
func (g *Governor) ChainVerify(sessionID string) (engine.ChainReport, error) {
	g.writeMu.Lock()
	var held *engine.ChainAnchor
	if a, ok := g.chain[sessionID]; ok {
		copy := *a
		held = &copy
	}
	g.writeMu.Unlock()
	rows, err := g.ix.EventChainRows(sessionID)
	if err != nil {
		return engine.ChainReport{}, err
	}
	if held != nil {
		// Rows a concurrent Observe committed after the anchor was copied are beyond this
		// verification's snapshot, not a fork: verify up to the held tail (postwork C4 —
		// an active session otherwise reported "fork" to the team server).
		rows = rowsThrough(rows, held.Seq)
	}
	return engine.VerifyEventChain(sessionID, rows, held), nil
}

// rowsThrough keeps the rows up to and including chain seq (legacy rows kept).
func rowsThrough(rows []engine.ChainRow, seq int64) []engine.ChainRow {
	out := rows[:0:0]
	for _, r := range rows {
		if r.Hash == "" || r.Body.Seq <= seq {
			out = append(out, r)
		}
	}
	return out
}

// SessionState returns the folded state for a session (console + verification).
func (g *Governor) SessionState(sessionID string) ([]store.StateRow, error) {
	return g.ix.SessionState(sessionID)
}

// entityEventCap bounds one entity's answer. A hot file (this repo's own docs) can
// accumulate thousands of events; the report says when it truncated rather than
// quietly showing a prefix as if it were everything (INV-21).
const entityEventCap = 2000

// storeDefaultEventLimit mirrors the store's own clamp for limit<=0. Duplicated as a
// named constant rather than a bare 500 so the two are greppable together.
const storeDefaultEventLimit = 500

// governedSessionCap bounds the Governance rail. Sessions are far fewer than events
// (hundreds, not thousands), so this is generous on purpose.
const governedSessionCap = 500

// EntityReport answers "which session touched this resource?" — the query the
// governance model exists for. Sessions are joined to their human titles here: the
// log keys on raw ids, and an answer in UUIDs is not an answer.
func (g *Governor) EntityReport(entityID string, cap int) (*EntityReport, error) {
	rep := &EntityReport{ID: entityID, State: []store.StateRow{}, Touches: []EntityTouch{}}

	ent, err := g.ix.LookupEntity(entityID)
	if err != nil {
		return nil, err
	}
	rep.Entity, rep.Found = ent, ent != nil

	if rep.State, err = g.ix.EntityState(entityID); err != nil {
		return nil, err
	}
	if rep.State == nil {
		rep.State = []store.StateRow{}
	}
	// Clamp HERE, matching the store's own default, so Truncated is computed against
	// the limit actually applied. Passing cap<=0 previously returned up to 500 rows
	// and reported Truncated=true unconditionally — the two halves of the contract
	// disagreed, and EntityReport is exported.
	if cap <= 0 {
		cap = storeDefaultEventLimit
	}
	evs, err := g.ix.EventsForEntity(entityID, cap)
	if err != nil {
		return nil, err
	}
	rep.Events, rep.Truncated = len(evs), len(evs) >= cap

	titles := sessionTitles()
	order := []string{}
	by := map[string]*EntityTouch{}
	for _, e := range evs {
		t := by[e.SessionID]
		if t == nil {
			meta := titles[e.SessionID]
			t = &EntityTouch{SessionID: e.SessionID, Title: meta.Title, Runtime: meta.Runtime,
				Verbs: []string{}, Decisions: []string{}, FirstSeen: e.TS, LastSeen: e.TS}
			by[e.SessionID], order = t, append(order, e.SessionID)
		}
		t.Events++
		if e.TS < t.FirstSeen {
			t.FirstSeen = e.TS
		}
		if e.TS > t.LastSeen {
			t.LastSeen = e.TS
		}
		t.Verbs = addOnce(t.Verbs, e.Verb)
		t.Decisions = addOnce(t.Decisions, e.Decision)
	}
	for _, id := range order {
		rep.Touches = append(rep.Touches, *by[id])
	}

	// An empty answer must say WHY: "never observed" and "observed but no events
	// survive the retention window" are different facts, and a blank list is a lie
	// that reads as "nothing happened here".
	switch {
	case !rep.Found && rep.Events == 0:
		rep.Note = "no record of this resource — it has never been observed by a governed session"
	case rep.Found && rep.Events == 0:
		rep.Note = "resource known, but no events target it (state may come from a resource-scoped detector)"
	case !anyDecisionRecorded(evs):
		// An empty decision list here means UNKNOWN, not "nothing was blocked".
		// Every event captured before decisions were recorded (D19) is in this
		// state permanently — the log cannot be backfilled with a judgement nobody
		// wrote down. Callers must not render blank as "allowed".
		rep.Note = "no decisions recorded for these events — they predate decision capture; " +
			"absent is UNKNOWN, not allowed"
	}
	return rep, nil
}

// addOnce appends a non-empty value if absent, preserving first-seen order.
func addOnce(xs []string, v string) []string {
	if v == "" {
		return xs
	}
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// sessionTitles maps a raw session id to its human title. Live governance rows carry
// the runtime's bare session id, which is the harvest filename stem — so the two
// sides join on it. Best-effort: an unknown id simply keeps its UUID. A thread id
// joins only the sessions it names, never a subagent rollout carrying it.
func sessionTitles() map[string]harvest.SessionSummary {
	out := map[string]harvest.SessionSummary{}
	for _, s := range ScanSessions() {
		for _, id := range sessionIdentities(s) {
			out[id] = s
		}
	}
	return out
}

// anyDecisionRecorded reports whether ANY event carries a decision, so the report can
// distinguish "nothing was blocked" from "we were not recording yet".
func anyDecisionRecorded(evs []store.EventRecord) bool {
	for _, e := range evs {
		if e.Decision != "" {
			return true
		}
	}
	return false
}

// SessionEvents exposes a session's events for the console footprint.
func (g *Governor) SessionEvents(sessionID string, limit int) ([]store.EventRecord, error) {
	return g.ix.EventsForSession(sessionID, limit)
}

// StoredSessionIdentity reports whether any canonical evidence owner contains the
// supplied native session identity.
func (g *Governor) StoredSessionIdentity(sessionID string) (bool, error) {
	return g.ix.SessionEvidenceExists(sessionID)
}

// StoredSessionRoots returns the distinct working roots captured for one session.
func (g *Governor) StoredSessionRoots(sessionID string) ([]string, error) {
	sources, err := g.ix.LiveSessionRuntimesForSession(sessionID, 100)
	if err != nil {
		return nil, err
	}
	roots := []string{}
	seen := map[string]bool{}
	for _, source := range sources {
		if source.WorkingDirectory != "" && !seen[source.WorkingDirectory] {
			seen[source.WorkingDirectory] = true
			roots = append(roots, source.WorkingDirectory)
		}
	}
	return roots, nil
}

// SessionEvidenceSummary is the bounded aggregate used by the persistent panel header.
type SessionEvidenceSummary struct {
	ID              string                       `json:"id"`
	Found           bool                         `json:"found"`
	Note            string                       `json:"note,omitempty"`
	Title           string                       `json:"title,omitempty"`
	Runtime         string                       `json:"runtime,omitempty"`
	TitleSource     string                       `json:"title_source,omitempty"`
	EventCount      int                          `json:"event_count"`
	LastActionAt    int64                        `json:"last_action_at,omitempty"`
	FileTargetCount int                          `json:"file_target_count"`
	WithoutFile     int                          `json:"without_file_target"`
	Decisions       store.SessionDecisionStat    `json:"decisions"`
	Facts           store.SessionFactCounts      `json:"facts"`
	Results         store.ResultStats            `json:"result_stats"`
	CodeChanges     []changeenv.CodeChangeStatus `json:"code_changes"`
}

// SessionEvidenceSummary returns exact aggregate facts without loading row populations.
func (g *Governor) SessionEvidenceSummary(sessionID, runtime string) (SessionEvidenceSummary, error) {
	out := SessionEvidenceSummary{ID: sessionID, CodeChanges: []changeenv.CodeChangeStatus{}}
	if sessionID == "" {
		out.Note = "no session id was given"
		return out, nil
	}
	stat, err := g.ix.EventStatForSession(sessionID)
	if err != nil {
		return out, err
	}
	out.EventCount, out.LastActionAt = stat.Total, stat.LastEventTS
	out.Decisions, err = g.ix.DecisionStatForSession(sessionID)
	if err != nil {
		return out, err
	}
	out.Facts, err = g.ix.FactCountsForSession(sessionID)
	if err != nil {
		return out, err
	}
	out.Results, err = g.ix.ResultStatsForSession(sessionID)
	if err != nil {
		return out, err
	}
	out.CodeChanges, err = changeenv.CodeChangeStatuses(g.ix, sessionID, currentAnalyzerBundle())
	if err != nil {
		return out, err
	}
	for index := range out.CodeChanges {
		g.markCodeChangeStatusPending(&out.CodeChanges[index])
	}
	touches, err := g.ix.SessionFileTouches(sessionID, 1)
	if err != nil {
		return out, err
	}
	out.FileTargetCount, out.WithoutFile = touches.DistinctFiles, touches.NonFileEvents
	if meta, found, metaErr := g.ix.SessionByID(runtime, sessionID); metaErr != nil {
		return out, metaErr
	} else if found {
		out.Title, out.Runtime, out.TitleSource = meta.Title, meta.Vendor, "index"
	} else {
		out.Runtime = runtime
	}
	out.Found = out.EventCount > 0 || out.Facts.Results > 0 || out.Facts.Checkpoints > 0
	if !out.Found {
		out.Note = "no captured session evidence"
	}
	return out, nil
}

func (g *Governor) markCodeChangeStatusPending(status *changeenv.CodeChangeStatus) {
	if status == nil || status.State != "unavailable" || status.Current == nil || g.understanding == nil {
		return
	}
	if g.understanding.pending(status.RepositoryID, status.CheckoutID, status.Current.SnapshotDigest) {
		status.State = "pending"
		status.Reason = "exact analysis is queued or running for the selected checkpoint"
	}
}

// SessionCodeChanges returns a bounded factual comparison and never admits analyzer
// work. Pending is projected only from already-admitted coordinator state.
func (g *Governor) SessionCodeChanges(sessionID string, options changeenv.CodeChangeOptions) (changeenv.CodeChanges, error) {
	if options.AnalyzerBundleDigest == "" {
		options.AnalyzerBundleDigest = currentAnalyzerBundle()
	}
	out, err := changeenv.BuildCodeChanges(g.ix, sessionID, options)
	if err != nil {
		return out, err
	}
	if out.State == "unavailable" && out.Current != nil && g.understanding != nil &&
		g.understanding.pending(out.RepositoryID, out.CheckoutID, out.Current.SnapshotDigest) {
		out.State = "pending"
		out.Reason = "exact analysis is queued or running for the selected checkpoint"
	}
	return out, nil
}

// SessionEdits pages the session's recorded edits (workspace-panes plan §4.2):
// what the runtime reported changing, with each retained body described by kind
// and size. Bodies are fetched one at a time through SessionBody.
func (g *Governor) SessionEdits(sessionID string, limit, offset int) (changeenv.SessionEdits, error) {
	return changeenv.BuildSessionEdits(g.ix, sessionID, limit, offset)
}

// SessionCodeChangeFile returns one bounded centered comparison and never admits work.
func (g *Governor) SessionCodeChangeFile(sessionID, path string, options changeenv.CodeChangeOptions,
	structuralOffset, structuralLimit int) (changeenv.CodeChangeFileDetail, error) {
	if options.AnalyzerBundleDigest == "" {
		options.AnalyzerBundleDigest = currentAnalyzerBundle()
	}
	out, err := changeenv.BuildCodeChangeFile(g.ix, sessionID, path, options, structuralOffset, structuralLimit)
	if err != nil {
		return out, err
	}
	if out.State == "unavailable" && out.Current != nil && g.understanding != nil &&
		g.understanding.pending(out.RepositoryID, out.CheckoutID, out.Current.SnapshotDigest) {
		out.State = "pending"
		out.Reason = "exact analysis is queued or running for the selected checkpoint"
	}
	return out, nil
}

// SessionTrace is one stable, bounded page of session actions and their resources.
type SessionTrace struct {
	ID        string                `json:"id"`
	Found     bool                  `json:"found"`
	Events    []store.EventRecord   `json:"events"`
	Resources []store.EventResource `json:"event_resources"`
	Total     int                   `json:"event_count"`
	Snapshot  int64                 `json:"snapshot_id"`
	NextTS    int64                 `json:"-"`
	NextID    int64                 `json:"-"`
	HasMore   bool                  `json:"-"`
}

// SessionTrace reads one snapshot-bound trace page.
func (g *Governor) SessionTrace(sessionID string, query store.SessionTraceQuery) (SessionTrace, error) {
	page, err := g.ix.TracePageForSession(sessionID, query)
	if err != nil {
		return SessionTrace{}, err
	}
	return SessionTrace{ID: sessionID, Found: page.Total > 0, Events: page.Events,
		Resources: page.Resources, Total: page.Total, Snapshot: page.SnapshotID,
		NextTS: page.LastTS, NextID: page.LastID, HasMore: page.HasMore}, nil
}

// SessionActionResult pairs one exact result observation with its current reconciliation.
type SessionActionResult struct {
	Observation      store.ResultObservation     `json:"observation"`
	Reconciliation   *store.ResultReconciliation `json:"reconciliation,omitempty"`
	EffectCount      int                         `json:"effect_count"`
	EffectsTruncated bool                        `json:"effects_truncated"`
}

// SessionActionDetail contains one session-owned call and its captured evidence.
type SessionActionDetail struct {
	ID               string                `json:"id"`
	Event            store.EventRecord     `json:"event"`
	Resources        []store.EventResource `json:"resources"`
	Delivery         *store.EventDelivery  `json:"delivery,omitempty"`
	Input            *store.EventInput     `json:"input,omitempty"`
	Results          []SessionActionResult `json:"results"`
	ResultCount      int                   `json:"result_count"`
	ResultsTruncated bool                  `json:"results_truncated"`
}

// SessionAction returns exact metadata for an action owned by the supplied session.
func (g *Governor) SessionAction(sessionID string, eventID int64) (SessionActionDetail, error) {
	event, err := g.ix.EventForSession(sessionID, eventID)
	if err != nil {
		return SessionActionDetail{}, err
	}
	out := SessionActionDetail{ID: sessionID, Event: event, Results: []SessionActionResult{}}
	out.Resources, err = g.ix.EventResourcesForEvent(eventID)
	if err != nil {
		return out, err
	}
	observationID, evidenceErr := g.ix.ObservationIDForEvent(eventID)
	if evidenceErr == nil {
		delivery, input, found, readErr := g.ix.ObservationEvidence(observationID)
		if readErr != nil {
			return out, readErr
		}
		if found {
			input.Payload = nil
			out.Delivery, out.Input = &delivery, &input
		}
	} else if !errors.Is(evidenceErr, sql.ErrNoRows) {
		return out, evidenceErr
	}
	resultIDs, resultCount, err := g.ix.ExactResultObservationIDsForEventPage(eventID, sessionActionResultLimit)
	if err != nil {
		return out, err
	}
	out.ResultCount, out.ResultsTruncated = resultCount, resultCount > len(resultIDs)
	for _, observationID := range resultIDs {
		result, readErr := g.ix.ResultObservationByObservationID(observationID)
		if readErr != nil {
			return out, readErr
		}
		result.Payload = nil
		for index := range result.Effects {
			result.Effects[index].ReplacementBeforePayload = nil
			result.Effects[index].ReplacementAfterPayload = nil
			result.Effects[index].ContentPayload = nil
			result.Effects[index].DiffPayload = nil
		}
		effectCount := len(result.Effects)
		if len(result.Effects) > sessionActionEffectLimit {
			result.Effects = result.Effects[:sessionActionEffectLimit]
		}
		item := SessionActionResult{Observation: result, EffectCount: effectCount,
			EffectsTruncated: effectCount > len(result.Effects)}
		if reconciliation, found, recErr := g.ix.CurrentResultReconciliation(result.ID); recErr != nil {
			return out, recErr
		} else if found {
			item.Reconciliation = &reconciliation
		}
		out.Results = append(out.Results, item)
	}
	return out, nil
}

// SessionReach aggregates observed runtime and decision populations for one session.
type SessionReach struct {
	ID       string                             `json:"id"`
	Runtimes []store.SessionRuntimeDecisionStat `json:"runtimes"`
}

// SessionCapture is the bounded compatibility read model for the Governance center.
type SessionCapture struct {
	ID                string              `json:"id"`
	Found             bool                `json:"found"`
	Note              string              `json:"note,omitempty"`
	Title             string              `json:"title,omitempty"`
	Runtime           string              `json:"runtime,omitempty"`
	TitleSource       string              `json:"title_source,omitempty"`
	State             []store.StateRow    `json:"state"`
	Events            []store.EventRecord `json:"events"`
	EventCount        int                 `json:"event_count"`
	Truncated         bool                `json:"truncated"`
	DecisionsRecorded bool                `json:"decisions_recorded"`
}

// SessionCapture returns folded state and a bounded action sample.
func (g *Governor) SessionCapture(sessionID, runtime string, limit int) (SessionCapture, error) {
	if limit <= 0 || limit > sessionCaptureLimit {
		limit = sessionCaptureLimit
	}
	out := SessionCapture{ID: sessionID, State: []store.StateRow{}, Events: []store.EventRecord{}}
	var err error
	out.State, err = g.ix.SessionState(sessionID)
	if err != nil {
		return out, err
	}
	out.Events, err = g.ix.EventsForSession(sessionID, limit)
	if err != nil {
		return out, err
	}
	stat, err := g.ix.EventStatForSession(sessionID)
	if err != nil {
		return out, err
	}
	out.EventCount, out.Truncated = stat.Total, stat.Total > len(out.Events)
	out.DecisionsRecorded = anyDecisionRecorded(out.Events)
	out.Found = out.EventCount > 0 || len(out.State) > 0
	if meta, found, metaErr := g.ix.SessionByID(runtime, sessionID); metaErr != nil {
		return out, metaErr
	} else if found {
		out.Title, out.Runtime, out.TitleSource = meta.Title, meta.Vendor, "index"
	} else {
		out.Runtime = runtime
	}
	if !out.Found {
		out.Note = "no governed tool call was captured for this session"
	}
	return out, nil
}

// SessionReach returns aggregate runtime and decision facts without action rows.
func (g *Governor) SessionReach(sessionID string) (SessionReach, error) {
	stats, err := g.ix.RuntimeDecisionStatsForSession(sessionID)
	return SessionReach{ID: sessionID, Runtimes: stats}, err
}

// SessionChange delegates the session Change/Impact/Verify projection to changeenv.
func (g *Governor) SessionChange(sessionID string, options changeenv.ViewOptions) (changeenv.SessionView, error) {
	if options.AnalyzerBundleDigest == "" {
		options.AnalyzerBundleDigest = currentAnalyzerBundle()
	}
	return changeenv.Build(g.ix, sessionID, options)
}

// SessionFileDetail joins one validated Change row to its stored actions and effects.
type SessionFileDetail struct {
	ID                  string                         `json:"id"`
	File                changeenv.FileEvidenceIdentity `json:"file"`
	Events              []store.EventRecord            `json:"events"`
	EventCount          int                            `json:"event_count"`
	Effects             []store.LinkedResultEffect     `json:"effects"`
	EffectCount         int                            `json:"effect_count"`
	Reconciliation      []store.PathReconciliation     `json:"path_reconciliation"`
	ReconciliationCount int                            `json:"path_reconciliation_count"`
}

// SessionFile returns evidence for a server-issued file key owned by the session.
func (g *Governor) SessionFile(sessionID, key string) (SessionFileDetail, error) {
	identity, err := changeenv.DecodeFileEvidenceKey(key)
	if err != nil {
		return SessionFileDetail{}, err
	}
	view, err := g.SessionChange(sessionID, changeenv.ViewOptions{RepositoryLimit: sessionFileValidationLimit,
		FileLimit: sessionFileValidationLimit, FileFilter: "all", FileQuery: identity.Path})
	if err != nil {
		return SessionFileDetail{}, err
	}
	owned := false
	for _, repository := range view.Repositories {
		if repository.RepositoryID != identity.RepositoryID || repository.CheckoutID != identity.CheckoutID {
			continue
		}
		for _, row := range repository.Files {
			if row.EvidenceKey == key {
				owned = true
				break
			}
		}
	}
	if !owned {
		return SessionFileDetail{}, sql.ErrNoRows
	}
	out := SessionFileDetail{ID: sessionID, File: identity}
	identities := []string{identity.AbsolutePath, identity.Path}
	out.Events, out.EventCount, err = g.ix.FileEventsForSession(sessionID, identities, sessionFileDetailLimit)
	if err != nil {
		return out, err
	}
	out.Effects, out.EffectCount, err = g.ix.LinkedResultEffectsForFile(sessionID, identities, sessionFileDetailLimit)
	if err != nil {
		return out, err
	}
	out.Reconciliation, out.ReconciliationCount, err = g.ix.PathReconciliationsForFile(sessionID,
		[]string{identity.Path, identity.AbsolutePath}, sessionFileDetailLimit)
	return out, err
}

// SessionBody is one explicitly requested retained textual evidence body.
type SessionBody struct {
	Kind         string `json:"kind"`
	MediaType    string `json:"media_type"`
	Digest       string `json:"digest"`
	Completeness string `json:"completeness"`
	Bytes        int    `json:"bytes"`
	Text         string `json:"text"`
}

// SessionBody returns a complete UTF-8 body only after session ownership is proven.
func (g *Governor) SessionBody(sessionID, kind string, eventID, resultID int64, ordinal int) (SessionBody, error) {
	var retained store.RetainedTextBody
	var err error
	switch kind {
	case "event_input":
		retained, err = g.ix.EventInputBodyForSession(sessionID, eventID)
	case "result":
		retained, err = g.ix.ResultBodyForSession(sessionID, resultID)
	case "effect_content", "effect_diff", "effect_before", "effect_after":
		retained, err = g.ix.ResultEffectBodyForSession(sessionID, resultID, ordinal, kind)
	default:
		return SessionBody{}, sql.ErrNoRows
	}
	if err != nil {
		return SessionBody{}, err
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(retained.MediaType, ";")[0]))
	textual := strings.HasPrefix(media, "text/") || media == "application/json" || media == "application/xml" || media == "application/javascript"
	if retained.Completeness != "complete" || len(retained.Bytes) == 0 || len(retained.Bytes) > observation.MaxRetainedInput || !textual || !utf8.Valid(retained.Bytes) {
		return SessionBody{}, sql.ErrNoRows
	}
	return SessionBody{Kind: kind, MediaType: retained.MediaType, Digest: retained.Digest,
		Completeness: retained.Completeness, Bytes: len(retained.Bytes), Text: string(retained.Bytes)}, nil
}

// SessionReport is the session-side twin of EntityReport, and is deliberately the
// SAME shape. GET /api/govern/session used to return a bare []StateRow, which could
// not distinguish "this session was never observed" from "observed, and we folded
// no state onto it" — both serialize as []. A console cannot render an honest empty
// state (INV-21) from a payload that does not carry why it is empty.
type SessionReport struct {
	ID      string `json:"id"`
	Found   bool   `json:"found"`
	Note    string `json:"note,omitempty"` // why the answer is empty, when it is
	Title   string `json:"title,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	// TitleSource carries Step 1's honesty label through to the governance rail, so
	// a truncated first prompt is not displayed as an authored title here either.
	TitleSource        string                `json:"title_source,omitempty"`
	State              []store.StateRow      `json:"state"`
	Events             []store.EventRecord   `json:"events"`
	Resources          []store.EventResource `json:"event_resources"`
	ResourceCount      int                   `json:"event_resource_count"`
	ResourcesTruncated bool                  `json:"event_resources_truncated"`
	EventCount         int                   `json:"event_count"`
	Truncated          bool                  `json:"truncated"` // hit the row cap: the answer is PARTIAL
	// DecisionsRecorded distinguishes "nothing was blocked" from "we were not
	// recording decisions yet". Every event before 00032f0 has an empty decision,
	// permanently; absent is UNKNOWN, never "allowed".
	DecisionsRecorded      bool                       `json:"decisions_recorded"`
	Results                []store.ResultObservation  `json:"results"`
	ResultStats            store.ResultStats          `json:"result_stats"`
	Checkpoints            []store.SessionCheckpoint  `json:"checkpoints"`
	Reconciliation         []store.PathReconciliation `json:"path_reconciliation"`
	CollectionIssues       []store.CollectionIssue    `json:"collection_issues"`
	ResultPayloadMode      string                     `json:"result_payload_mode"`
	CollectionConfigOrigin string                     `json:"collection_config_origin"`
	CaptureFound           bool                       `json:"capture_found"`
	Change                 changeenv.SessionView      `json:"change"`
}

// SessionReport folds one session's events and state into the whole answer.
func (g *Governor) SessionReport(sessionID string, cap int, viewOptions ...changeenv.ViewOptions) (*SessionReport, error) {
	rep := &SessionReport{ID: sessionID, State: []store.StateRow{}, Events: []store.EventRecord{},
		Resources: []store.EventResource{}, Results: []store.ResultObservation{},
		Checkpoints: []store.SessionCheckpoint{}, Reconciliation: []store.PathReconciliation{},
		CollectionIssues: []store.CollectionIssue{}, ResultPayloadMode: string(g.resultPayloadMode),
		CollectionConfigOrigin: g.collectionConfigOrigin}
	if sessionID == "" {
		rep.Note = "no session id was given"
		return rep, nil
	}
	// Clamp here, matching the store's own default, so Truncated is computed against
	// the limit actually applied — the bug EntityReport already had and fixed.
	if cap <= 0 {
		cap = storeDefaultEventLimit
	}
	st, err := g.ix.SessionState(sessionID)
	if err != nil {
		return nil, err
	}
	if st != nil {
		rep.State = st
	}
	evs, err := g.ix.EventsForSession(sessionID, cap)
	if err != nil {
		return nil, err
	}
	if evs != nil {
		rep.Events = evs
	}
	rep.Resources, rep.ResourceCount, err = g.ix.EventResourcesForSession(sessionID, cap, 5000)
	if err != nil {
		return nil, err
	}
	rep.ResourcesTruncated = rep.ResourceCount > len(rep.Resources)
	stat, err := g.ix.EventStatForSession(sessionID)
	if err != nil {
		return nil, err
	}
	rep.EventCount = stat.Total
	rep.Truncated = stat.Total > len(rep.Events)
	rep.DecisionsRecorded = anyDecisionRecorded(rep.Events)
	rep.Results, err = g.ix.ResultsForSession(sessionID, 1000)
	if err != nil {
		return nil, err
	}
	rep.ResultStats, err = g.ix.ResultStatsForSession(sessionID)
	if err != nil {
		return nil, err
	}
	rep.Checkpoints, err = g.ix.SessionCheckpoints(sessionID, 200)
	if err != nil {
		return nil, err
	}
	rep.Reconciliation, err = g.ix.PathReconciliationsForSession(sessionID, 1000)
	if err != nil {
		return nil, err
	}
	rep.CollectionIssues, err = g.ix.CollectionIssuesForSession(sessionID, 1000)
	if err != nil {
		return nil, err
	}

	// Found is about the EVENT LOG, not the vendor file: this endpoint answers "what
	// did we capture", and a session we harvested but never observed is a miss here.
	rep.CaptureFound = rep.EventCount > 0 || len(rep.State) > 0
	viewOption := changeenv.ViewOptions{AnalyzerBundleDigest: currentAnalyzerBundle()}
	if len(viewOptions) > 0 {
		viewOption = viewOptions[0]
		if viewOption.AnalyzerBundleDigest == "" {
			viewOption.AnalyzerBundleDigest = currentAnalyzerBundle()
		}
	}
	rep.Change, err = changeenv.Build(g.ix, sessionID, viewOption)
	if err != nil {
		return nil, err
	}
	rep.Found = rep.CaptureFound || rep.Change.Available
	if !rep.Found {
		rep.Note = "no governance rows captured for this session — it was never observed by the governor"
	} else if !rep.CaptureFound {
		rep.Note = "change evidence exists, but no governed tool call was captured for this session"
	}
	if rep.Change.Available {
		claimedRuntime, claimedTitle, _, metaErr := changeenv.Metadata(g.ix, sessionID)
		if metaErr != nil {
			return nil, metaErr
		}
		if claimedRuntime != "" {
			foundHarvested := false
			if harvest.CouldMatchID(claimedRuntime, sessionID) {
				if meta, ok := harvest.Find(claimedRuntime, sessionID); ok {
					rep.Title, rep.Runtime, rep.TitleSource = meta.Title, meta.Runtime, meta.TitleSource
					foundHarvested = true
				}
			}
			if !foundHarvested {
				rep.Title, rep.Runtime, rep.TitleSource = claimedTitle, claimedRuntime, "envelope-claim"
			}
		}
	} else if meta, ok := sessionTitles()[sessionID]; ok {
		rep.Title, rep.Runtime, rep.TitleSource = meta.Title, meta.Runtime, meta.TitleSource
	}
	return rep, nil
}

// GovernedSession is one rail entry: a session we actually hold rows for.
type GovernedSession struct {
	store.SessionEventCount
	Title       string `json:"title,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	TitleSource string `json:"title_source,omitempty"`
}

// GovernedSessions backs the Governance rail. Titles are joined here for the same
// reason EntityReport joins them: an answer in UUIDs is not an answer.
func (g *Governor) GovernedSessions(limit int) ([]GovernedSession, error) {
	counts, err := g.ix.SessionsWithEvents(limit)
	if err != nil {
		return nil, err
	}
	titles := sessionTitles()
	out := make([]GovernedSession, 0, len(counts))
	for _, c := range counts {
		gs := GovernedSession{SessionEventCount: c}
		if meta, ok := titles[c.SessionID]; ok {
			gs.Title, gs.Runtime, gs.TitleSource = meta.Title, meta.Runtime, meta.TitleSource
		}
		out = append(out, gs)
	}
	return out, nil
}
