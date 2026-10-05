package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type openCodeServerEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

type openCodePermission struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Permission string         `json:"permission"`
	Patterns   []string       `json:"patterns"`
	Metadata   map[string]any `json:"metadata"`
}

type openCodeEventState struct {
	sessionID   string
	active      bool
	roles       map[string]string
	parts       map[string]bool
	permissions map[string]bool
}

func newOpenCodeEventState(sessionID string) *openCodeEventState {
	return &openCodeEventState{sessionID: sessionID, roles: map[string]string{}, parts: map[string]bool{}, permissions: map[string]bool{}}
}

func openCodeWireID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > 200 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func readOpenCodeEvents(ctx context.Context, input io.Reader, frames chan<- openCodeServerEvent) error {
	defer close(frames)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), openCodeHTTPBound)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var event openCodeServerEvent
			if err := json.Unmarshal([]byte(data.String()), &event); err != nil || event.Type == "" {
				return errors.New("invalid OpenCode event")
			}
			select {
			case frames <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
			data.Reset()
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len()+len(value)+1 > openCodeHTTPBound {
				return errors.New("OpenCode event exceeds size limit")
			}
			data.WriteString(strings.TrimPrefix(value, " "))
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (s *openCodeEventState) permission(raw json.RawMessage) (*openCodePermission, error) {
	var permission openCodePermission
	if json.Unmarshal(raw, &permission) != nil || !openCodeWireID(permission.ID, "per_") || permission.Permission == "" || len(permission.Patterns) == 0 {
		return nil, errors.New("OpenCode returned an invalid permission request")
	}
	if permission.SessionID != s.sessionID {
		return nil, errors.New("OpenCode child-session approval is not supported by this GUI transport; continue that work in OpenCode")
	}
	if s.permissions[permission.ID] {
		return nil, nil
	}
	if len(s.permissions) >= 4096 {
		return nil, errors.New("OpenCode permission history limit exceeded")
	}
	s.permissions[permission.ID] = true
	return &permission, nil
}

func (s *openCodeEventState) project(event openCodeServerEvent, emit func(ChatEvent)) (bool, error) {
	var props map[string]any
	if json.Unmarshal(event.Properties, &props) != nil {
		return false, errors.New("OpenCode event properties are malformed")
	}
	sessionID := anyString(props["sessionID"])
	if info, ok := props["info"].(map[string]any); ok && sessionID == "" {
		sessionID = anyString(info["sessionID"])
	}
	if part, ok := props["part"].(map[string]any); ok && sessionID == "" {
		sessionID = anyString(part["sessionID"])
	}
	if sessionID != s.sessionID {
		return false, nil
	}
	switch event.Type {
	case "session.status":
		status, _ := props["status"].(map[string]any)
		switch anyString(status["type"]) {
		case "busy", "retry":
			s.active = true
		case "idle":
			return s.active, nil
		}
	case "session.error":
		message := openCodeErrorText(props["error"])
		if message == "" {
			message = "Unknown runtime error"
		}
		return false, fmt.Errorf("OpenCode: %s", message)
	case "question.asked":
		return false, errors.New("OpenCode asked an interactive question unsupported by this transport; answer it in OpenCode and continue")
	case "message.updated":
		info, _ := props["info"].(map[string]any)
		id, role := anyString(info["id"]), anyString(info["role"])
		if id == "" || (role != "assistant" && role != "user") {
			return false, errors.New("OpenCode message identity is incomplete")
		}
		if len(s.roles) >= 32768 && s.roles[id] == "" {
			return false, errors.New("OpenCode message history limit exceeded")
		}
		s.roles[id] = role
		if role == "assistant" && info["error"] != nil {
			return false, fmt.Errorf("OpenCode: %s", openCodeErrorText(info["error"]))
		}
	case "message.part.updated":
		part, _ := props["part"].(map[string]any)
		return false, s.projectPart(part, emit)
	}
	return false, nil
}

func (s *openCodeEventState) projectPart(part map[string]any, emit func(ChatEvent)) error {
	id, messageID := anyString(part["id"]), anyString(part["messageID"])
	if id == "" || messageID == "" {
		return errors.New("OpenCode part identity is incomplete")
	}
	if s.roles[messageID] != "assistant" || s.parts[id] {
		return nil
	}
	kind := anyString(part["type"])
	switch kind {
	case "text", "reasoning":
		clock, _ := part["time"].(map[string]any)
		if clock["end"] == nil {
			return nil
		}
	case "tool":
		state, _ := part["state"].(map[string]any)
		if state["status"] != "completed" && state["status"] != "error" {
			return nil
		}
	case "step-start", "step-finish":
	default:
		return nil
	}
	if len(s.parts) >= 32768 {
		return errors.New("OpenCode part history limit exceeded")
	}
	s.parts[id] = true
	for _, event := range (openCodeChatDriver{}).ProjectEvent(map[string]any{"type": kind, "part": part, "sessionID": s.sessionID}) {
		emit(event)
	}
	return nil
}

func openCodeErrorText(value any) string {
	if text, ok := value.(string); ok {
		return truncate(text, 1000)
	}
	object, _ := value.(map[string]any)
	if data, ok := object["data"].(map[string]any); ok {
		if text := anyString(data["message"]); text != "" {
			return truncate(text, 1000)
		}
	}
	if text := anyString(object["message"]); text != "" {
		return truncate(text, 1000)
	}
	return truncate(anyString(object["name"]), 1000)
}
