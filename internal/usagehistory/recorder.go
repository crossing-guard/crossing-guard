// Package usagehistory records model calls from runtimes' native sources into
// the store (token-usage-analytics plan §3.4). It owns the application policy
// only: which sources to read, in what order, within one byte budget, and how
// a read becomes one source write. Reading a source stays behind each
// runtime's harvest.UsageSource; persistence stays behind Repository.
//
// It is not the transcript search projection (which is all-or-nothing per
// session, bounded by document limits, and blocks a runtime on a runtime-level
// limitation) nor the summary scan (in memory, whole-file re-reads, no
// cursor). Usage needs per-source, append-only reading with no size bound.
package usagehistory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// Repository is the consumer-owned persistence port. *store.Index satisfies it.
type Repository interface {
	UsageSourceStates(runtime string) (map[string]store.UsageSourceState, error)
	WriteUsageSource(store.UsageSourceWrite) (store.UsageWriteResult, error)
	// UpdateUsageSourceRoles stores roles on sources that are otherwise up to
	// date, touching nothing else (session usage breakdown plan S-1).
	UpdateUsageSourceRoles(runtime string, roles map[string]string) (int, error)
}

// Recorder runs passes over every runtime's usage sources.
type Recorder struct {
	sources map[string]harvest.UsageSource
	repo    Repository
	now     func() time.Time
	// previousPass is when the previous pass started: a never-recorded source
	// modified since then is live and is read before the backfill (S3-8).
	previousPass time.Time
}

// New returns a recorder over the given runtime sources, by runtime name.
func New(sources map[string]harvest.UsageSource, repo Repository) *Recorder {
	return &Recorder{sources: sources, repo: repo, now: time.Now}
}

// PassResult is what one pass saw and did.
type PassResult struct {
	Discovered int // sources listed
	Recorded   int // sources read to their end at their current marker and reader
	Pending    int // sources with content left to read after this pass
	Failed     int // sources whose read failed; retried next pass
	BytesRead  int64
	Calls      int
	Removed    int // stale rows removed by completed re-reads
	Promoted   int // copies that took a removed owner's call over
	Roles      int // up-to-date sources whose role was stored without a read
	Started    time.Time
	Finished   time.Time
	// Present names the sessions whose sources this pass listed, by runtime;
	// ListingFailed names runtimes whose listing failed, whose presence is
	// therefore unknown.
	Present       map[string]map[string]bool
	ListingFailed map[string]bool
}

// work classes, in the order a pass reads them (plan §3.4 step 2).
const (
	workAppended = iota // a recorded source that grew, or a new source that is live
	workReread          // a source whose reader changed
	workBackfill        // a never-recorded source that is not live
)

type workItem struct {
	runtime string
	ref     harvest.UsageSourceRef
	state   store.UsageSourceState
	known   bool
	class   int
}

// Pass reads sources until budget bytes are spent or no source has content
// left, and writes each source's read in one transaction. A failed read skips
// that source for this pass; a failed write stops the pass and is returned,
// since the store is the part that is unavailable.
func (r *Recorder) Pass(ctx context.Context, budget int64) (PassResult, error) {
	result := PassResult{Started: r.now(), Present: map[string]map[string]bool{}, ListingFailed: map[string]bool{}}
	items, roles, err := r.plan(ctx, &result)
	if err != nil {
		return result, err
	}
	// Roles first: a metadata write spends no budget and never waits behind a
	// long re-read (S-1).
	for _, runtime := range r.runtimeNames() {
		if len(roles[runtime]) == 0 {
			continue
		}
		updated, err := r.repo.UpdateUsageSourceRoles(runtime, roles[runtime])
		if err != nil {
			return result, fmt.Errorf("%w: %v", errRepository, err)
		}
		result.Roles += updated
	}
	remaining := budget
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			result.Pending += len(items) - index
			return result, err
		}
		if remaining <= 0 {
			result.Pending += len(items) - index
			break
		}
		read, complete, err := r.readOne(ctx, item, remaining, &result)
		if err != nil {
			if errors.Is(err, errRepository) {
				result.Pending += len(items) - index
				return result, err
			}
			result.Failed++
			result.Pending++
			continue
		}
		remaining -= read
		if complete {
			result.Recorded++
		} else {
			result.Pending++
		}
	}
	r.previousPass = result.Started
	result.Finished = r.now()
	return result, nil
}

// plan lists every source and returns the ones with content to read, in
// pass order. Up-to-date sources count as recorded; those whose listing
// states a role the store does not hold yet are returned by runtime, source
// key and role, for a metadata-only write.
func (r *Recorder) plan(ctx context.Context, result *PassResult) ([]workItem, map[string]map[string]string, error) {
	var items []workItem
	roles := map[string]map[string]string{}
	for _, runtime := range r.runtimeNames() {
		source := r.sources[runtime]
		refs, err := source.UsageSources(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			result.Failed++
			result.ListingFailed[runtime] = true
			continue
		}
		present := map[string]bool{}
		for _, ref := range refs {
			present[ref.Session] = true
		}
		result.Present[runtime] = present
		states, err := r.repo.UsageSourceStates(runtime)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", errRepository, err)
		}
		reader := source.UsageReader()
		for _, ref := range refs {
			result.Discovered++
			state, known := states[ref.Key]
			item := workItem{runtime: runtime, ref: ref, state: state, known: known}
			switch {
			case !known && ref.Modified.After(r.previousPass) && !r.previousPass.IsZero():
				item.class = workAppended
			case !known:
				item.class = workBackfill
			case state.Reader != reader:
				item.class = workReread
			case state.Marker != ref.Marker || !state.Complete:
				item.class = workAppended
			default:
				result.Recorded++
				if ref.Role != "" && ref.Role != state.Role {
					if roles[runtime] == nil {
						roles[runtime] = map[string]string{}
					}
					roles[runtime][ref.Key] = ref.Role
				}
				continue
			}
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].class != items[j].class {
			return items[i].class < items[j].class
		}
		return items[i].ref.Modified.After(items[j].ref.Modified)
	})
	return items, roles, nil
}

func (r *Recorder) runtimeNames() []string {
	names := make([]string, 0, len(r.sources))
	for name := range r.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// errRepository marks a store failure, which stops the pass.
var errRepository = errors.New("usage repository unavailable")

// readOne reads one source within the remaining budget and writes it.
func (r *Recorder) readOne(ctx context.Context, item workItem, budget int64, result *PassResult) (int64, bool, error) {
	source := r.sources[item.runtime]
	reader := source.UsageReader()
	readerChanged := item.known && item.state.Reader != reader
	cursor := item.state.Cursor
	if readerChanged {
		cursor = nil // a new mapping reads the source from the start
	}
	batch, err := source.ReadUsage(ctx, harvest.UsageReadRequest{Source: item.ref, Cursor: cursor, Budget: budget})
	if err != nil {
		return 0, false, err
	}
	parent := batch.ParentSession
	if parent == "" {
		parent = item.ref.ParentSession
	}
	role := batch.Role
	if role == "" {
		role = item.ref.Role // an empty role never overwrites a stored one; the store keeps it
	}
	write := store.UsageSourceWrite{Restart: readerChanged || batch.Restarted,
		State: store.UsageSourceState{Runtime: item.runtime, Source: item.ref.Key, SessionID: item.ref.Session,
			SessionAlias: item.ref.SessionAlias, ParentSession: parent, Role: role, Marker: item.ref.Marker,
			Reader: reader,
			Cursor: batch.Cursor, Complete: batch.Complete, UpdatedAtMS: r.now().UnixMilli()}}
	for _, call := range batch.Calls {
		record, err := callRecord(item.runtime, item.ref, reader, call)
		if err != nil {
			return 0, false, err
		}
		write.Calls = append(write.Calls, record)
	}
	written, err := r.repo.WriteUsageSource(write)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %v", errRepository, err)
	}
	result.BytesRead += batch.BytesRead
	result.Calls += len(batch.Calls)
	result.Removed += written.Removed
	result.Promoted += written.Promoted
	return batch.BytesRead, batch.Complete, nil
}

// callRecord maps one neutral call to the store's row. Parts and other
// classes are written in one canonical form, so equal sets are equal text.
func callRecord(runtime string, ref harvest.UsageSourceRef, reader string, call harvest.UsageCall) (store.UsageCallRecord, error) {
	session := call.Session
	if session == "" {
		session = ref.Session
	}
	record := store.UsageCallRecord{Runtime: runtime, CallID: call.ID, SessionID: session,
		ParentSessionID: call.ParentSession, Agent: call.Agent, Source: ref.Key,
		FirstAtMS: call.FirstAt.UnixMilli(), AtMS: call.At.UnixMilli(), Model: call.Model, Effort: call.Effort,
		Client: call.Client, Input: call.Input, CacheRead: call.CacheRead, CacheWrite: call.CacheWrite,
		Output: call.Output, Reasoning: call.Reasoning, ContextWindow: call.ContextWindow, Reader: reader}
	if call.FirstAt.IsZero() {
		record.FirstAtMS = record.AtMS
	}
	if !ref.Born.IsZero() {
		born := ref.Born.UnixMilli()
		record.SourceBornMS = &born
	}
	if call.Cost != nil {
		record.Cost = &store.UsageCost{Amount: call.Cost.Amount, Unit: call.Cost.Unit, Basis: call.Cost.Basis}
	}
	var err error
	if record.Parts, err = canonicalParts(call.Parts); err != nil {
		return record, err
	}
	record.Other, err = canonicalOther(call.Other)
	return record, err
}

func canonicalParts(parts []harvest.TokenPart) (string, error) {
	if len(parts) == 0 {
		return "", nil
	}
	sorted := append([]harvest.TokenPart(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Of != sorted[j].Of {
			return sorted[i].Of < sorted[j].Of
		}
		return sorted[i].ID < sorted[j].ID
	})
	body, err := json.Marshal(sorted)
	return string(body), err
}

func canonicalOther(other []harvest.TokenCount) (string, error) {
	if len(other) == 0 {
		return "", nil
	}
	sorted := append([]harvest.TokenCount(nil), other...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	body, err := json.Marshal(sorted)
	return string(body), err
}
