package daemon

// OpenCode's run client rejects interactive permissions. This protocol uses the
// server API instead; task_process still owns the one server process and pipes.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"crossing-guard/internal/guardcli"
)

const openCodeHTTPBound = 16 << 20

type openCodeServerProtocol struct {
	request      ChatRequest
	launch       ChatLaunchContext
	password     string
	timeout      time.Duration
	client       *http.Client
	endpoint     string
	approve      func(context.Context, guardcli.RuntimeApprovalRequest) (guardcli.RuntimeApprovalResult, error)
	permissionMu sync.Mutex
	grantTokens  map[string]string
}

func (openCodeChatDriver) ProcessProtocol(req ChatRequest, launch ChatLaunchContext, cmd *exec.Cmd) chatProcessProtocol {
	password := ""
	for _, entry := range cmd.Env {
		if value, ok := strings.CutPrefix(entry, "OPENCODE_SERVER_PASSWORD="); ok {
			password = value
		}
	}
	return &openCodeServerProtocol{request: req, launch: launch, password: password,
		timeout: activeApprovalsConfig().runtimeToolTimeout(),
		client: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OpenCode redirects are refused") }},
		approve: func(ctx context.Context, request guardcli.RuntimeApprovalRequest) (guardcli.RuntimeApprovalResult, error) {
			return guardcli.RequestRuntimeApprovalFromDataDir(ctx, launch.DataDir, request)
		},
	}
}

func (s *openCodeServerProtocol) Run(stdout io.ReadCloser, emit func(ChatEvent)) error {
	defer s.releaseRuntimeGrants()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.client.CloseIdleConnections()
	endpoints := make(chan string, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer cancel() // server death also closes held approvals and the SSE request
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		found := false
		for scanner.Scan() {
			endpoint, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "opencode server listening on ")
			if ok && !found {
				endpoints <- endpoint
				found = true
			}
		}
	}()
	defer func() { _ = stdout.Close(); <-drained }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case s.endpoint = <-endpoints:
	case <-ctx.Done():
		return errors.New("OpenCode server exited before becoming ready")
	case <-timer.C:
		return errors.New("OpenCode server startup timed out")
	}
	if !validOpenCodeEndpoint(s.endpoint) || s.password == "" {
		return errors.New("OpenCode returned an invalid private server endpoint")
	}
	return s.converse(ctx, emit)
}

func (s *openCodeServerProtocol) releaseRuntimeGrants() {
	seen := map[string]bool{}
	for _, token := range s.grantTokens {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = guardcli.ReleaseRuntimeApprovalGrantFromDataDir(ctx, s.launch.DataDir, token)
		cancel()
	}
	s.grantTokens = nil
}

func validOpenCodeEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	port, err := net.LookupPort("tcp", u.Port())
	return err == nil && port > 0 && port <= 65535
}

func (s *openCodeServerProtocol) httpRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+path+"?directory="+url.QueryEscape(s.request.Cwd), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("opencode", s.password)
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}

func (s *openCodeServerProtocol) call(ctx context.Context, method, path string, body, out any) error {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := s.httpRequest(bounded, method, path, body)
	if err != nil {
		return fmt.Errorf("OpenCode %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, openCodeHTTPBound+1))
	if err != nil || len(data) > openCodeHTTPBound {
		return fmt.Errorf("OpenCode %s response unreadable or too large", path)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("OpenCode %s returned HTTP %d", path, response.StatusCode)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return fmt.Errorf("OpenCode %s returned invalid JSON", path)
	}
	return nil
}

func (s *openCodeServerProtocol) prepareSession(ctx context.Context) (string, error) {
	var health struct {
		Healthy bool
		Version string
	}
	if err := s.call(ctx, "GET", "/global/health", nil, &health); err != nil {
		return "", err
	}
	if !health.Healthy || health.Version != "1.18.0" {
		return "", fmt.Errorf("OpenCode GUI approvals require verified version 1.18.0; server reports %q", health.Version)
	}
	if s.request.Mode != "" {
		var agents []struct{ Name string }
		if err := s.call(ctx, "GET", "/agent", nil, &agents); err != nil {
			return "", err
		}
		found := false
		for _, agent := range agents {
			if agent.Name == s.request.Mode {
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("OpenCode agent %q is unavailable; refusing fallback", s.request.Mode)
		}
	}
	var session struct {
		ID        string
		Directory string
	}
	if s.request.SessionID != "" {
		if err := s.call(ctx, "GET", "/session/"+s.request.SessionID, nil, &session); err != nil {
			return "", err
		}
		want, err := os.Stat(s.request.Cwd)
		actual, actualErr := os.Stat(session.Directory)
		if err != nil || actualErr != nil || !os.SameFile(want, actual) || session.ID != s.request.SessionID {
			return "", errors.New("OpenCode session does not match the selected working directory and session id")
		}
	} else {
		permission := []map[string]string{}
		for _, name := range []string{"question", "plan_enter", "plan_exit"} {
			permission = append(permission, map[string]string{"permission": name, "pattern": "*", "action": "deny"})
		}
		if err := s.call(ctx, "POST", "/session", map[string]any{"permission": permission}, &session); err != nil {
			return "", err
		}
	}
	if !openCodeWireID(session.ID, "ses_") {
		return "", errors.New("OpenCode returned an invalid session id")
	}
	return session.ID, nil
}

func (s *openCodeServerProtocol) promptBody() map[string]any {
	parts := []map[string]any{}
	for _, input := range s.launch.Inputs {
		mime := input.MediaType
		if mime == "" {
			mime = "text/plain"
		}
		parts = append(parts, map[string]any{"type": "file", "url": (&url.URL{Scheme: "file", Path: input.Path}).String(), "mime": mime, "filename": input.Name})
	}
	parts = append(parts, map[string]any{"type": "text", "text": s.request.Prompt})
	body := map[string]any{"parts": parts}
	if effort := s.request.ThinkingEffort; effort != nil && effort.Kind == "level" {
		body["variant"] = effort.Value
	}
	if s.request.Model != "" {
		provider, model, _ := strings.Cut(s.request.Model, "/")
		body["model"] = map[string]string{"providerID": provider, "modelID": model}
	}
	if s.request.Mode != "" {
		body["agent"] = s.request.Mode
	}
	return body
}

func (s *openCodeServerProtocol) converse(parent context.Context, emit func(ChatEvent)) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if err := s.validateEffort(ctx); err != nil {
		return err
	}
	sessionID, err := s.prepareSession(ctx)
	if err != nil {
		return err
	}
	emit(ChatEvent{"type": "session", "id": sessionID})
	response, err := s.httpRequest(ctx, "GET", "/event", nil)
	if err != nil {
		return fmt.Errorf("OpenCode event subscription: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return errors.New("OpenCode event subscription was refused")
	}
	frames := make(chan openCodeServerEvent, 32)
	streamDone := make(chan error, 1)
	go func() { streamDone <- readOpenCodeEvents(ctx, response.Body, frames) }()
	// POST only after the stream is subscribed. An ambiguous failure is never retried.
	if err := s.call(ctx, "POST", "/session/"+sessionID+"/prompt_async", s.promptBody(), nil); err != nil {
		return err
	}
	state := newOpenCodeEventState(sessionID)
	decisions := make(chan error, 16)
	var pending sync.WaitGroup
	defer func() { cancel(); pending.Wait() }()
	outstanding := 0
	for {
		select {
		case <-ctx.Done():
			return errors.New("OpenCode server or task stopped before the turn completed")
		case err := <-decisions:
			outstanding--
			if err != nil {
				return err
			}
		case event, open := <-frames:
			if !open {
				return fmt.Errorf("OpenCode event stream ended before turn completion: %w", <-streamDone)
			}
			if event.Type == "permission.asked" {
				permission, err := state.permission(event.Properties)
				if err != nil {
					return err
				}
				if permission == nil {
					continue
				}
				if outstanding >= 16 {
					return errors.New("OpenCode exceeded the pending permission limit")
				}
				outstanding++
				pending.Add(1)
				go func() {
					defer pending.Done()
					result := s.decidePermission(ctx, sessionID, *permission)
					select {
					case decisions <- result:
					case <-ctx.Done():
					}
				}()
				continue
			}
			done, err := state.project(event, emit)
			if err != nil {
				return err
			}
			if done {
				// A permission handler can still be returning when idle arrives. Read all
				// outcomes before declaring success, especially the denial that caused idle.
				for outstanding > 0 {
					select {
					case err := <-decisions:
						outstanding--
						if err != nil {
							return err
						}
					case <-ctx.Done():
						return errors.New("OpenCode stopped while an approval was settling")
					}
				}
				return nil
			}
		}
	}
}

func (s *openCodeServerProtocol) decidePermission(ctx context.Context, sessionID string, permission openCodePermission) error {
	s.permissionMu.Lock()
	defer s.permissionMu.Unlock()
	summary, _ := json.Marshal(map[string]any{"permission": permission.Permission, "patterns": permission.Patterns, "metadata": permission.Metadata})
	if len(summary) > 7<<10 {
		return errors.New("OpenCode permission details exceed the approval display limit; request stopped")
	}
	keyBytes, _ := json.Marshal([]any{permission.Permission, permission.Patterns})
	key := string(keyBytes)
	if s.grantTokens == nil {
		s.grantTokens = map[string]string{}
	}
	request := guardcli.RuntimeApprovalRequest{Runtime: "opencode", TaskID: s.launch.TaskID,
		CatalogSessionID: s.request.CatalogSessionID, NativeSessionID: sessionID, ToolCallID: permission.ID,
		ToolName: permission.Permission, Summary: string(summary), Timeout: s.timeout,
		Action: openCodePermissionAction(permission.Permission), Targets: append([]string(nil), permission.Patterns...),
		ApprovalReason:     "OpenCode requires approval for this permission before it can continue.",
		OfferExactRunGrant: true, GrantToken: s.grantTokens[key]}
	result, requestErr := s.approve(ctx, request)
	if requestErr != nil && request.GrantToken != "" {
		delete(s.grantTokens, key)
		request.GrantToken = ""
		result, requestErr = s.approve(ctx, request)
	}
	reply := "reject"
	var outcome error
	if requestErr != nil {
		outcome = errors.New("OpenCode approval service unavailable; request denied. Restore the service and continue the session")
	} else if result.Decision == "allowed" {
		reply = "once"
		if result.GrantID == approvalGrantRunExact {
			if result.GrantToken == "" {
				return errors.New("OpenCode exact-run approval returned no capability; request stopped")
			}
			s.grantTokens[key] = result.GrantToken
		}
	} else if result.Decision == "denied" {
		outcome = errors.New("OpenCode permission denied in Crossing Guard; turn stopped. Continue the session when ready")
	} else if result.Decision == "expired" {
		outcome = errors.New("OpenCode approval expired; turn stopped. Continue the session to request approval again")
	} else {
		outcome = errors.New("OpenCode approval returned an invalid decision; turn stopped")
	}
	if err := s.call(ctx, "POST", "/permission/"+permission.ID+"/reply", map[string]string{"reply": reply}, nil); err != nil {
		return fmt.Errorf("OpenCode permission reply failed: %w", err)
	}
	return outcome
}

func openCodePermissionAction(permission string) string {
	switch permission {
	case "external_directory":
		return "Access paths outside the session working folder"
	case "bash":
		return "Run a shell command"
	case "edit":
		return "Modify files"
	case "read":
		return "Read files"
	case "glob":
		return "List files matching a pattern"
	case "grep":
		return "Search file contents"
	case "task":
		return "Start a subagent"
	case "webfetch", "websearch":
		return "Access the web"
	default:
		return "Use the “" + permission + "” permission"
	}
}
