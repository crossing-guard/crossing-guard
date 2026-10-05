package daemon

// Runtime model discovery (runtime-model-catalog-and-usage design §5.1). The
// twin of chat_auth.go: an optional port on the registered chat driver, a
// per-runtime route, and "unsupported" when the port is absent. The adapter
// knows how its runtime lists models; this file knows only the neutral entry
// shape, the bounded runner every discovery process goes through, and a
// small cache. No model, price, unit or id grammar lives here.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"crossing-guard/harvest"
)

// chatModelDiscoverer is the optional port. The adapter resolves its own
// binary, builds its commands, and runs them only through env.
type chatModelDiscoverer interface {
	DiscoverChatModels(ctx context.Context, env ChatModelEnv) (ChatModelDiscovery, error)
}

// ChatModelEnv is what the framework hands an adapter for one discovery.
type ChatModelEnv struct {
	// Run executes a one-shot command and returns its stdout.
	Run func(*exec.Cmd) ([]byte, error)
	// Session starts an interactive command (stdin plus a line stream).
	Session func(*exec.Cmd) (*RunnerSession, error)
}

// ChatModelDiscovery is an adapter's answer. Binary and Scope are shown in
// Settings; Rejected counts entries the adapter could not map.
type ChatModelDiscovery struct {
	Models   []ChatModelOption
	Binary   string
	Scope    string
	Rejected int
}

// Reason codes: a fixed framework set. A reason never quotes vendor output,
// which is measured to echo configuration contents (S-RT7).
const (
	modelReasonNotInstalled   = "not-installed"
	modelReasonExited         = "exited-with-error"
	modelReasonTimedOut       = "timed-out"
	modelReasonTooLarge       = "output-too-large"
	modelReasonUnparseable    = "unparseable"
	modelReasonTooManyEntries = "too-many-entries"
	modelReasonWorkDirInRepo  = "work-dir-in-repository"
)

// ModelDiscoveryError carries one reason code.
type ModelDiscoveryError struct{ Reason string }

func (e *ModelDiscoveryError) Error() string { return "model discovery failed: " + e.Reason }

func modelDiscoveryFailure(reason string) error { return &ModelDiscoveryError{Reason: reason} }

func modelDiscoveryReason(err error) string {
	var failure *ModelDiscoveryError
	if errors.As(err, &failure) {
		return failure.Reason
	}
	return modelReasonUnparseable
}

// boundedRunner runs runtime CLIs for discovery. It owns the deadline, the
// kill, the output caps and the working directory; the adapter's command
// contributes only its binary, arguments and environment. Stderr is always
// discarded.
type boundedRunner struct {
	ctx       context.Context
	workDir   string
	maxBytes  int64
	waitDelay time.Duration
}

// prepare rebuilds cmd under ctx (a child of the runner's deadline) and the
// runner's directory.
func (r boundedRunner) prepare(ctx context.Context, cmd *exec.Cmd) (*exec.Cmd, error) {
	if err := r.checkWorkDir(); err != nil {
		return nil, err
	}
	if cmd == nil || cmd.Path == "" || len(cmd.Args) == 0 {
		return nil, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	bounded.Env = cmd.Env
	bounded.Dir = r.workDir
	bounded.Stderr = io.Discard
	bounded.WaitDelay = r.waitDelay
	prepareTaskProcess(bounded)
	bounded.Cancel = func() error { return interruptTaskProcess(bounded) }
	return bounded, nil
}

// checkWorkDir enforces "never in a repository" at run time (S-RT8): the
// directory exists, is empty of anything a runtime would read as a project,
// and no ancestor holds a .git entry.
func (r boundedRunner) checkWorkDir() error {
	if err := os.MkdirAll(r.workDir, 0o700); err != nil {
		return modelDiscoveryFailure(modelReasonNotInstalled)
	}
	for dir := filepath.Clean(r.workDir); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return modelDiscoveryFailure(modelReasonWorkDirInRepo)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return nil
		}
	}
}

// Run executes one command to completion and returns its stdout.
func (r boundedRunner) Run(cmd *exec.Cmd) ([]byte, error) {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	bounded, err := r.prepare(ctx, cmd)
	if err != nil {
		return nil, err
	}
	stdout := &boundedWriter{max: r.maxBytes, overflowed: cancel}
	bounded.Stdout = stdout
	if err := bounded.Start(); err != nil {
		return nil, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	waitErr := bounded.Wait()
	switch {
	case stdout.overflow:
		return nil, modelDiscoveryFailure(modelReasonTooLarge)
	case r.ctx.Err() != nil:
		return nil, modelDiscoveryFailure(modelReasonTimedOut)
	case errors.Is(waitErr, exec.ErrWaitDelay):
		return nil, modelDiscoveryFailure(modelReasonTimedOut)
	case waitErr != nil:
		return nil, modelDiscoveryFailure(modelReasonExited)
	}
	return stdout.buf, nil
}

// RunnerSession is an interactive discovery process: write requests to Stdin,
// read newline-delimited replies from Lines. Lines closes when the process
// ends, its output exceeds the runner's cap, or the deadline passes; Close
// ends it early. Err reports why Lines closed.
type RunnerSession struct {
	Stdin io.Writer
	Lines <-chan []byte
	Close func()
	err   error
	mu    sync.Mutex
}

// Err is the reason code the stream ended with, or nil.
func (s *RunnerSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *RunnerSession) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Session starts an interactive command under the runner's bounds.
func (r boundedRunner) Session(cmd *exec.Cmd) (*RunnerSession, error) {
	ctx, cancel := context.WithCancel(r.ctx)
	bounded, err := r.prepare(ctx, cmd)
	if err != nil {
		cancel()
		return nil, err
	}
	stdin, err := bounded.StdinPipe()
	if err != nil {
		cancel()
		return nil, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	stdout, err := bounded.StdoutPipe()
	if err != nil {
		cancel()
		return nil, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	if err := bounded.Start(); err != nil {
		cancel()
		return nil, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	lines := make(chan []byte)
	session := &RunnerSession{Stdin: stdin, Lines: lines}
	var closeOnce sync.Once
	session.Close = func() { closeOnce.Do(cancel) }
	// A descendant outside the process group can keep stdout open after the
	// kill; closing our read end at the deadline ends the read loop anyway, so
	// Wait (and WaitDelay) are always reached (P-RT5).
	go func() {
		<-ctx.Done()
		if closer, ok := stdout.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	go func() {
		defer close(lines)
		defer func() { _ = bounded.Wait() }()
		defer session.Close()
		reader := bufio.NewReaderSize(stdout, 64<<10)
		var total int64
		for {
			line, readErr := readBoundedLine(reader, r.maxBytes-total)
			total += int64(len(line)) + 1
			if readErr == errLineTooLong || total > r.maxBytes {
				session.fail(modelDiscoveryFailure(modelReasonTooLarge))
				return
			}
			if len(line) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					if r.ctx.Err() != nil {
						session.fail(modelDiscoveryFailure(modelReasonTimedOut))
					}
					return
				}
			}
			if readErr != nil {
				if r.ctx.Err() != nil {
					session.fail(modelDiscoveryFailure(modelReasonTimedOut))
				}
				return
			}
		}
	}()
	return session, nil
}

var errLineTooLong = errors.New("line exceeds the discovery output bound")

// readBoundedLine reads one line without holding more than limit bytes.
func readBoundedLine(reader *bufio.Reader, limit int64) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if int64(len(line)+len(chunk)) > limit {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
		}
		return line, err
	}
}

// boundedWriter keeps at most max bytes and signals the first byte past it; a
// truncated output is never parsed.
type boundedWriter struct {
	buf        []byte
	max        int64
	overflow   bool
	overflowed func()
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.overflow {
		return len(p), nil
	}
	if int64(len(w.buf)+len(p)) > w.max {
		w.overflow = true
		w.overflowed()
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// Model list states.
const (
	modelStateFresh       = "fresh"
	modelStateStale       = "stale"
	modelStateUnavailable = "unavailable"
	modelStateUnsupported = "unsupported"
)

// ChatModelList is the typed body of GET /api/chat/models.
type ChatModelList struct {
	Runtime           string            `json:"runtime"`
	State             string            `json:"state"`
	ObservedAt        *time.Time        `json:"observed_at,omitempty"`
	Digest            string            `json:"digest,omitempty"`
	Scope             string            `json:"scope,omitempty"`
	Binary            string            `json:"binary,omitempty"`
	InterfaceRevision string            `json:"interface_revision,omitempty"`
	ReasonCode        string            `json:"reason_code,omitempty"`
	Rejected          int               `json:"rejected,omitempty"`
	Models            []ChatModelOption `json:"models"`
}

type chatModelCacheEntry struct {
	list        ChatModelList
	good        bool // list holds a successful discovery (fresh or stale)
	lastAttempt time.Time
	running     chan struct{} // closed when the in-flight discovery ends
}

// chatModelCache is the only new mechanism besides the runner: discovery is
// too heavy to run per page load. One entry per runtime, one discovery at a
// time per runtime, stale-while-revalidate.
type chatModelCache struct {
	mu      sync.Mutex
	entries map[string]*chatModelCacheEntry
	now     func() time.Time
	bounds  func() ConsoleModelsDefaults
	workDir func() string
}

var chatModels = &chatModelCache{
	entries: map[string]*chatModelCacheEntry{},
	now:     time.Now,
	bounds:  func() ConsoleModelsDefaults { config, _ := consoleConfig(); return config.Models },
	workDir: func() string { return filepath.Join(filepath.Dir(indexPath()), "model-discovery") },
}

func chatModelSource(runtime string) (chatModelDiscoverer, bool) {
	discoverer, ok := chatDrivers[runtime].(chatModelDiscoverer)
	return discoverer, ok
}

// List returns the runtime's list, discovering on first use and refreshing in
// the background once the list is older than refresh_after_seconds.
func (c *chatModelCache) List(runtime string) ChatModelList {
	if _, ok := chatModelSource(runtime); !ok {
		return ChatModelList{Runtime: runtime, State: modelStateUnsupported, Models: []ChatModelOption{}}
	}
	bounds := c.bounds()
	c.mu.Lock()
	entry := c.entries[runtime]
	if entry == nil || (!entry.good && entry.running == nil && c.now().Sub(entry.lastAttempt) >= time.Duration(bounds.MinRefreshSeconds)*time.Second) {
		wait := c.startLocked(runtime)
		c.mu.Unlock()
		<-wait
		return c.snapshot(runtime)
	}
	if entry.running != nil && !entry.good {
		wait := entry.running
		c.mu.Unlock()
		<-wait
		return c.snapshot(runtime)
	}
	if entry.good && entry.running == nil && entry.list.ObservedAt != nil &&
		c.now().Sub(*entry.list.ObservedAt) >= time.Duration(bounds.RefreshAfterSeconds)*time.Second {
		entry.list.State = modelStateStale
		// A failed refresh leaves ObservedAt old; min_refresh_seconds keeps a
		// broken runtime from being rerun on every read (P-RT4).
		if c.now().Sub(entry.lastAttempt) >= time.Duration(bounds.MinRefreshSeconds)*time.Second {
			c.startLocked(runtime)
		}
	}
	list := cloneChatModelList(entry.list)
	c.mu.Unlock()
	return list
}

// Refresh reruns discovery now unless one ran within min_refresh_seconds.
func (c *chatModelCache) Refresh(runtime string) ChatModelList {
	if _, ok := chatModelSource(runtime); !ok {
		return c.List(runtime)
	}
	bounds := c.bounds()
	c.mu.Lock()
	entry := c.entries[runtime]
	var wait chan struct{}
	switch {
	case entry != nil && entry.running != nil:
		wait = entry.running
	case entry != nil && c.now().Sub(entry.lastAttempt) < time.Duration(bounds.MinRefreshSeconds)*time.Second:
		list := cloneChatModelList(entry.list)
		c.mu.Unlock()
		return list
	default:
		wait = c.startLocked(runtime)
	}
	c.mu.Unlock()
	<-wait
	return c.snapshot(runtime)
}

func (c *chatModelCache) snapshot(runtime string) ChatModelList {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[runtime]; entry != nil {
		return cloneChatModelList(entry.list)
	}
	return ChatModelList{Runtime: runtime, State: modelStateUnavailable, Models: []ChatModelOption{}}
}

// startLocked begins one discovery; c.mu is held by the caller.
func (c *chatModelCache) startLocked(runtime string) chan struct{} {
	entry := c.entries[runtime]
	if entry == nil {
		entry = &chatModelCacheEntry{list: ChatModelList{Runtime: runtime, State: modelStateUnavailable, Models: []ChatModelOption{}}}
		c.entries[runtime] = entry
	}
	if entry.running != nil {
		return entry.running
	}
	done := make(chan struct{})
	entry.running = done
	entry.lastAttempt = c.now()
	bounds, workDir := c.bounds(), c.workDir()
	go func() {
		list := discoverChatModels(runtime, bounds, workDir, c.now)
		c.mu.Lock()
		if list.State == modelStateFresh {
			entry.list, entry.good = list, true
		} else if entry.good {
			// Keep the last good list, labelled stale, with the new reason.
			entry.list.State, entry.list.ReasonCode = modelStateStale, list.ReasonCode
		} else {
			entry.list = list
		}
		entry.running = nil
		c.mu.Unlock()
		close(done)
	}()
	return done
}

// Model returns the cached entry for a concrete id, if the runtime has a
// successful list. A cold or failed cache has no entries (fail closed).
func (c *chatModelCache) Model(runtime, id string) (ChatModelOption, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[runtime]
	if entry == nil || !entry.good {
		return ChatModelOption{}, false
	}
	for _, model := range entry.list.Models {
		if model.ID == id {
			model.Effort = cloneEffortCapability(model.Effort)
			return model, true
		}
	}
	return ChatModelOption{}, false
}

// discoverChatModels runs the adapter once under the configured bounds and
// validates every entry it returns.
func discoverChatModels(runtime string, bounds ConsoleModelsDefaults, workDir string, now func() time.Time) ChatModelList {
	list := ChatModelList{Runtime: runtime, State: modelStateUnavailable, Models: []ChatModelOption{},
		InterfaceRevision: harvest.CLIRevision(runtime)}
	source, ok := chatModelSource(runtime)
	if !ok {
		list.State = modelStateUnsupported
		return list
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(bounds.DiscoveryTimeoutSeconds)*time.Second)
	defer cancel()
	runner := boundedRunner{ctx: ctx, workDir: workDir, maxBytes: bounds.DiscoveryOutputMaxBytes, waitDelay: time.Second}
	discovery, err := source.DiscoverChatModels(ctx, ChatModelEnv{Run: runner.Run, Session: runner.Session})
	list.Binary, list.Scope = discovery.Binary, discovery.Scope
	if err != nil {
		list.ReasonCode = modelDiscoveryReason(err)
		if ctx.Err() != nil {
			list.ReasonCode = modelReasonTimedOut
		}
		return list
	}
	if len(discovery.Models) > bounds.MaxEntries {
		list.ReasonCode = modelReasonTooManyEntries
		return list
	}
	declared := map[string]bool{}
	if provider, ok := chatDrivers[runtime].(chatCapabilityProvider); ok {
		for _, model := range provider.ChatCapability().Models {
			declared[model.ID] = true
		}
	}
	modelBounds := chatModelBounds{MaxIDBytes: bounds.MaxIDBytes, MaxLabelBytes: bounds.MaxLabelBytes}
	seen := map[string]bool{}
	list.Rejected = discovery.Rejected
	for _, model := range discovery.Models {
		model.Source = chatModelSourceRuntime
		if declared[model.ID] || seen[model.ID] || validateChatModelOption(model, true, modelBounds) != nil {
			list.Rejected++
			continue
		}
		seen[model.ID] = true
		list.Models = append(list.Models, model)
	}
	observed := now().UTC()
	list.State, list.ObservedAt, list.Digest = modelStateFresh, &observed, chatModelDigest(list.Models)
	return list
}

// chatModelDigest identifies a list by content; a future named route pins it.
func chatModelDigest(models []ChatModelOption) string {
	sorted := append([]ChatModelOption(nil), models...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	encoded, _ := json.Marshal(sorted)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func handleChatModels(w http.ResponseWriter, r *http.Request) {
	runtimeName := r.URL.Query().Get("runtime")
	if _, ok := chatDrivers[runtimeName]; !ok {
		http.Error(w, "unsupported runtime", http.StatusBadRequest)
		return
	}
	writeJSON(w, chatModels.List(runtimeName))
}

func handleChatModelsRefresh(w http.ResponseWriter, r *http.Request) {
	runtimeName := r.URL.Query().Get("runtime")
	if _, ok := chatDrivers[runtimeName]; !ok {
		http.Error(w, "unsupported runtime", http.StatusBadRequest)
		return
	}
	writeJSON(w, chatModels.Refresh(runtimeName))
}

func cloneChatModelList(list ChatModelList) ChatModelList {
	list.Models = append([]ChatModelOption(nil), list.Models...)
	for i := range list.Models {
		list.Models[i].Effort = cloneEffortCapability(list.Models[i].Effort)
	}
	return list
}
