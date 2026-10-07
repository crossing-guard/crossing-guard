package harvest

// UsageSource is the optional capability through which the usage recorder
// reads model calls from a runtime's native sources (token-usage-analytics
// plan §3.3). It is discovered by type assertion, like
// TranscriptProjectionSource: a runtime without it records no calls and
// nothing else changes (ADR 0020, open/closed).
//
// File-backed runtimes read through readFileUsage below, which reuses the
// lifecycle tailer's bounded record reader (plan §3.4, red-team S2-1): no
// trailing partial record is ever committed, and a record over
// MaxLifecycleRecordBytes is skipped without being allocated, so no line can
// stop a source's progress (S2-2).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"
)

// UsageSource lists a runtime's native usage sources and reads calls from them.
type UsageSource interface {
	// UsageReader names the mapping that produced a call. A new identity makes
	// the recorder read every live source of the runtime again (ADR 0026).
	UsageReader() string
	// UsageSources lists every source cheaply: a directory listing and stat,
	// or one query, never a read of source content. A small metadata file the
	// vendor writes beside a source may be read (cached by its stat).
	UsageSources(ctx context.Context) ([]UsageSourceRef, error)
	// ReadUsage reads from the request's cursor toward the end within its byte
	// budget and returns the calls seen and the cursor to resume from.
	ReadUsage(ctx context.Context, request UsageReadRequest) (UsageBatch, error)
}

// UsageSourceRef is one native source. Key is opaque and unique within the
// runtime; it is never path text (plan invariant 8).
type UsageSourceRef struct {
	Key string
	// Session is the session the source belongs to. SessionAlias is another id
	// that names the same session (a child may name its parent by it), and
	// ParentSession is set when the session is itself delegated by another.
	Session       string
	SessionAlias  string
	ParentSession string
	// Role is the vendor-published agent type of a delegated source (for
	// example "general-purpose"), when the listing can state it; "" otherwise.
	// An empty role never overwrites one already stored (session usage
	// breakdown plan S-1).
	Role string
	// Marker changes whenever the source may hold new calls.
	Marker   string
	Size     int64
	Modified time.Time
	// Born is the source's birth time where the platform reports it; zero
	// otherwise. It orders copies of one call (plan §3.5, D-4).
	Born time.Time
	// locator is where the adapter reads the source from. It never leaves
	// the adapter and is never stored.
	locator string
}

// UsageReadRequest asks for calls after Cursor, reading at most Budget bytes.
// An empty cursor reads from the start.
type UsageReadRequest struct {
	Source UsageSourceRef
	Cursor []byte
	Budget int64
}

// UsageBatch is one read's yield.
type UsageBatch struct {
	Calls  []UsageCall
	Cursor []byte
	// ParentSession is the source's parent as learned while reading, for a
	// runtime that states it only inside the source.
	ParentSession string
	// Role is the source's vendor-published agent type as learned while
	// reading, for a runtime that states it only inside the source; it wins
	// over the ref's.
	Role      string
	BytesRead int64
	// Complete is true when the read stopped because the source held no
	// further complete record, and false when the budget stopped it.
	Complete bool
	// Restarted is true when the source no longer matched its cursor (it was
	// rewritten), so this read started over from the beginning.
	Restarted bool
}

// UsageSources returns every registered runtime that implements UsageSource,
// by runtime name.
func UsageSources() map[string]UsageSource {
	out := map[string]UsageSource{}
	for name, runtime := range runtimes {
		if source, ok := runtime.(UsageSource); ok {
			out[name] = source
		}
	}
	return out
}

// SourcePrefixWindow is how many leading bytes of a file source identify its
// generation. It is the lifecycle tailer's window; both callers share it.
const SourcePrefixWindow int64 = 64 << 10

// SourcePrefixDigest digests the first length bytes of a file (fewer when the
// file is shorter), in the store's digest format. The lifecycle tailer calls it
// with SourcePrefixWindow; the usage reader with min(window, committed offset),
// so a small file that only grows is never taken for a rewrite (S2-6).
func SourcePrefixDigest(path string, length int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	prefix, err := io.ReadAll(io.LimitReader(file, length))
	if err != nil {
		return "", err
	}
	return lifecycleDigest(prefix), nil
}

// fileUsageCursor is the cursor every file source carries: where the committed
// read ends, the line number there (call ids depend on it, S3-2), the reader's
// discard state inside a skipped long record, and the prefix digest that
// detects a rewrite. State is the adapter's own carry-forward state.
type fileUsageCursor struct {
	Offset    int64                  `json:"offset"`
	Line      int64                  `json:"line"`
	Discard   *lifecycleDiscardState `json:"discard,omitempty"`
	Digest    string                 `json:"digest,omitempty"`
	DigestLen int64                  `json:"digest_len,omitempty"`
	State     json.RawMessage        `json:"state,omitempty"`
}

// fileUsageLine handles one complete record and its 1-based line number.
type fileUsageLine func(record []byte, line int64)

// fileUsageAdapter is what a file-backed runtime supplies to readFileUsage.
type fileUsageAdapter struct {
	runtime string
	// begin receives the carried state (nil after a restart) and returns the
	// line handler; finish returns the calls seen and the state to carry.
	begin  func(state json.RawMessage) fileUsageLine
	finish func() ([]UsageCall, json.RawMessage, error)
}

// usageRecordsUnbounded lets the byte budget alone bound a read (S3-3).
const usageRecordsUnbounded = int(^uint(0) >> 1)

// usageCancelCheckRecords is how many records pass between cancellation
// checks: often enough to stop promptly, rare enough to cost nothing.
const usageCancelCheckRecords = 1024

func readFileUsage(ctx context.Context, path string, cursorBody []byte, budget int64,
	adapter fileUsageAdapter) (UsageBatch, error) {
	cursor := fileUsageCursor{}
	if len(cursorBody) > 0 {
		if err := json.Unmarshal(cursorBody, &cursor); err != nil {
			return UsageBatch{}, fmt.Errorf("decode usage cursor: %w", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return UsageBatch{}, err
	}
	restarted, err := fileSourceRewritten(path, info.Size(), cursor)
	if err != nil {
		return UsageBatch{}, err
	}
	if restarted {
		cursor = fileUsageCursor{}
	}
	reader, err := newLifecycleRecordReader(LifecycleReadRequest{Path: path, Runtime: adapter.runtime,
		Offset: cursor.Offset, SourceLine: cursor.Line, MaxBytes: budget,
		MaxRecords: usageRecordsUnbounded}, cursor.Discard)
	if err != nil {
		return UsageBatch{}, err
	}
	defer func() { _ = reader.Close() }()
	handle := adapter.begin(cursor.State)
	for records := 0; ; records++ {
		if records%usageCancelCheckRecords == 0 {
			if err := ctx.Err(); err != nil {
				return UsageBatch{}, err
			}
		}
		record, ok, err := reader.Next()
		if err != nil {
			return UsageBatch{}, err
		}
		if !ok {
			break
		}
		handle(record, reader.sourceLine)
	}
	calls, state, err := adapter.finish()
	if err != nil {
		return UsageBatch{}, err
	}
	next := fileUsageCursor{Offset: reader.offset, Line: reader.sourceLine, State: state}
	if reader.discard != nil {
		discard := *reader.discard
		next.Discard = &discard
	}
	next.DigestLen = min(SourcePrefixWindow, next.Offset)
	if next.Digest, err = SourcePrefixDigest(path, next.DigestLen); err != nil {
		return UsageBatch{}, err
	}
	body, err := json.Marshal(next)
	if err != nil {
		return UsageBatch{}, err
	}
	return UsageBatch{Calls: calls, Cursor: body, BytesRead: reader.bytesInspected,
		Complete: !reader.continuation, Restarted: restarted}, nil
}

// fileSourceRewritten reports whether the file no longer extends what the
// cursor read: it shrank below the committed offset, or its first DigestLen
// bytes changed.
func fileSourceRewritten(path string, size int64, cursor fileUsageCursor) (bool, error) {
	if cursor.Offset == 0 && cursor.Line == 0 {
		return false, nil
	}
	if size < cursor.Offset {
		return true, nil
	}
	digest, err := SourcePrefixDigest(path, cursor.DigestLen)
	if err != nil {
		return false, err
	}
	return digest != cursor.Digest, nil
}

// fileUsageMarker changes whenever a file may hold new content.
func fileUsageMarker(info os.FileInfo) string {
	return strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// usageSourceKey is an opaque key for a path relative to a vendor root:
// never path text, and distinct for worktree twins and repeated agent file
// names (S2-3).
func usageSourceKey(relative string) string {
	sum := sha256.Sum256([]byte(relative))
	return hex.EncodeToString(sum[:16])
}

// usageCallFolder keeps one call per id within a read: a later line of an id
// replaces the counts (the last line is the complete one) and keeps the first
// line's time as FirstAt.
type usageCallFolder struct {
	order []string
	calls map[string]UsageCall
}

func newUsageCallFolder() *usageCallFolder {
	return &usageCallFolder{calls: map[string]UsageCall{}}
}

func (f *usageCallFolder) observe(call UsageCall) {
	previous, seen := f.calls[call.ID]
	if !seen {
		f.order = append(f.order, call.ID)
		f.calls[call.ID] = call
		return
	}
	first := previous.FirstAt
	if first.IsZero() || (!call.FirstAt.IsZero() && call.FirstAt.Before(first)) {
		first = call.FirstAt
	}
	if admitted, ok := AdmitUsageCall(call); ok {
		admitted.FirstAt = first
		f.calls[call.ID] = admitted
		return
	}
	previous.FirstAt = first
	f.calls[call.ID] = previous
}

// admitted returns the recorded calls in first-seen order.
func (f *usageCallFolder) admitted() []UsageCall {
	out := make([]UsageCall, 0, len(f.order))
	for _, id := range f.order {
		if call, ok := AdmitUsageCall(f.calls[id]); ok {
			out = append(out, call)
		}
	}
	return out
}

// statRegular stats a path that must be a regular file.
func statRegular(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("usage source %s is not a regular file", path)
	}
	return info, nil
}

// errUsageLocator is returned when a ref did not come from this adapter.
var errUsageLocator = errors.New("usage source has no locator; list it with UsageSources")

// sortUsageSources orders sources newest first, then by key, so listings are
// deterministic.
func sortUsageSources(refs []UsageSourceRef) {
	sort.SliceStable(refs, func(i, j int) bool {
		if !refs[i].Modified.Equal(refs[j].Modified) {
			return refs[i].Modified.After(refs[j].Modified)
		}
		return refs[i].Key < refs[j].Key
	})
}
