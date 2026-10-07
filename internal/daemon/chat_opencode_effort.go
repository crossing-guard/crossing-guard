package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// Variants may contain credentials or unrelated settings. Only a single explicit
// reasoningEffort field is admitted here; everything else remains unavailable.
func openCodeEffort(variants map[string]json.RawMessage, reasoning *bool) *ChatEffortCapability {
	out := &ChatEffortCapability{State: "unknown"}
	if reasoning != nil && !*reasoning {
		out.State = "unsupported"
		return out
	}
	for id, raw := range variants {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 {
			continue
		}
		var level string
		if json.Unmarshal(fields["reasoningEffort"], &level) != nil || effortLabel(level) == "" {
			continue
		}
		sum := sha256.Sum256([]byte(level))
		out.Choices = append(out.Choices, ChatEffortChoice{ID: id, Label: effortLabel(level), MappingDigest: hex.EncodeToString(sum[:])})
	}
	order := map[string]int{"None": 0, "Minimal": 1, "Low": 2, "Medium": 3, "High": 4, "Extra high": 5, "Max": 6, "Ultra": 7}
	sort.Slice(out.Choices, func(i, j int) bool {
		a, b := out.Choices[i], out.Choices[j]
		if a.Label == b.Label {
			return a.ID < b.ID
		}
		return order[a.Label] < order[b.Label]
	})
	if len(out.Choices) > 0 {
		out.State = "supported"
		out.CanStart = true
		out.CanResume = true
	}
	return out
}

// The live server has the effective project configuration. Never assume the
// globally discovered variant still means the same thing in this directory.
func (s *openCodeServerProtocol) validateEffort(ctx context.Context) error {
	if s.request.ThinkingEffort == nil || s.request.ThinkingEffort.Kind != "level" {
		return nil
	}
	var config struct {
		Providers []struct {
			ID     string                          `json:"id"`
			Models map[string]openCodeVerboseModel `json:"models"`
		} `json:"providers"`
	}
	if err := s.call(ctx, "GET", "/config/providers", nil, &config); err != nil {
		return err
	}
	provider, id, _ := strings.Cut(s.request.Model, "/")
	for _, p := range config.Providers {
		if p.ID != provider {
			continue
		}
		model, ok := p.Models[id]
		if !ok {
			break
		}
		effort := openCodeEffort(model.Variants, model.Capabilities.Reasoning)
		for _, choice := range effort.Choices {
			if choice.ID == s.request.ThinkingEffort.Value && choice.MappingDigest == s.request.effortMapping && choice.MappingDigest != "" {
				return nil
			}
		}
	}
	return errors.New("OpenCode effort variant differs from verified catalog; refresh model settings")
}

func (openCodeChatDriver) ParseEffort(req ChatRequest) (ChatRequest, error) {
	if strings.TrimSpace(req.ExtraArgs) != "" {
		return req, effortError("legacy_conflict", "OpenCode extra args are unsupported; remove them in Settings")
	}
	return req, nil
}
