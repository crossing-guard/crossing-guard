package modelroute

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// configFormat names the tunables document. The shipped defaults are the embedded
// routes.default.json; an owner who wants other bounds writes the same shape to
// <dataDir>/models/routes.json. Nobody has to create that file.
const configFormat = "crossing-guard-model-routes-config-v1"

//go:embed routes.default.json
var defaultConfigDocument []byte

// Config holds the route owner's tunables. Both are storage bounds, not policy.
type Config struct {
	FormatVersion string `json:"format_version"`
	// HistoryMax is how many earlier revisions one route's selection record keeps.
	HistoryMax int `json:"history_max"`
	// NameMaxRunes bounds a route's display name.
	NameMaxRunes int `json:"name_max_runes"`
}

// Hard limits a configured value must stay inside: a selection record is read whole
// and bounded by maxJSONBytes, so its history cannot be unbounded.
const (
	historyMaxCeiling   = 500
	nameMaxRunesCeiling = 200
)

// DefaultConfig returns the shipped tunables, decoded from the embedded document.
func DefaultConfig() Config {
	var config Config
	if err := decodeStrictJSON(defaultConfigDocument, &config); err != nil {
		// The embedded document is checked by TestDefaultConfigDocumentIsValid; a
		// broken one is a build defect, not a runtime condition to recover from.
		panic("modelroute: embedded routes.default.json is invalid: " + err.Error())
	}
	return config
}

func (config Config) validate() error {
	if config.FormatVersion != configFormat {
		return fmt.Errorf("model routes config format_version must be %q", configFormat)
	}
	if config.HistoryMax < 1 || config.HistoryMax > historyMaxCeiling {
		return fmt.Errorf("model routes config history_max must be between 1 and %d", historyMaxCeiling)
	}
	if config.NameMaxRunes < 1 || config.NameMaxRunes > nameMaxRunesCeiling {
		return fmt.Errorf("model routes config name_max_runes must be between 1 and %d", nameMaxRunesCeiling)
	}
	return nil
}

// loadConfig reads <dataDir>/models/routes.json when present and valid; otherwise the
// shipped defaults apply. An unreadable or invalid override is an error the caller
// surfaces — it never silently changes a bound.
func loadConfig(dataDir string) (Config, error) {
	config := DefaultConfig()
	raw, err := readRegularFile(filepath.Join(dataDir, "models", "routes.json"), maxJSONBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("model routes config: %w", err)
	}
	override := Config{}
	if err := decodeStrictJSON(raw, &override); err != nil {
		return config, fmt.Errorf("model routes config: %w", err)
	}
	if err := override.validate(); err != nil {
		return config, err
	}
	return override, nil
}
