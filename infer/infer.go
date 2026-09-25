// Package infer owns programmatic model transport. It deliberately knows nothing
// about orchestration profiles, governance, approvals, sessions, or runtime tasks.
package infer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	LocalOllamaPath = "local-ollama-v1"
	maxMessages     = 8
	maxSchemaBytes  = 64 * 1024
)

type ErrorKind string

const (
	ErrorInvalid     ErrorKind = "invalid_request"
	ErrorUnavailable ErrorKind = "unavailable"
	ErrorTimeout     ErrorKind = "timed_out"
	ErrorMalformed   ErrorKind = "malformed_response"
)

type Error struct {
	Kind  ErrorKind
	cause error
}

func (e *Error) Error() string { return string(e.Kind) }
func (e *Error) Unwrap() error { return e.cause }

func Kind(err error) ErrorKind {
	var target *Error
	if errors.As(err, &target) {
		return target.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrorTimeout
	}
	return ErrorUnavailable
}

// RequestPath is the immutable ADR-0012 model-request path for one call. Digest
// covers every field and the effective request ceilings; fallback is always explicit.
type RequestPath struct {
	Kind                 string `json:"kind"`
	Backend              string `json:"backend"`
	IntegrationSurface   string `json:"integration_surface"`
	ExecutionHost        string `json:"execution_host"`
	Provider             string `json:"provider"`
	AuthOwner            string `json:"auth_owner"`
	AuthMode             string `json:"auth_mode"`
	BillingMode          string `json:"billing_mode"`
	PromptSender         string `json:"prompt_sender"`
	CredentialVisibility string `json:"credential_visibility"`
	DataDestination      string `json:"data_destination"`
	FallbackPolicy       string `json:"fallback_policy"`
	PolicyRevision       string `json:"policy_revision"`
	Endpoint             string `json:"endpoint"`
	Model                string `json:"model"`
	MaxInputBytes        int    `json:"max_input_bytes"`
	MaxOutputBytes       int    `json:"max_output_bytes"`
	MaxTokens            int    `json:"max_tokens"`
	Digest               string `json:"digest"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Path     RequestPath
	Messages []Message
	Schema   json.RawMessage
}

type Response struct {
	Content          []byte
	Model            string
	RequestBytes     int
	ResponseBytes    int
	PromptTokens     int
	CompletionTokens int
}

type Backend interface {
	Name() string
	Run(context.Context, Request) (Response, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Backend{}
)

func Register(backend Backend) {
	if backend == nil || !validIdentifier(backend.Name()) {
		panic("infer backend registration requires a bounded identifier and backend")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[backend.Name()]; exists {
		panic("duplicate infer backend registration: " + backend.Name())
	}
	registry[backend.Name()] = backend
}

func Lookup(name string) (Backend, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	backend, ok := registry[name]
	return backend, ok
}

func NewLocalOllamaPath(endpoint, model string, maxInputBytes, maxOutputBytes, maxTokens int) (RequestPath, error) {
	canonical, err := CanonicalLoopbackEndpoint(endpoint)
	if err != nil {
		return RequestPath{}, err
	}
	if !validModel(model) {
		return RequestPath{}, invalid()
	}
	if maxInputBytes < 1 || maxInputBytes > 1<<20 || maxOutputBytes < 1 || maxOutputBytes > 256<<10 ||
		maxTokens < 1 || maxTokens > 65536 {
		return RequestPath{}, invalid()
	}
	path := RequestPath{
		Kind: LocalOllamaPath, Backend: "ollama", IntegrationSurface: "ollama-native-http",
		ExecutionHost: "user-local-device", Provider: "ollama", AuthOwner: "none", AuthMode: "none",
		BillingMode: "local-compute", PromptSender: "crossing-guard-daemon",
		CredentialVisibility: "none", DataDestination: "literal-loopback",
		FallbackPolicy: "disabled", PolicyRevision: "report-only-review-v1",
		Endpoint: canonical, Model: model, MaxInputBytes: maxInputBytes,
		MaxOutputBytes: maxOutputBytes, MaxTokens: maxTokens,
	}
	encoded, err := json.Marshal(path)
	if err != nil {
		return RequestPath{}, &Error{Kind: ErrorInvalid, cause: err}
	}
	sum := sha256.Sum256(append([]byte("crossing-guard-inference-request-path-v1\x00"), encoded...))
	path.Digest = "sha256-v1:" + hex.EncodeToString(sum[:])
	return path, nil
}

func CanonicalLoopbackEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", invalid()
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return "", invalid()
	}
	port := u.Port()
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return "", invalid()
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func ValidateRequest(request Request, backendName string) error {
	if request.Path.Backend != backendName || request.Path.Kind != LocalOllamaPath || request.Path.Digest == "" {
		return invalid()
	}
	rebuilt, err := NewLocalOllamaPath(request.Path.Endpoint, request.Path.Model,
		request.Path.MaxInputBytes, request.Path.MaxOutputBytes, request.Path.MaxTokens)
	if err != nil || rebuilt != request.Path {
		return invalid()
	}
	if len(request.Messages) < 1 || len(request.Messages) > maxMessages || len(request.Schema) == 0 ||
		len(request.Schema) > maxSchemaBytes || !json.Valid(request.Schema) {
		return invalid()
	}
	var schema any
	if json.Unmarshal(request.Schema, &schema) != nil {
		return invalid()
	}
	if _, ok := schema.(map[string]any); !ok {
		return invalid()
	}
	total := 0
	for _, message := range request.Messages {
		if message.Role != "system" && message.Role != "user" || !utf8.ValidString(message.Content) {
			return invalid()
		}
		total += len(message.Content)
	}
	if total < 1 || total > request.Path.MaxInputBytes {
		return invalid()
	}
	return nil
}

func invalid() error { return &Error{Kind: ErrorInvalid} }

func validIdentifier(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (index > 0 && (r == '-' || r == '_')) {
			continue
		}
		return false
	}
	return true
}

func validModel(value string) bool {
	if len(value) < 1 || len(value) > 128 || strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func NewError(kind ErrorKind, cause error) error {
	if kind == "" {
		kind = ErrorUnavailable
	}
	return &Error{Kind: kind, cause: cause}
}

func RequireBackend(name string) (Backend, error) {
	backend, ok := Lookup(name)
	if !ok {
		return nil, &Error{Kind: ErrorUnavailable, cause: fmt.Errorf("backend unavailable")}
	}
	return backend, nil
}
