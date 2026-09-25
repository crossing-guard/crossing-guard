package transcription

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sentinel errors the HTTP adapter maps to statuses. They are not end states.
var (
	ErrBusy               = errors.New("finish the current dictation first")
	ErrNotFound           = errors.New("dictation not found")
	ErrNotListening       = errors.New("dictation is not listening")
	ErrDisclosureRequired = errors.New("this backend needs your confirmation before audio leaves the machine")
	ErrDisclosureInvalid  = errors.New("confirmation expired or does not match; confirm again")
	ErrNoDisclosure       = errors.New("the selected backend does not use a disclosure")
)

// Clock lets tests drive time.
type Clock func() time.Time

// HintRequest is what the daemon knows about where a dictation is happening.
// The service applies the backend's hint policy; the source only derives.
type HintRequest struct {
	Cwd                     string
	WorkspaceSelectionID    string
	WorkspaceBindingVersion int64
}

// HintSource derives hints from validated task context. The daemon implements
// it over its cwd validation and workspace selection; the service never
// touches a path itself.
type HintSource interface {
	Hints(ctx context.Context, request HintRequest) (Hints, error)
}

// Event is one message on a dictation's stream.
type Event struct {
	Kind          string   `json:"kind"`
	Committed     string   `json:"committed,omitempty"`
	Tail          string   `json:"tail,omitempty"`
	Text          string   `json:"text,omitempty"`
	DurationMS    int64    `json:"duration_ms,omitempty"`
	Confidence    float64  `json:"confidence,omitempty"`
	HasConfidence bool     `json:"has_confidence,omitempty"`
	State         EndState `json:"state,omitempty"`
	Message       string   `json:"message,omitempty"`
}

const (
	EventPartial = "partial"
	EventEnded   = "ended"
	// eventBuffer bounds unread partials per dictation. A partial beyond it is
	// dropped, which loses nothing: each partial carries the whole provisional
	// text, so a later one supersedes it. The terminal event always lands.
	eventBuffer = 256
)

// CreateRequest opens a dictation.
type CreateRequest struct {
	Hint            HintRequest
	DisclosureToken string
}

// Created is what the browser needs to start sending frames.
type Created struct {
	ID         string `json:"id"`
	SampleRate int    `json:"sample_rate"`
}

// ServiceOptions wires a Service. Every dependency is a port.
type ServiceOptions struct {
	Config   Config
	Backend  Backend
	ClipRoot string
	Clock    Clock
	Hints    HintSource
	Logf     func(format string, values ...any)
}

// Service owns every live dictation on the daemon.
type Service struct {
	config   Config
	backend  Backend
	caps     Capabilities
	clipRoot string
	now      Clock
	hints    HintSource
	logf     func(string, ...any)

	mu         sync.Mutex
	dictations map[string]*dictation
	tokens     map[string]disclosureToken
	active     int
}

type disclosureToken struct {
	backend string
	version int
	expires time.Time
}

// dictation is one act of speaking. Its state moves listening → finishing →
// ended; ended is terminal and is reached exactly once.
type dictation struct {
	id       string
	dir      string
	clip     *Clip
	hints    Hints
	events   chan Event
	lastSeen time.Time

	state string

	committed        strings.Builder
	lastCommitted    string
	committedUntilMS int64
	tail             string

	decoding     bool
	decodeCancel context.CancelFunc
	decodeDone   chan struct{}
	lastDecodeAt time.Time
	finalCancel  context.CancelFunc
	finalDone    chan struct{}

	ended         EndState
	terminalEvent *Event
	closedEvents  bool
	dirRemoved    bool
}

// NewService builds a Service. The clip root must already exist.
func NewService(options ServiceOptions) (*Service, error) {
	if options.Backend == nil {
		return nil, errors.New("dictation service requires a backend")
	}
	if options.Hints == nil {
		return nil, errors.New("dictation service requires a hint source")
	}
	if options.ClipRoot == "" {
		return nil, errors.New("dictation service requires a clip root")
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.Logf == nil {
		options.Logf = func(string, ...any) {}
	}
	return &Service{
		config: options.Config, backend: options.Backend, caps: options.Backend.Capabilities(),
		clipRoot: options.ClipRoot, now: options.Clock, hints: options.Hints, logf: options.Logf,
		dictations: map[string]*dictation{}, tokens: map[string]disclosureToken{},
	}, nil
}

// Capabilities reports the selected backend's facts.
func (s *Service) Capabilities() Capabilities { return s.caps }

// Config returns the validated transcription configuration.
func (s *Service) Config() Config { return s.config }

// MintDisclosure issues a single-use token after the user confirmed the
// backend's disclosure text at the stated version.
func (s *Service) MintDisclosure(backend string, version int) (string, time.Duration, error) {
	if !s.caps.RequiresDisclosure || backend != s.caps.Backend {
		return "", 0, ErrNoDisclosure
	}
	disclosure := s.config.DisclosureFor(backend)
	if disclosure == nil || disclosure.Version != version {
		return "", 0, ErrDisclosureInvalid
	}
	token, err := randomID("disc")
	if err != nil {
		return "", 0, err
	}
	ttl := time.Duration(s.config.Disclosure.TokenSeconds) * time.Second
	s.mu.Lock()
	s.tokens[token] = disclosureToken{backend: backend, version: version, expires: s.now().Add(ttl)}
	s.mu.Unlock()
	return token, ttl, nil
}

// Create opens a dictation. For a disclosure backend it consumes the token
// first; hints are derived only after that, so nothing about the task is
// derived for a dictation the user did not confirm.
func (s *Service) Create(ctx context.Context, request CreateRequest) (Created, error) {
	s.mu.Lock()
	if s.active >= s.config.Limits.MaxConcurrent {
		s.mu.Unlock()
		return Created{}, ErrBusy
	}
	if s.caps.RequiresDisclosure {
		if err := s.consumeTokenLocked(request.DisclosureToken); err != nil {
			s.mu.Unlock()
			return Created{}, err
		}
	}
	s.active++
	s.mu.Unlock()

	hints, err := s.hints.Hints(ctx, request.Hint)
	if err != nil {
		s.release()
		return Created{}, err
	}
	hints = s.applyHintPolicy(hints)
	id, err := randomID("dict")
	if err != nil {
		s.release()
		return Created{}, err
	}
	dir := filepath.Join(s.clipRoot, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.release()
		return Created{}, err
	}
	d := &dictation{id: id, dir: dir, clip: NewClip(s.caps.PreferredSampleRate), hints: hints,
		events: make(chan Event, eventBuffer), lastSeen: s.now(), state: "listening"}
	s.mu.Lock()
	s.dictations[id] = d
	s.mu.Unlock()
	return Created{ID: id, SampleRate: s.caps.PreferredSampleRate}, nil
}

func (s *Service) consumeTokenLocked(token string) error {
	if token == "" {
		return ErrDisclosureRequired
	}
	entry, ok := s.tokens[token]
	delete(s.tokens, token)
	disclosure := s.config.DisclosureFor(s.caps.Backend)
	if !ok || s.now().After(entry.expires) || entry.backend != s.caps.Backend || disclosure == nil || entry.version != disclosure.Version {
		return ErrDisclosureInvalid
	}
	return nil
}

func (s *Service) applyHintPolicy(hints Hints) Hints {
	policy := s.config.HintPolicyFor(s.caps.Backend)
	out := Hints{}
	if policy.ProjectName {
		out.ProjectName = hints.ProjectName
	}
	if policy.BranchName {
		out.BranchName = hints.BranchName
	}
	out.Keywords = append([]string(nil), policy.Keywords...)
	return out
}

func (s *Service) release() {
	s.mu.Lock()
	if s.active > 0 {
		s.active--
	}
	s.mu.Unlock()
}

// Events returns the dictation's stream. It is closed after the terminal event.
func (s *Service) Events(id string) (<-chan Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dictations[id]
	if !ok {
		return nil, false
	}
	return d.events, true
}

// Frame appends one sequenced frame while listening. The state check and the
// append happen under one lock so a frame can never land after Finish has
// snapshotted the clip. Reaching the configured maximum duration finishes the
// dictation as if the user had let go.
func (s *Service) Frame(id string, seq, offset int64, data []byte) error {
	d, err := s.lookup(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if d.state != "listening" {
		s.mu.Unlock()
		return ErrNotListening
	}
	d.lastSeen = s.now()
	appendErr := d.clip.Append(seq, offset, data)
	s.mu.Unlock()
	if appendErr != nil {
		s.end(d, StateOf(appendErr), appendErr.Error(), Event{})
		return appendErr
	}
	if d.clip.DurationMS() >= int64(s.config.Limits.MaxSeconds)*1000 {
		// The cap finishes in the background. ErrNotListening there only means
		// the user's own release won the race, which is the same outcome.
		go func() { _, _ = s.Finish(id, "cap") }()
		return nil
	}
	if s.caps.EmitsPartials {
		s.maybeStartPartial(d)
	}
	return nil
}

func (s *Service) maybeStartPartial(d *dictation) {
	s.mu.Lock()
	floor := time.Duration(s.config.Streaming.PartialMinIntervalMS) * time.Millisecond
	if d.decoding || s.now().Sub(d.lastDecodeAt) < floor || d.state != "listening" {
		s.mu.Unlock()
		return
	}
	d.decoding = true
	d.lastDecodeAt = s.now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.config.Limits.FinalDeadlineSeconds)*time.Second)
	d.decodeCancel = cancel
	done := make(chan struct{})
	d.decodeDone = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		s.partialDecode(ctx, d)
		s.mu.Lock()
		d.decoding = false
		s.mu.Unlock()
	}()
}

// partialDecode runs one window decode and applies the commit and overlap rules.
func (s *Service) partialDecode(ctx context.Context, d *dictation) {
	pcm, windowStartMS := d.clip.Window(s.config.Streaming.WindowSeconds)
	clipDurationMS := windowStartMS + pcmDurationMS(len(pcm), d.clip.SampleRate())
	peak, rms := Energy(pcm)
	if peak < s.config.Limits.MinPeak || rms < s.config.Limits.MinRMS {
		return
	}
	wavPath := filepath.Join(d.dir, "window.wav")
	if err := WriteWAV(wavPath, d.clip.SampleRate(), pcm); err != nil {
		if ctx.Err() == nil {
			s.logf("dictation %s: write window: %v", d.id, err)
		}
		return
	}
	decoded, err := s.backend.Decode(ctx, DecodeRequest{WAVPath: wavPath, WorkDir: d.dir, Hints: d.hints,
		Window: true, CaptureLimit: s.config.Limits.MaxCaptureBytes}, nil)
	if err != nil {
		if ctx.Err() == nil {
			s.logf("dictation %s: window decode: %v", d.id, detailOf(err))
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.state != "listening" {
		return
	}
	text := strings.TrimSpace(decoded.Text)
	if text == "" {
		return
	}
	cutoffMS := clipDurationMS - int64(s.config.Streaming.OverlapSeconds)*1000
	committed, tail, ok := commitWindow(decoded.Words, windowStartMS, cutoffMS, text, d.committedUntilMS, d.lastCommitted)
	if !ok {
		return
	}
	for _, word := range committed {
		if d.committed.Len() > 0 {
			d.committed.WriteByte(' ')
		}
		d.committed.WriteString(word.Text)
		d.lastCommitted = word.Text
		d.committedUntilMS = windowStartMS + word.EndMS
	}
	d.tail = tail
	s.emitLocked(d, Event{Kind: EventPartial, Committed: d.committed.String(), Tail: d.tail})
}

// commitWindow is the pure partial rule. It returns the words to commit, the
// provisional tail, and false when the decode is a stray to discard. The
// stray check applies only while the last committed word can still lie inside
// the window; once it has scrolled out there is nothing to overlap with.
func commitWindow(words []Word, windowStartMS, cutoffMS int64, text string, committedUntilMS int64, lastCommitted string) ([]Word, string, bool) {
	if lastCommitted != "" && committedUntilMS >= windowStartMS &&
		!strings.Contains(strings.ToLower(text), strings.ToLower(lastCommitted)) {
		return nil, "", false
	}
	var committed []Word
	var tail []string
	for _, word := range words {
		startMS, endMS := windowStartMS+word.StartMS, windowStartMS+word.EndMS
		if startMS < committedUntilMS {
			continue
		}
		if endMS <= cutoffMS {
			committed = append(committed, word)
			continue
		}
		tail = append(tail, word.Text)
	}
	return committed, strings.Join(tail, " "), true
}

// Finish stops listening and runs the authoritative final pass. It blocks until
// the dictation has ended and returns the terminal event; the same event is
// also the last message on the stream. reason reaches the log only.
func (s *Service) Finish(id, reason string) (Event, error) {
	d, err := s.lookup(id)
	if err != nil {
		return Event{}, err
	}
	s.mu.Lock()
	if d.state != "listening" {
		s.mu.Unlock()
		return Event{}, ErrNotListening
	}
	d.state = "finishing"
	cancelPartial, partialDone := d.decodeCancel, d.decodeDone
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.config.Limits.FinalDeadlineSeconds)*time.Second)
	finalDone := make(chan struct{})
	d.finalCancel, d.finalDone = cancel, finalDone
	s.mu.Unlock()
	defer close(finalDone)
	defer cancel()
	if cancelPartial != nil {
		cancelPartial()
		<-partialDone
	}

	durationMS := d.clip.DurationMS()
	pcm := d.clip.Bytes()
	peak, rms := Energy(pcm)
	if durationMS < int64(s.config.Limits.MinMS) || peak < s.config.Limits.MinPeak || rms < s.config.Limits.MinRMS {
		return s.endFromFinish(d, EndNoSpeech, "nothing heard", Event{DurationMS: durationMS}), nil
	}
	// A cancel may have ended the dictation while the clip was being copied;
	// never write audio into a directory that is being removed.
	if !s.stillFinishing(d) {
		return s.terminal(d), nil
	}
	wavPath := filepath.Join(d.dir, "clip.wav")
	if err := WriteWAV(wavPath, d.clip.SampleRate(), pcm); err != nil {
		if !s.stillFinishing(d) {
			return s.terminal(d), nil
		}
		s.logf("dictation %s: write clip: %v", d.id, err)
		return s.endFromFinish(d, EndEngineError, "dictation failed", Event{DurationMS: durationMS}), nil
	}
	onPartial := func(text string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if d.state == "finishing" {
			s.emitLocked(d, Event{Kind: EventPartial, Tail: text})
		}
	}
	decoded, err := s.backend.Decode(ctx, DecodeRequest{WAVPath: wavPath, WorkDir: d.dir, Hints: d.hints,
		Window: false, CaptureLimit: s.config.Limits.MaxCaptureBytes}, onPartial)
	if err != nil {
		state := StateOf(err)
		if state == EndCancelled {
			return s.endFromFinish(d, EndCancelled, "", Event{DurationMS: durationMS}), nil
		}
		s.logf("dictation %s (%s): final decode: %v", d.id, reason, detailOf(err))
		return s.endFromFinish(d, state, messageFor(state), Event{Text: s.lastSeenText(d), DurationMS: durationMS}), nil
	}
	state, message, result := classifyFinal(decoded, durationMS, s.config.Limits.MinConfidence)
	return s.endFromFinish(d, state, message, result), nil
}

// classifyFinal turns a final decode into its end state.
func classifyFinal(decoded Decoded, durationMS int64, minConfidence float64) (EndState, string, Event) {
	text := strings.TrimSpace(decoded.Text)
	result := Event{Text: text, DurationMS: durationMS, Confidence: decoded.Confidence, HasConfidence: decoded.HasConfidence}
	switch {
	case text == "":
		return EndNoSpeech, "nothing heard", result
	case decoded.HasConfidence && decoded.Confidence < minConfidence:
		return EndLowConfidence, "this may not be right", result
	default:
		return EndFinal, "", result
	}
}

func (s *Service) stillFinishing(d *dictation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return d.state == "finishing"
}

func (s *Service) lastSeenText(d *dictation) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(d.committed.String() + " " + d.tail)
}

// Cancel discards the dictation and its audio.
func (s *Service) Cancel(id string) error {
	d, err := s.lookup(id)
	if err != nil {
		return err
	}
	s.end(d, EndCancelled, "", Event{})
	return nil
}

// Sweep abandons dictations that stopped sending frames while listening,
// retries any clip directory a previous end could not remove, forgets ended
// dictations whose stream has been consumed, and prunes expired tokens.
func (s *Service) Sweep() {
	s.mu.Lock()
	now := s.now()
	limit := time.Duration(s.config.Limits.AbandonSeconds) * time.Second
	var abandoned, retry []*dictation
	for id, d := range s.dictations {
		switch {
		case d.state == "listening" && now.Sub(d.lastSeen) > limit:
			abandoned = append(abandoned, d)
		case d.state == "ended" && !d.dirRemoved:
			retry = append(retry, d)
		case d.state == "ended" && now.Sub(d.lastSeen) > limit:
			delete(s.dictations, id)
		}
	}
	for token, entry := range s.tokens {
		if now.After(entry.expires) {
			delete(s.tokens, token)
		}
	}
	s.mu.Unlock()
	for _, d := range abandoned {
		s.end(d, EndAbandoned, "", Event{})
	}
	for _, d := range retry {
		s.removeClip(d)
	}
}

// end records the terminal state from outside the final pass. It stops any
// in-flight partial or final decode and waits for both before deleting the
// clip, so no process and no file outlives the dictation.
func (s *Service) end(d *dictation, state EndState, message string, result Event) Event {
	return s.finishWith(d, state, message, result, true)
}

// endFromFinish is end() as called by Finish itself, which owns the final
// decode and therefore must not wait for its own completion.
func (s *Service) endFromFinish(d *dictation, state EndState, message string, result Event) Event {
	return s.finishWith(d, state, message, result, false)
}

func (s *Service) finishWith(d *dictation, state EndState, message string, result Event, waitFinal bool) Event {
	s.mu.Lock()
	if d.state == "ended" {
		event := s.terminalLocked(d)
		s.mu.Unlock()
		return event
	}
	d.state = "ended"
	d.ended = state
	d.lastSeen = s.now()
	cancelPartial, partialDone := d.decodeCancel, d.decodeDone
	cancelFinal, finalDone := d.finalCancel, d.finalDone
	result.Kind, result.State, result.Message = EventEnded, state, message
	d.tail = ""
	d.committed.Reset()
	d.committed.WriteString(result.Text)
	d.terminalEvent = &result
	s.emitLocked(d, result)
	if !d.closedEvents {
		close(d.events)
		d.closedEvents = true
	}
	s.mu.Unlock()
	s.release()
	if cancelPartial != nil {
		cancelPartial()
		<-partialDone
	}
	if cancelFinal != nil {
		cancelFinal()
		if waitFinal {
			<-finalDone
		}
	}
	s.removeClip(d)
	return result
}

func (s *Service) removeClip(d *dictation) {
	if err := os.RemoveAll(d.dir); err != nil {
		s.logf("dictation %s: clip cleanup will be retried by the sweeper: %v", d.id, err)
		return
	}
	s.mu.Lock()
	d.dirRemoved = true
	s.mu.Unlock()
}

func (s *Service) terminal(d *dictation) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalLocked(d)
}

func (s *Service) terminalLocked(d *dictation) Event {
	if d.terminalEvent != nil {
		return *d.terminalEvent
	}
	return Event{Kind: EventEnded, State: d.ended}
}

func (s *Service) emitLocked(d *dictation, event Event) {
	if d.closedEvents {
		return
	}
	select {
	case d.events <- event:
	default:
		if event.Kind == EventEnded {
			select {
			case <-d.events:
			default:
			}
			d.events <- event
		}
	}
}

func (s *Service) lookup(id string) (*dictation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dictations[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

func messageFor(state EndState) string {
	switch state {
	case EndTimeout:
		return "took too long"
	case EndProviderError:
		return "connection to the transcription provider lost"
	default:
		return "dictation failed"
	}
}

func detailOf(err error) string {
	var typed *Error
	if errors.As(err, &typed) && typed.Detail != "" {
		return typed.Message + ": " + typed.Detail
	}
	return err.Error()
}

func randomID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw), nil
}
