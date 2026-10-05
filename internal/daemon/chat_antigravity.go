package daemon

import (
	"bufio"
	"context"
	"crossing-guard/internal/guardcli"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"crossing-guard/internal/taskinput"
)

func init() { registerChatDriver("antigravity", antigravityChatDriver{}) }

// antigravityChatDriver drives the official headless CLI. It uses the existing
// task/process owner; native tools remain subject to the CLI's own permissions.
type antigravityChatDriver struct{}

var antigravityConversationUUID = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (antigravityChatDriver) ChatCapability() ChatCapability {
	return ChatCapability{
		MessageDelivery: SessionMessageCapability{
			Detail: "Antigravity has no verified external transport into an exact active CLI conversation.",
		},
		Runtime: "antigravity", DisplayName: "Antigravity CLI",
		CanStart: true, CanResume: true, VendorAuthDefault: true,
		AcceptsCustomModel: true, ModelHint: "Choose a model reported by the CLI, or enter an exact custom model ID.",
		Modes: []ChatMode{{ID: "", Label: "Vendor review", Risk: "elevated",
			Description: "The CLI's configured permissions apply. Headless approval requests can be denied automatically; Crossing Guard cannot answer them here."}},
		Models: []ChatModelOption{{ID: "", Label: "CLI default", Description: "Use the signed-in CLI's configured model."},
			{ID: "custom", Label: "Exact model ID", Custom: true}},
		Inputs: []ChatInputCapability{{Kind: "text", MediaTypes: []string{"text/plain"},
			CanStart: true, CanResume: true, Note: "Prompt text only; separate file attachments are unavailable."}},
	}
}

func (antigravityChatDriver) ValidateChatInputs(_ ChatRequest, inputs []taskinput.ResolvedInput) error {
	if len(inputs) != 0 {
		return errors.New("antigravity separate task inputs are not verified; send prompt text only")
	}
	return nil
}

func (antigravityChatDriver) CanonicalizeChatRequest(req ChatRequest) (ChatRequest, error) {
	if req.SessionID != "" && !antigravityConversationUUID.MatchString(req.SessionID) {
		return ChatRequest{}, errors.New("antigravity resume requires an exact conversation UUID")
	}
	if req.Model != "" && !antigravityModelID(req.Model) {
		return ChatRequest{}, errors.New("antigravity model ID must be a bounded nonblank token")
	}
	if req.Mode != "" || req.ExtraArgs != "" || req.BaseURL != "" ||
		req.AuthToken != "" || req.PermissionMode != "" || req.Sandbox != "" ||
		req.OSS || req.LocalProvider != "" || req.ThinkingEffort != nil {
		return ChatRequest{}, errors.New("antigravity currently accepts only its configured mode and vendor authentication")
	}
	return req, nil
}

func (d antigravityChatDriver) BuildCmd(req ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error) {
	var err error
	req, err = d.CanonicalizeChatRequest(req)
	if err != nil {
		return nil, err
	}
	if err := d.ValidateChatInputs(req, launch.Inputs); err != nil {
		return nil, err
	}
	bin, err := guardcli.ResolveRuntimeBinary("agy", req.Binary)
	if err != nil {
		return nil, err
	}
	args := []string{"-p", req.Prompt, "--output-format", "stream-json",
		"--disable-slash-commands", "--sandbox", "--print-timeout", "5m"}
	if req.SessionID != "" {
		args = append(args, "--conversation", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = req.Cwd
	cmd.Env = chatLaunchEnv(req)
	return cmd, nil
}

// The CLI owns model existence. We bound a single literal ID; catalog labels
// supply no evidence about price, modality, limits or effort choices.
func antigravityModelID(id string) bool {
	return id != "" && !strings.HasPrefix(id, "-") && len(id) <= defaultChatModelBounds.MaxIDBytes &&
		strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func (antigravityChatDriver) DiscoverChatModels(_ context.Context, env ChatModelEnv) (ChatModelDiscovery, error) {
	discovery := ChatModelDiscovery{Scope: "Antigravity CLI signed-in account and global configuration"}
	bin, err := guardcli.ResolveRuntimeBinary("agy", "")
	if err != nil {
		return discovery, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	discovery.Binary = bin
	cmd := exec.Command(bin, "models")
	cmd.Env = chatLaunchEnv(ChatRequest{})
	out, err := env.Run(cmd)
	if err != nil {
		return discovery, err
	}
	discovery.Models, discovery.Rejected, err = parseAntigravityModels(out)
	return discovery, err
}

func parseAntigravityModels(out []byte) ([]ChatModelOption, int, error) {
	models := []ChatModelOption{}
	rejected := 0
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, label, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "\t")
		label = strings.TrimSpace(label)
		if !ok || !antigravityModelID(id) || label == "" || len(label) > defaultChatModelBounds.MaxLabelBytes ||
			strings.IndexFunc(label, unicode.IsControl) >= 0 || seen[id] || id == "custom" {
			rejected++
			continue
		}
		seen[id] = true
		models = append(models, ChatModelOption{ID: id, Label: label, Source: chatModelSourceRuntime})
	}
	if len(models) == 0 {
		return models, rejected, modelDiscoveryFailure(modelReasonUnparseable)
	}
	return models, rejected, nil
}

func (antigravityChatDriver) ProjectEvent(map[string]any) []ChatEvent {
	// A terminal result must determine task success; the stateful protocol below
	// owns projection so a process that exits 0 without an answer still fails.
	return nil
}

func (antigravityChatDriver) ProcessProtocol(req ChatRequest, _ ChatLaunchContext, _ *exec.Cmd) chatProcessProtocol {
	return &antigravityStream{requestedID: req.SessionID, seenTools: map[int]bool{},
		seenResults: map[int]bool{}, responseSteps: map[int]bool{}}
}

type antigravityStream struct {
	requestedID    string
	conversationID string
	seenInit       bool
	seenTools      map[int]bool
	seenResults    map[int]bool
	seenResult     bool
	failedTool     string
	responseSteps  map[int]bool // true once the native assistant step is DONE
	responseBytes  int
}

func (*antigravityStream) WaitForNaturalExit() bool { return true }

func (stream *antigravityStream) Run(stdout io.ReadCloser, emit func(ChatEvent)) error {
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 0, 64*1024), taskStdoutRecordMax)
	for scan.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
			return errors.New("antigravity emitted a malformed stream record")
		}
		if stream.seenResult {
			return errors.New("antigravity emitted data after the terminal result")
		}
		switch anyString(record["event"]) {
		case "init":
			if stream.seenInit {
				return errors.New("antigravity emitted a duplicate stream init")
			}
			if err := stream.acceptID(anyString(record["conversation_id"])); err != nil {
				return err
			}
			stream.seenInit = true
			emit(ChatEvent{"type": "session", "id": stream.conversationID})
		case "step_update":
			if !stream.seenInit {
				return errors.New("antigravity emitted a step before stream init")
			}
			step, _ := record["step_update"].(map[string]any)
			if err := stream.acceptID(anyString(step["conversation_id"])); err != nil {
				return err
			}
			stream.emitTool(step, emit)
			if err := stream.emitResponse(step, emit); err != nil {
				return err
			}
		case "result":
			if !stream.seenInit {
				return errors.New("antigravity emitted a result before stream init")
			}
			result, _ := record["result"].(map[string]any)
			if err := stream.acceptID(anyString(result["conversation_id"])); err != nil {
				return err
			}
			if status := anyString(result["status"]); status != "SUCCESS" {
				detail, _ := result["error"].(map[string]any)
				if strings.Contains(strings.ToUpper(anyString(detail["message"])), "RESOURCE_EXHAUSTED") {
					return errors.New("antigravity quota exhausted; retry after the vendor reset")
				}
				return errors.New("antigravity turn ended unsuccessfully; check runtime diagnostics")
			}
			response := anyString(result["response"])
			if strings.TrimSpace(response) == "" {
				if stream.failedTool != "" {
					return fmt.Errorf("antigravity ended without a response after %s failed", stream.failedTool)
				}
				return errors.New("antigravity ended without a response; check runtime diagnostics")
			}
			emit(ChatEvent{"type": "text", "text": truncate(response, 64*1024)})
			emit(ChatEvent{"type": "result", "id": stream.conversationID})
			stream.seenResult = true
		default:
			// Future vendor records do not become invented conversation content.
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("antigravity output stream failed: %w", err)
	}
	if stream.seenResult {
		return nil
	}
	return errors.New("antigravity ended before a terminal result")
}

func (stream *antigravityStream) acceptID(id string) error {
	if !antigravityConversationUUID.MatchString(id) {
		return errors.New("antigravity stream did not report an exact conversation UUID")
	}
	if stream.requestedID != "" && !strings.EqualFold(id, stream.requestedID) {
		return errors.New("antigravity resumed a different conversation")
	}
	if stream.conversationID != "" && !strings.EqualFold(id, stream.conversationID) {
		return errors.New("antigravity changed conversation identity during a turn")
	}
	stream.conversationID = id
	return nil
}

func (stream *antigravityStream) emitResponse(step map[string]any, emit func(ChatEvent)) error {
	state := anyString(step["state"])
	if anyString(step["step_type"]) != "agent_response" || (state != "ACTIVE" && state != "DONE") {
		return nil
	}
	index, ok := step["step_index"].(float64)
	if !ok || index < 0 || index != float64(int(index)) {
		return errors.New("antigravity emitted an invalid assistant step index")
	}
	i := int(index)
	if done, seen := stream.responseSteps[i]; seen {
		if done {
			if state == "DONE" {
				return nil
			}
			return errors.New("antigravity updated a completed assistant step")
		}
	} else if len(stream.responseSteps) >= 4096 {
		return errors.New("antigravity emitted too many assistant steps")
	}
	text, ok := step["text_delta"].(string)
	if !ok && step["text_delta"] != nil {
		return errors.New("antigravity emitted an invalid assistant text delta")
	}
	stream.responseSteps[i] = state == "DONE"
	// Native deltas are fragments, including spaces/newlines. The shared
	// truncate helper trims text, so only clip this turn's remaining byte budget.
	if remaining := 64*1024 - stream.responseBytes; len(text) > remaining {
		text = text[:remaining]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	stream.responseBytes += len(text)
	if text != "" {
		emit(ChatEvent{"type": "delta", "text": text})
	}
	return nil
}

func (stream *antigravityStream) emitTool(step map[string]any, emit func(ChatEvent)) {
	if anyString(step["step_type"]) != "tool" {
		return
	}
	index, ok := step["step_index"].(float64)
	if !ok || index < 0 || index != float64(int(index)) {
		return
	}
	info, _ := step["tool_info"].(map[string]any)
	name := anyString(step["tool_name"])
	if name == "" {
		name = anyString(info["name"])
	}
	if name == "" {
		return
	}
	i := int(index)
	if !stream.seenTools[i] {
		stream.seenTools[i] = true
		emit(ChatEvent{"type": "tool", "name": truncate(name, 128),
			"text": truncate(compactJSON(info["parameters"]), 600)})
	}
	if state := anyString(step["state"]); state == "DONE" || state == "ERROR" {
		if stream.seenResults[i] {
			return
		}
		stream.seenResults[i] = true
		output := anyString(info["output"])
		isError := state == "ERROR"
		if isError {
			stream.failedTool = truncate(name, 80)
			if detail, ok := info["error"].(map[string]any); ok {
				output = anyString(detail["message"])
			}
		}
		if output != "" {
			emit(ChatEvent{"type": "tool_result", "text": truncate(output, 600), "is_error": isError})
		}
	}
}
