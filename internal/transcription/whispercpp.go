package transcription

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const backendWhisperCPP = "whispercpp"

func init() {
	Register(backendWhisperCPP, newWhisperCPP)
}

// whisperCPPSettings are the adapter-specific keys of the backend section.
type whisperCPPSettings struct {
	Executable  string `json:"executable"`
	ModelPath   string `json:"model_path"`
	ModelSHA256 string `json:"model_sha256"`
	Language    string `json:"language"`
	CPUOnly     bool   `json:"cpu_only"`
	Sandbox     string `json:"sandbox"`
}

var (
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	languagePattern = regexp.MustCompile(`^[a-z]{2,3}$`)
)

func (w *whisperCPPSettings) validate() error {
	w.Executable = strings.TrimSpace(w.Executable)
	w.ModelPath = strings.TrimSpace(w.ModelPath)
	w.ModelSHA256 = strings.ToLower(strings.TrimSpace(w.ModelSHA256))
	w.Language = strings.ToLower(strings.TrimSpace(w.Language))
	w.Sandbox = strings.ToLower(strings.TrimSpace(w.Sandbox))
	if !filepath.IsAbs(w.Executable) {
		return errors.New("executable must be an absolute path")
	}
	if !filepath.IsAbs(w.ModelPath) {
		return errors.New("model_path must be an absolute path")
	}
	if !sha256Pattern.MatchString(w.ModelSHA256) {
		return errors.New("model_sha256 must be 64 lowercase hex characters")
	}
	if !languagePattern.MatchString(w.Language) {
		return errors.New("language must be a two- or three-letter code")
	}
	if w.Sandbox != "strict" {
		return errors.New(`sandbox must be "strict"`)
	}
	if !w.CPUOnly {
		// The sandbox never grants the GPU path: granting it made whisper-cli
		// hang in the 2026-09-01 probe. A GPU run would hang to the deadline.
		return errors.New("cpu_only must be true; the sandbox denies GPU access")
	}
	return nil
}

// newWhisperCPP decodes the section, validates it, and performs the readiness
// checks that need the filesystem: the executable exists, the model checksum
// matches, and the platform sandbox is available.
func newWhisperCPP(config Config) (Backend, error) {
	section, ok := config.Section(backendWhisperCPP)
	if !ok {
		return nil, errors.New("whispercpp section is required")
	}
	var settings whisperCPPSettings
	if err := section.DecodeSettings(&settings); err != nil {
		return nil, fmt.Errorf("whispercpp: %w", err)
	}
	if err := settings.validate(); err != nil {
		return nil, fmt.Errorf("whispercpp: %w", err)
	}
	if info, err := os.Stat(settings.Executable); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("executable %s is not an executable file", settings.Executable)
	}
	digest, err := fileSHA256(settings.ModelPath)
	switch {
	case err != nil:
		return nil, fmt.Errorf("model file %s is not readable: %v", settings.ModelPath, err)
	case digest != settings.ModelSHA256:
		return nil, fmt.Errorf("model file %s checksum differs from model_sha256", settings.ModelPath)
	}
	sandbox := NewStrictSandbox()
	if ok, reason := sandbox.Available(); !ok {
		return nil, errors.New(reason)
	}
	return &whisperCPP{settings: settings, sandbox: sandbox}, nil
}

// whisperCPP is the local backend: one sandboxed whisper-cli process per
// decode, hints only in the prompt, JSON-full output parsed for words and
// token probabilities.
type whisperCPP struct {
	settings whisperCPPSettings
	sandbox  Sandbox
}

const whisperSampleRate = 16000

func (w *whisperCPP) Name() string { return backendWhisperCPP }

func (w *whisperCPP) Capabilities() Capabilities {
	return Capabilities{Backend: backendWhisperCPP, DisplayName: "Local (whisper.cpp)", EmitsPartials: true,
		ReportsConfidence: true, RequiresDisclosure: false, PreferredSampleRate: whisperSampleRate,
		Facts: []Fact{
			{Label: "Model file", Value: w.settings.ModelPath, Note: "checksum verified at start"},
			{Label: "Runs", Value: "locally, CPU only", Note: "in a " + w.settings.Sandbox + " sandbox that denies network access"},
		}}
}

// Decode runs whisper-cli inside the sandbox. Dictated text never appears in
// argv: the prompt carries hints only. Errors carry bounded stderr as log
// detail, never as the user-facing message.
func (w *whisperCPP) Decode(ctx context.Context, request DecodeRequest, _ PartialFunc) (Decoded, error) {
	outputPrefix := filepath.Join(request.WorkDir, "decode")
	args := []string{"-m", w.settings.ModelPath, "-f", request.WAVPath, "-l", w.settings.Language,
		"-np", "--output-json-full", "-of", outputPrefix}
	if w.settings.CPUOnly {
		args = append(args, "--no-gpu")
	}
	if prompt := hintPrompt(request.Hints); prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	cmd, err := w.sandbox.Command(ctx, SandboxSpec{Executable: w.settings.Executable, Args: args,
		ReadPaths: []string{w.settings.ModelPath, request.WorkDir}, WritePaths: []string{request.WorkDir}, Dir: request.WorkDir})
	if err != nil {
		return Decoded{}, FailWithDetail(EndEngineError, "dictation failed", err.Error())
	}
	stdout := &boundedBuffer{limit: request.CaptureLimit}
	stderr := &boundedBuffer{limit: request.CaptureLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Decoded{}, ctx.Err()
		}
		return Decoded{}, FailWithDetail(EndEngineError, "dictation failed",
			fmt.Sprintf("whisper-cli exit: %v; stderr: %s", err, stderr.String()))
	}
	raw, err := os.ReadFile(outputPrefix + ".json")
	if err != nil {
		return Decoded{}, FailWithDetail(EndEngineError, "dictation failed", "whisper-cli produced no JSON output")
	}
	decoded, err := parseWhisperJSON(raw)
	if err != nil {
		return Decoded{}, FailWithDetail(EndEngineError, "dictation failed", "whisper-cli JSON: "+err.Error())
	}
	return decoded, nil
}

// hintPrompt renders hints as a short initial prompt. It never includes any
// dictated text.
func hintPrompt(hints Hints) string {
	var parts []string
	if hints.ProjectName != "" {
		parts = append(parts, "Project: "+hints.ProjectName+".")
	}
	if hints.BranchName != "" {
		parts = append(parts, "Branch: "+hints.BranchName+".")
	}
	if len(hints.Keywords) > 0 {
		parts = append(parts, "Terms: "+strings.Join(hints.Keywords, ", ")+".")
	}
	return strings.Join(parts, " ")
}

type whisperJSON struct {
	Transcription []struct {
		Text   string `json:"text"`
		Tokens []struct {
			Text    string  `json:"text"`
			P       float64 `json:"p"`
			Offsets struct {
				From int64 `json:"from"`
				To   int64 `json:"to"`
			} `json:"offsets"`
		} `json:"tokens"`
	} `json:"transcription"`
}

// parseWhisperJSON turns whisper-cli's full JSON into words and a mean
// content-token probability. Control tokens are skipped; a token whose text
// starts with a space begins a new word.
func parseWhisperJSON(raw []byte) (Decoded, error) {
	var parsed whisperJSON
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Decoded{}, err
	}
	var (
		words   []Word
		current *Word
		sumP    float64
		count   int
		text    strings.Builder
	)
	for _, segment := range parsed.Transcription {
		text.WriteString(segment.Text)
		for _, token := range segment.Tokens {
			if isSpecialToken(token.Text) || strings.TrimSpace(token.Text) == "" {
				continue
			}
			sumP += token.P
			count++
			if strings.HasPrefix(token.Text, " ") || current == nil {
				words = append(words, Word{Text: strings.TrimSpace(token.Text), StartMS: token.Offsets.From, EndMS: token.Offsets.To})
				current = &words[len(words)-1]
				continue
			}
			current.Text += token.Text
			current.EndMS = token.Offsets.To
		}
	}
	decoded := Decoded{Text: strings.TrimSpace(specialTokenPattern.ReplaceAllString(text.String(), "")), Words: words}
	if count > 0 {
		decoded.Confidence = sumP / float64(count)
		decoded.HasConfidence = true
	}
	return decoded, nil
}

// specialTokenPattern matches whisper control tokens such as [_BEG_], [_TT_75]
// and <|endoftext|>, which are never dictated words.
var specialTokenPattern = regexp.MustCompile(`\[_[A-Za-z0-9_]+\]|<\|[^|]*\|>`)

func isSpecialToken(text string) bool {
	return specialTokenPattern.MatchString(strings.TrimSpace(text))
}

func fileSHA256(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = handle.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// boundedBuffer keeps at most limit bytes of process output.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - int64(b.buf.Len())
	if remaining > 0 {
		if int64(len(p)) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
