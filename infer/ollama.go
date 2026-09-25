package infer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

type ollamaBackend struct{}

func init() { Register(ollamaBackend{}) }

func (ollamaBackend) Name() string { return "ollama" }

func (ollamaBackend) Run(ctx context.Context, request Request) (result Response, runErr error) {
	if err := ValidateRequest(request, "ollama"); err != nil {
		return Response{}, err
	}
	body := struct {
		Model    string          `json:"model"`
		Messages []Message       `json:"messages"`
		Stream   bool            `json:"stream"`
		Format   json.RawMessage `json:"format"`
		Options  struct {
			Temperature int `json:"temperature"`
			NumPredict  int `json:"num_predict"`
		} `json:"options"`
	}{Model: request.Path.Model, Messages: request.Messages, Stream: false, Format: request.Schema}
	body.Options.NumPredict = request.Path.MaxTokens
	encoded, err := json.Marshal(body)
	if err != nil {
		return Response{}, NewError(ErrorInvalid, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		request.Path.Endpoint+"/api/chat", bytes.NewReader(encoded))
	if err != nil {
		return Response{}, NewError(ErrorInvalid, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil, DialContext: (&netDialer).DialContext,
		ForceAttemptHTTP2: false, DisableCompression: true, ResponseHeaderTimeout: 30 * time.Second}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirect refused")
	}}
	response, err := client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Response{}, NewError(ErrorTimeout, err)
		}
		return Response{}, NewError(ErrorUnavailable, err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && runErr == nil {
			result = Response{}
			runErr = NewError(ErrorUnavailable, errors.New("backend response close failed"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Response{}, NewError(ErrorUnavailable, errors.New("backend HTTP status"))
	}
	limit := int64(request.Path.MaxOutputBytes + 128*1024)
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return Response{}, NewError(ErrorUnavailable, err)
	}
	if int64(len(raw)) > limit {
		return Response{}, NewError(ErrorMalformed, errors.New("backend response exceeded limit"))
	}
	var wire struct {
		Model   string `json:"model"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.Message.Content == "" ||
		len(wire.Message.Content) > request.Path.MaxOutputBytes {
		return Response{}, NewError(ErrorMalformed, errors.New("backend response was malformed"))
	}
	return Response{Content: []byte(wire.Message.Content), Model: wire.Model,
		RequestBytes: len(encoded), ResponseBytes: len(raw), PromptTokens: wire.PromptEvalCount,
		CompletionTokens: wire.EvalCount}, nil
}

// netDialer is a value rather than http.DefaultTransport so proxy and redirect
// behavior cannot be inherited from process-wide configuration.
var netDialer = net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
