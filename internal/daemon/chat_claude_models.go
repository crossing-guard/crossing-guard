package daemon

import (
	"context"
	"encoding/json"
	"os/exec"

	"crossing-guard/internal/guardcli"
)

// The initialize control response is measured on Claude Code 2.1.280. Decode
// only model metadata; account details in the same response are never retained.
func (claudeChatDriver) DiscoverChatModels(ctx context.Context, env ChatModelEnv) (ChatModelDiscovery, error) {
	discovery := ChatModelDiscovery{Scope: "Claude Code initialized model capabilities"}
	bin, err := guardcli.ResolveRuntimeBinary("claude", "")
	if err != nil {
		return discovery, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	discovery.Binary = bin
	cmd := exec.Command(bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence", "--settings", `{"disableAllHooks":true}`)
	cmd.Env = chatLaunchEnv(ChatRequest{}) // a model listing: no ticket
	session, err := env.Session(cmd)
	if err != nil {
		return discovery, err
	}
	defer session.Close()
	if err = json.NewEncoder(session.Stdin).Encode(map[string]any{"type": "control_request", "request_id": "effort-models", "request": map[string]string{"subtype": "initialize"}}); err != nil {
		return discovery, modelDiscoveryFailure(modelReasonExited)
	}
	for {
		select {
		case <-ctx.Done():
			return discovery, modelDiscoveryFailure(modelReasonTimedOut)
		case line, ok := <-session.Lines:
			if !ok {
				return discovery, modelDiscoveryFailure(modelReasonExited)
			}
			var response struct {
				Type     string `json:"type"`
				Response struct {
					RequestID string `json:"request_id"`
					Subtype   string `json:"subtype"`
					Response  struct {
						Models []claudeDiscoveredModel `json:"models"`
					} `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(line, &response) != nil || response.Type != "control_response" || response.Response.RequestID != "effort-models" {
				continue
			}
			if response.Response.Subtype != "success" {
				return discovery, modelDiscoveryFailure(modelReasonUnparseable)
			}
			discovery.Models = claudeModelOptions(response.Response.Response.Models)
			return discovery, nil
		}
	}
}

type claudeDiscoveredModel struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

func claudeModelOptions(models []claudeDiscoveredModel) []ChatModelOption {
	out := []ChatModelOption{}
	seen := map[string]bool{}
	for _, model := range models {
		if seen[model.ResolvedModel] && model.Value != "default" {
			for i := range out {
				if out[i].ID == model.ResolvedModel {
					out[i].Label = model.DisplayName
				}
			}
		}
		if model.ResolvedModel == "" || seen[model.ResolvedModel] {
			continue
		}
		seen[model.ResolvedModel] = true
		effort := nativeEffortCapability(model.SupportedEffortLevels, "")
		if !model.SupportsEffort {
			effort = &ChatEffortCapability{State: "unsupported"}
		}
		out = append(out, ChatModelOption{ID: model.ResolvedModel, Label: model.DisplayName, Description: model.Description, Source: chatModelSourceRuntime, Effort: effort})
	}
	return out
}
