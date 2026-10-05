package guardcli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"time"
)

// Measured on agy 1.2.14. Native arguments are retained separately from the
// canonical projection the shared evaluator/resource collectors understand.
const antigravityMaxHookBytes = 4 << 20

func (antigravityInstaller) DecodeHookInput(input io.Reader, event string) (hookInput, error) {
	in := hookInput{HookEventName: event, RawHookEventName: event}
	if event != "PreToolUse" && event != "PostToolUse" && event != "Stop" {
		return in, fmt.Errorf("unsupported hook event %q", event)
	}
	raw, err := io.ReadAll(io.LimitReader(input, antigravityMaxHookBytes+1))
	if err != nil {
		return in, err
	}
	if len(raw) > antigravityMaxHookBytes {
		return in, fmt.Errorf("hook payload exceeds %d bytes", antigravityMaxHookBytes)
	}
	var wire struct {
		ConversationID string   `json:"conversationId"`
		WorkspacePaths []string `json:"workspacePaths"`
		TranscriptPath string   `json:"transcriptPath"`
		StepIdx        *int     `json:"stepIdx"`
		FullyIdle      bool     `json:"fullyIdle"`
		Error          string   `json:"error"`
		ToolCall       struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return in, err
	}
	if wire.ConversationID == "" {
		return in, fmt.Errorf("missing conversationId")
	}
	// hooks.json is shared with desktop. Its events must not enter the CLI lane.
	// This is a source-shape check, not authentication of the vendor process.
	if !antigravityCLITranscript(wire.TranscriptPath, wire.ConversationID) {
		return hookInput{}, nil
	}
	in.SessionID, in.TranscriptPath = wire.ConversationID, wire.TranscriptPath
	in.RawEnvelopeBytes = len(raw)
	if event == "Stop" {
		in.Cwd = boundedWorkspaceCwd(wire.WorkspacePaths, toolInput{})
		if wire.FullyIdle {
			in.TurnKind = "turn.ended"
		}
		return in, nil
	}
	if wire.StepIdx == nil || *wire.StepIdx < 0 || wire.ToolCall.Name == "" {
		return in, fmt.Errorf("tool hook requires nonnegative stepIdx and toolCall.name")
	}
	in.CallID = "step:" + strconv.Itoa(*wire.StepIdx)
	in.RawToolInput = append(json.RawMessage(nil), wire.ToolCall.Args...)
	in.ToolName, in.ToolInput, err = antigravityToolInput(wire.ToolCall.Name, wire.ToolCall.Args)
	if err != nil {
		return in, err
	}
	in.Cwd = boundedWorkspaceCwd(wire.WorkspacePaths, in.ToolInput)
	in.ToolIsError, in.ToolError = wire.Error != "", wire.Error
	return in, nil
}

func antigravityCLITranscript(path, session string) bool {
	if !filepath.IsAbs(path) || session == "" || filepath.Base(session) != session {
		return false
	}
	name := filepath.Base(path)
	if name != "transcript.jsonl" && name != "transcript_full.jsonl" {
		return false
	}
	directory := filepath.Dir(filepath.Clean(path))
	for _, component := range []string{"logs", ".system_generated", session, "brain", "antigravity-cli", ".gemini"} {
		if filepath.Base(directory) != component {
			return false
		}
		directory = filepath.Dir(directory)
	}
	return true
}

func antigravityToolInput(name string, raw json.RawMessage) (string, toolInput, error) {
	var args struct {
		CommandLine        string
		Cwd                string
		AbsolutePath       string
		TargetFile         string
		CodeContent        string
		TargetContent      string
		ReplacementContent string
		SearchPath         string
		SearchDirectory    string
		Url                string
	}
	if len(raw) == 0 || string(raw) == "null" {
		return name, toolInput{}, fmt.Errorf("toolCall.args must be an object")
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return name, toolInput{}, err
	}
	in := toolInput{WorkingDirectory: args.Cwd}
	switch name {
	case "run_command":
		name, in.Command = "Bash", commandField(args.CommandLine)
	case "view_file":
		name, in.FilePath = "Read", args.AbsolutePath
	case "write_to_file":
		name, in.FilePath, in.Content = "Write", args.TargetFile, args.CodeContent
	case "replace_file_content":
		name, in.FilePath = "Edit", args.TargetFile
		in.OldString, in.NewString = args.TargetContent, args.ReplacementContent
	case "grep_search":
		name, in.Path = "Grep", args.SearchPath
	case "find_by_name":
		name, in.Path = "Glob", args.SearchDirectory
	case "read_url_content":
		name, in.URL = "WebFetch", args.Url
	}
	return name, in, nil
}

// Unknown vendor timing must not inherit another runtime's approval wait.
func (antigravityInstaller) HookAskBudget() time.Duration { return 0 }

func (antigravityInstaller) EncodeHookDeny(_ string, reason string) []byte {
	body, _ := json.Marshal(map[string]string{"decision": "deny", "reason": reason})
	return body
}

var _ HookInputDecoder = antigravityInstaller{}
var _ HookDecisionEncoder = antigravityInstaller{}
