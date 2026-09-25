package transcription

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"sync"
	"time"
)

const (
	testLocalBackend = "whispercpp"
	testCloudBackend = "openai-batch"
)

func rawSettings(values map[string]any) json.RawMessage {
	raw, _ := json.Marshal(values)
	return raw
}

func testConfig() Config {
	return Config{
		Backend: testLocalBackend,
		Backends: map[string]BackendSection{
			testLocalBackend: {
				Hints: HintPolicy{ProjectName: true, BranchName: true, Keywords: []string{"diff"}},
				Settings: rawSettings(map[string]any{"executable": "/usr/bin/true", "model_path": "/tmp/model.bin",
					"model_sha256": "4baf70dd0d7c4247ba2b81fafd9c01005ac77c2f9ef064e00dcf195d0e2fdd2f", "language": "en",
					"cpu_only": true, "sandbox": "strict"}),
			},
			testCloudBackend: {
				Hints:      HintPolicy{},
				Disclosure: &Disclosure{Text: "Your recording goes to OpenAI when you let go.", Version: 1},
				Settings: rawSettings(map[string]any{"api_key_env": "CG_TEST_OPENAI_KEY", "model": "gpt-transcribe",
					"origin": "https://api.openai.com"}),
			},
		},
		Disclosure: DisclosureLimits{TokenSeconds: 120},
		Audio:      Audio{Channels: 1, Bits: 16},
		Limits: Limits{MinMS: 250, MaxSeconds: 120, SilenceStopSeconds: 15, MinPeak: 0.01, MinRMS: 0.003,
			MinConfidence: 0.2, FinalDeadlineSeconds: 30, MaxConcurrent: 1, AbandonSeconds: 20, SweepSeconds: 5,
			FailurePauseCount: 3, FailurePauseSeconds: 10, MaxCaptureBytes: 1 << 20},
		Streaming: Streaming{FrameIntervalMS: 250, MaxFrameBytes: 65536, PartialMinIntervalMS: 700,
			WindowSeconds: 8, OverlapSeconds: 2, KeepaliveSeconds: 15, MaxStreams: 16},
		UI: UI{PushToTalk: "Alt+Space", HoldThresholdMS: 300, AutoSend: false, AutoSendMinWords: 3,
			DimTailWords: 2, TintSeconds: 4, FirstRunHintSessions: 3},
	}
}

// tonePCM produces mono 16-bit samples of a 440 Hz tone at the given amplitude.
func tonePCM(sampleRate int, ms int, amplitude float64) []byte {
	samples := sampleRate * ms / 1000
	out := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		value := amplitude * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(value*32767)))
	}
	return out
}

func silencePCM(sampleRate int, ms int) []byte { return make([]byte, sampleRate*ms/1000*2) }

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeHints struct{ hints Hints }

func (f fakeHints) Hints(context.Context, HintRequest) (Hints, error) { return f.hints, nil }

// fakeBackend records every decode request and answers from a script.
type fakeBackend struct {
	caps     Capabilities
	mu       sync.Mutex
	requests []DecodeRequest
	window   func(n int) (Decoded, error)
	final    func(onPartial PartialFunc) (Decoded, error)
	block    chan struct{}
	// ignoreCancel models a process that takes time to die: the decode returns
	// only when block is closed, even after its context is cancelled.
	ignoreCancel bool
}

func newFakeBackend(name string, emitsPartials, requiresDisclosure bool) *fakeBackend {
	return &fakeBackend{caps: Capabilities{Backend: name, DisplayName: "Fake", EmitsPartials: emitsPartials,
		ReportsConfidence: true, RequiresDisclosure: requiresDisclosure, PreferredSampleRate: 16000}}
}

func (f *fakeBackend) Name() string               { return f.caps.Backend }
func (f *fakeBackend) Capabilities() Capabilities { return f.caps }

func (f *fakeBackend) Decode(ctx context.Context, request DecodeRequest, onPartial PartialFunc) (Decoded, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	windows := 0
	for _, r := range f.requests {
		if r.Window {
			windows++
		}
	}
	f.mu.Unlock()
	if f.block != nil {
		if f.ignoreCancel {
			<-f.block
			if ctx.Err() != nil {
				return Decoded{}, ctx.Err()
			}
		} else {
			select {
			case <-f.block:
			case <-ctx.Done():
				return Decoded{}, ctx.Err()
			}
		}
	}
	if request.Window {
		if f.window == nil {
			return Decoded{}, nil
		}
		return f.window(windows)
	}
	if f.final == nil {
		return Decoded{Text: "final text", Confidence: 0.9, HasConfidence: true}, nil
	}
	return f.final(onPartial)
}

func (f *fakeBackend) recorded() []DecodeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DecodeRequest(nil), f.requests...)
}
