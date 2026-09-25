package transcription

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const backendOpenAIBatch = "openai-batch"

func init() {
	Register(backendOpenAIBatch, newOpenAIBatch)
}

// openAIBatchSettings are the adapter-specific keys of the backend section. The
// key is named by environment variable and read only when a decode runs.
type openAIBatchSettings struct {
	APIKeyEnv string `json:"api_key_env"`
	Model     string `json:"model"`
	Origin    string `json:"origin"`
}

var envNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

func (o *openAIBatchSettings) validate() error {
	o.APIKeyEnv = strings.TrimSpace(o.APIKeyEnv)
	o.Model = strings.TrimSpace(o.Model)
	o.Origin = strings.TrimSpace(o.Origin)
	if !envNamePattern.MatchString(o.APIKeyEnv) {
		return errors.New("api_key_env must name an environment variable")
	}
	if o.Model == "" || strings.ContainsAny(o.Model, " \t\n/") {
		return errors.New("model must be a bare model identifier")
	}
	origin, err := url.Parse(o.Origin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" {
		return errors.New("origin must be an https origin with no path")
	}
	return nil
}

// newOpenAIBatch decodes and validates the section. A disclosure is mandatory
// because audio leaves the machine.
func newOpenAIBatch(config Config) (Backend, error) {
	section, ok := config.Section(backendOpenAIBatch)
	if !ok {
		return nil, errors.New("openai-batch section is required")
	}
	var settings openAIBatchSettings
	if err := section.DecodeSettings(&settings); err != nil {
		return nil, fmt.Errorf("openai-batch: %w", err)
	}
	if err := settings.validate(); err != nil {
		return nil, fmt.Errorf("openai-batch: %w", err)
	}
	if section.Disclosure == nil {
		return nil, errors.New("openai-batch: a disclosure is required because audio leaves the machine")
	}
	return &openAIBatch{settings: settings, client: &http.Client{}}, nil
}

// openAIBatch uploads the finished clip to the transcription endpoint with
// streaming enabled, so text arrives progressively after release. It emits no
// partials while listening and never reads any coding runtime's login.
type openAIBatch struct {
	settings openAIBatchSettings
	client   *http.Client
}

const (
	openAISampleRate       = 16000
	openAITranscriptionAPI = "/v1/audio/transcriptions"
	openAIResponseLimit    = 1 << 20
)

func (o *openAIBatch) Name() string { return backendOpenAIBatch }

func (o *openAIBatch) Capabilities() Capabilities {
	return Capabilities{Backend: backendOpenAIBatch, DisplayName: "OpenAI (after release)", EmitsPartials: false,
		ReportsConfidence: false, RequiresDisclosure: true, PreferredSampleRate: openAISampleRate,
		Facts: []Fact{
			{Label: "Model", Value: o.settings.Model, Note: "text arrives after you let go"},
			{Label: "Key", Value: "environment variable " + o.settings.APIKeyEnv, Note: "never a coding runtime's login"},
		}}
}

// Decode performs one attempt. The API key is read from the configured
// environment variable at call time and never appears in any error.
func (o *openAIBatch) Decode(ctx context.Context, request DecodeRequest, onPartial PartialFunc) (Decoded, error) {
	if request.Window {
		return Decoded{}, Fail(EndEngineError, "this backend does not decode windows")
	}
	key := strings.TrimSpace(os.Getenv(o.settings.APIKeyEnv))
	if key == "" {
		return Decoded{}, Fail(EndProviderError, "the key named by %s is not set", o.settings.APIKeyEnv)
	}
	body, contentType, err := o.multipart(request)
	if err != nil {
		return Decoded{}, FailWithDetail(EndEngineError, "dictation failed", err.Error())
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, o.settings.Origin+openAITranscriptionAPI, body)
	if err != nil {
		return Decoded{}, FailWithDetail(EndEngineError, "dictation failed", err.Error())
	}
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.Header.Set("Authorization", "Bearer "+key)
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := o.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return Decoded{}, ctx.Err()
		}
		return Decoded{}, FailWithDetail(EndProviderError, "connection to OpenAI lost", redact(err.Error(), key))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return Decoded{}, FailWithDetail(EndProviderError, providerMessage(response.StatusCode),
			fmt.Sprintf("status %d request %s: %s", response.StatusCode, response.Header.Get("x-request-id"), redact(string(snippet), key)))
	}
	text, err := readTranscriptStream(io.LimitReader(response.Body, openAIResponseLimit), onPartial)
	if err != nil {
		if ctx.Err() != nil {
			return Decoded{}, ctx.Err()
		}
		return Decoded{}, FailWithDetail(EndProviderError, "connection to OpenAI lost", redact(err.Error(), key))
	}
	return Decoded{Text: strings.TrimSpace(text)}, nil
}

func (o *openAIBatch) multipart(request DecodeRequest) (io.Reader, string, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	part, err := writer.CreateFormFile("file", "dictation.wav")
	if err != nil {
		return nil, "", err
	}
	clip, err := os.Open(request.WAVPath)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = clip.Close() }()
	if _, err := io.Copy(part, clip); err != nil {
		return nil, "", err
	}
	fields := map[string]string{"model": o.settings.Model, "stream": "true", "response_format": "json"}
	if prompt := hintPrompt(request.Hints); prompt != "" {
		fields["prompt"] = prompt
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &buffer, writer.FormDataContentType(), nil
}

// readTranscriptStream consumes the endpoint's server-sent events. Each
// transcript.text.delta carries new text; transcript.text.done carries the
// whole text and ends the stream.
func readTranscriptStream(reader io.Reader, onPartial PartialFunc) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 256<<10)
	var accumulated strings.Builder
	var done string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch event.Type {
		case "transcript.text.delta":
			accumulated.WriteString(event.Delta)
			if onPartial != nil {
				onPartial(accumulated.String())
			}
		case "transcript.text.done":
			done = event.Text
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if done != "" {
		return done, nil
	}
	if accumulated.Len() == 0 {
		return "", errors.New("transcription stream ended without text")
	}
	return accumulated.String(), nil
}

func providerMessage(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "OpenAI rejected the configured key"
	case status == http.StatusTooManyRequests:
		return "OpenAI is rate limiting requests"
	case status >= 500:
		return "OpenAI is unavailable right now"
	default:
		return "OpenAI refused the request"
	}
}

func redact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[redacted]")
}
