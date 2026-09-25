package transcription

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Config is the transcription section of speech.json. Every number the feature
// needs lives here; the daemon and the browser hold no fallback values. Backend
// sections are generic: this file knows the keys every backend shares (hints,
// disclosure) and hands the rest to the selected adapter's factory untouched,
// so adding a backend never edits this file.
type Config struct {
	Backend    string                    `json:"backend"`
	Backends   map[string]BackendSection `json:"backends"`
	Disclosure DisclosureLimits          `json:"disclosure"`
	Audio      Audio                     `json:"audio"`
	Limits     Limits                    `json:"limits"`
	Streaming  Streaming                 `json:"streaming"`
	UI         UI                        `json:"ui"`
}

// BackendSection is one backend's configuration: the shared keys parsed here
// and the adapter-specific keys kept raw for the adapter to decode strictly.
type BackendSection struct {
	Hints      HintPolicy
	Disclosure *Disclosure
	Settings   json.RawMessage
}

// UnmarshalJSON splits the shared keys from the adapter-specific ones. The
// shared keys are decoded strictly here. Adapter keys are decoded strictly by
// the adapter's factory, which runs for the selected backend at load; the
// speech loader also checks that every configured section names a registered
// backend, so a misspelled section cannot hide.
func (b *BackendSection) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if hints, ok := fields["hints"]; ok {
		if err := strictDecode(hints, &b.Hints); err != nil {
			return fmt.Errorf("hints: %w", err)
		}
		delete(fields, "hints")
	}
	if disclosure, ok := fields["disclosure"]; ok {
		b.Disclosure = &Disclosure{}
		if err := strictDecode(disclosure, b.Disclosure); err != nil {
			return fmt.Errorf("disclosure: %w", err)
		}
		delete(fields, "disclosure")
	}
	settings, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	b.Settings = settings
	return nil
}

// MarshalJSON keeps the section round-trippable for tests and tooling.
func (b BackendSection) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(b.Settings) > 0 {
		if err := json.Unmarshal(b.Settings, &fields); err != nil {
			return nil, err
		}
	}
	hints, err := json.Marshal(b.Hints)
	if err != nil {
		return nil, err
	}
	fields["hints"] = hints
	if b.Disclosure != nil {
		disclosure, err := json.Marshal(b.Disclosure)
		if err != nil {
			return nil, err
		}
		fields["disclosure"] = disclosure
	}
	return json.Marshal(fields)
}

// DecodeSettings strictly decodes the adapter-specific keys into target.
func (b BackendSection) DecodeSettings(target any) error {
	raw := b.Settings
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return strictDecode(raw, target)
}

func strictDecode(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// Disclosure is the operator-authored text a disclosure backend shows before
// audio leaves the machine. Version changes invalidate outstanding tokens.
type Disclosure struct {
	Text    string `json:"text"`
	Version int    `json:"version"`
}

// DisclosureLimits bounds the single-use disclosure token.
type DisclosureLimits struct {
	TokenSeconds int `json:"token_seconds"`
}

// HintPolicy says which hints a backend may receive.
type HintPolicy struct {
	ProjectName bool     `json:"project_name"`
	BranchName  bool     `json:"branch_name"`
	Keywords    []string `json:"keywords"`
}

// Audio fixes the capture shape the browser must produce.
type Audio struct {
	Channels int `json:"channels"`
	Bits     int `json:"bits"`
}

// Limits are the duration, energy, confidence, deadline, and cleanup ceilings.
type Limits struct {
	MinMS                int     `json:"min_ms"`
	MaxSeconds           int     `json:"max_seconds"`
	SilenceStopSeconds   int     `json:"silence_stop_seconds"`
	MinPeak              float64 `json:"min_peak"`
	MinRMS               float64 `json:"min_rms"`
	MinConfidence        float64 `json:"min_confidence"`
	FinalDeadlineSeconds int     `json:"final_deadline_seconds"`
	MaxConcurrent        int     `json:"max_concurrent"`
	AbandonSeconds       int     `json:"abandon_seconds"`
	SweepSeconds         int     `json:"sweep_seconds"`
	FailurePauseCount    int     `json:"failure_pause_count"`
	FailurePauseSeconds  int     `json:"failure_pause_seconds"`
	MaxCaptureBytes      int64   `json:"max_capture_bytes"`
}

// Streaming shapes the frame upload and the partial decode loop.
type Streaming struct {
	FrameIntervalMS      int   `json:"frame_interval_ms"`
	MaxFrameBytes        int64 `json:"max_frame_bytes"`
	PartialMinIntervalMS int   `json:"partial_min_interval_ms"`
	WindowSeconds        int   `json:"window_seconds"`
	OverlapSeconds       int   `json:"overlap_seconds"`
	KeepaliveSeconds     int   `json:"keepalive_seconds"`
	MaxStreams           int   `json:"max_streams"`
}

// UI carries the browser-side preferences the capabilities endpoint publishes.
type UI struct {
	PushToTalk           string `json:"push_to_talk"`
	HoldThresholdMS      int    `json:"hold_threshold_ms"`
	AutoSend             bool   `json:"auto_send"`
	AutoSendMinWords     int    `json:"auto_send_min_words"`
	DimTailWords         int    `json:"dim_tail_words"`
	TintSeconds          int    `json:"tint_seconds"`
	FirstRunHintSessions int    `json:"first_run_hint_sessions"`
}

// Validate checks every shared field and the shared keys of every backend
// section. Adapter-specific keys are validated by the adapter's factory, which
// is why a valid file can still report a backend as not ready.
func (c *Config) Validate() error {
	c.Backend = strings.TrimSpace(c.Backend)
	if c.Backend == "" {
		return errors.New("backend is required")
	}
	if _, ok := c.Backends[c.Backend]; !ok {
		return fmt.Errorf("backends.%s section is required for the selected backend", c.Backend)
	}
	for name, entry := range c.Backends {
		if err := entry.Hints.validate(); err != nil {
			return fmt.Errorf("backends.%s.hints: %w", name, err)
		}
		if entry.Disclosure != nil {
			if err := entry.Disclosure.validate(entry.Hints); err != nil {
				return fmt.Errorf("backends.%s.disclosure: %w", name, err)
			}
		}
		c.Backends[name] = entry
	}
	if c.Disclosure.TokenSeconds <= 0 {
		return errors.New("disclosure.token_seconds must be positive")
	}
	if c.Audio.Channels != 1 {
		return errors.New("audio.channels must be 1")
	}
	if c.Audio.Bits != 16 {
		return errors.New("audio.bits must be 16")
	}
	if err := c.Limits.validate(); err != nil {
		return fmt.Errorf("limits: %w", err)
	}
	if err := c.Streaming.validate(); err != nil {
		return fmt.Errorf("streaming: %w", err)
	}
	if err := c.UI.validate(); err != nil {
		return fmt.Errorf("ui: %w", err)
	}
	if c.Streaming.WindowSeconds > c.Limits.MaxSeconds {
		return errors.New("streaming.window_seconds cannot exceed limits.max_seconds")
	}
	return nil
}

// Section returns the selected backend's section.
func (c *Config) Section(backend string) (BackendSection, bool) {
	section, ok := c.Backends[backend]
	return section, ok
}

// HintPolicyFor returns a backend's hint policy, empty when unconfigured.
func (c *Config) HintPolicyFor(backend string) HintPolicy {
	return c.Backends[backend].Hints
}

// DisclosureFor returns a backend's disclosure text, or nil when it has none.
func (c *Config) DisclosureFor(backend string) *Disclosure {
	return c.Backends[backend].Disclosure
}

// validate applies the one rule shared by every disclosure: it must name each
// hint the operator enabled, so what leaves the machine is what the card says.
func (d *Disclosure) validate(hints HintPolicy) error {
	d.Text = strings.TrimSpace(d.Text)
	if d.Text == "" || d.Version <= 0 {
		return errors.New("text and a positive version are required")
	}
	lower := strings.ToLower(d.Text)
	if hints.ProjectName && !strings.Contains(lower, "project") {
		return errors.New("text must name the project hint when hints.project_name is on")
	}
	if hints.BranchName && !strings.Contains(lower, "branch") {
		return errors.New("text must name the branch hint when hints.branch_name is on")
	}
	if len(hints.Keywords) > 0 && !strings.Contains(lower, "keyword") {
		return errors.New("text must name the keyword hint when hints.keywords is set")
	}
	return nil
}

func (h *HintPolicy) validate() error {
	seen := map[string]struct{}{}
	cleaned := make([]string, 0, len(h.Keywords))
	for _, raw := range h.Keywords {
		keyword := strings.TrimSpace(raw)
		if keyword == "" || len(keyword) > 64 || strings.ContainsAny(keyword, "\n\r\t") {
			return fmt.Errorf("invalid keyword %q", raw)
		}
		if _, exists := seen[keyword]; exists {
			return fmt.Errorf("duplicate keyword %q", keyword)
		}
		seen[keyword] = struct{}{}
		cleaned = append(cleaned, keyword)
	}
	if len(cleaned) > 64 {
		return errors.New("at most 64 keywords")
	}
	h.Keywords = cleaned
	return nil
}

func (l *Limits) validate() error {
	positive := []struct {
		name  string
		value int
	}{
		{"min_ms", l.MinMS}, {"max_seconds", l.MaxSeconds}, {"silence_stop_seconds", l.SilenceStopSeconds},
		{"final_deadline_seconds", l.FinalDeadlineSeconds}, {"max_concurrent", l.MaxConcurrent},
		{"abandon_seconds", l.AbandonSeconds}, {"sweep_seconds", l.SweepSeconds}, {"failure_pause_count", l.FailurePauseCount},
		{"failure_pause_seconds", l.FailurePauseSeconds},
	}
	for _, field := range positive {
		if field.value <= 0 {
			return fmt.Errorf("%s must be positive", field.name)
		}
	}
	if l.MaxCaptureBytes <= 0 {
		return errors.New("max_capture_bytes must be positive")
	}
	if l.MinPeak <= 0 || l.MinPeak >= 1 || l.MinRMS <= 0 || l.MinRMS >= 1 {
		return errors.New("min_peak and min_rms must be between 0 and 1")
	}
	if l.MinConfidence < 0 || l.MinConfidence > 1 {
		return errors.New("min_confidence must be between 0 and 1")
	}
	if l.MinMS >= l.MaxSeconds*1000 {
		return errors.New("min_ms must be shorter than max_seconds")
	}
	if l.SilenceStopSeconds > l.MaxSeconds {
		return errors.New("silence_stop_seconds cannot exceed max_seconds")
	}
	if l.SweepSeconds >= l.AbandonSeconds {
		return errors.New("sweep_seconds must be shorter than abandon_seconds")
	}
	return nil
}

func (s *Streaming) validate() error {
	positive := []struct {
		name  string
		value int
	}{
		{"frame_interval_ms", s.FrameIntervalMS}, {"partial_min_interval_ms", s.PartialMinIntervalMS},
		{"window_seconds", s.WindowSeconds}, {"overlap_seconds", s.OverlapSeconds},
		{"keepalive_seconds", s.KeepaliveSeconds}, {"max_streams", s.MaxStreams},
	}
	for _, field := range positive {
		if field.value <= 0 {
			return fmt.Errorf("%s must be positive", field.name)
		}
	}
	if s.MaxFrameBytes <= 0 {
		return errors.New("max_frame_bytes must be positive")
	}
	if s.OverlapSeconds >= s.WindowSeconds {
		return errors.New("overlap_seconds must be shorter than window_seconds")
	}
	return nil
}

func (u *UI) validate() error {
	u.PushToTalk = strings.TrimSpace(u.PushToTalk)
	if u.PushToTalk == "" || len(u.PushToTalk) > 32 {
		return errors.New("push_to_talk is required")
	}
	if u.HoldThresholdMS <= 0 || u.AutoSendMinWords <= 0 || u.DimTailWords <= 0 || u.TintSeconds <= 0 || u.FirstRunHintSessions < 0 {
		return errors.New("hold_threshold_ms, auto_send_min_words, dim_tail_words, tint_seconds must be positive and first_run_hint_sessions non-negative")
	}
	return nil
}
