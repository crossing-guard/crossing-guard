// Package taskinput owns private, bounded task-input staging and lifecycle.
package taskinput

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const FormatVersion = 1

type Limits struct {
	MaxItemsPerScope           int   `json:"max_items_per_scope"`
	MaxSourceBytesPerItem      int64 `json:"max_source_bytes_per_item"`
	MaxSourceBytesPerScope     int64 `json:"max_source_bytes_per_scope"`
	MaxGlobalSourceBytes       int64 `json:"max_global_source_bytes"`
	MaxTextBytesPerItem        int64 `json:"max_text_bytes_per_item"`
	MaxTextBytesPerScope       int64 `json:"max_text_bytes_per_scope"`
	MaxImageDimension          int   `json:"max_image_dimension"`
	MaxImagePixels             int64 `json:"max_image_pixels"`
	MaxNormalizedBytesPerItem  int64 `json:"max_normalized_bytes_per_item"`
	MaxNormalizedBytesPerScope int64 `json:"max_normalized_bytes_per_scope"`
	MaxBasenameBytes           int   `json:"max_basename_bytes"`
	MaxDisplayCodePoints       int   `json:"max_display_code_points"`
	UnclaimedExpirySeconds     int64 `json:"unclaimed_expiry_seconds"`
	MaxConcurrentAdmissions    int   `json:"max_concurrent_admissions"`
}

type Config struct {
	FormatVersion   int      `json:"format_version"`
	TextExtensions  []string `json:"text_extensions"`
	ImageMediaTypes []string `json:"image_media_types"`
	Limits          Limits   `json:"limits"`
}

type LoadedConfig struct {
	Config Config
	Path   string
}

func ConfigPath(dataDir string) string { return filepath.Join(dataDir, "task-inputs.json") }

func LoadConfig(dataDir string) (LoadedConfig, error) {
	path := ConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LoadedConfig{}, fmt.Errorf("task input admission is disabled until %s is configured", path)
		}
		return LoadedConfig{}, fmt.Errorf("open task input configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return LoadedConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return LoadedConfig{}, fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.Validate(); err != nil {
		return LoadedConfig{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return LoadedConfig{Config: config, Path: path}, nil
}

func (c *Config) Validate() error {
	if c.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported format_version %d", c.FormatVersion)
	}
	var err error
	c.TextExtensions, err = normalizedUnique(c.TextExtensions, func(value string) (string, bool) {
		value = strings.ToLower(strings.TrimSpace(value))
		return value, strings.HasPrefix(value, ".") && len(value) > 1 && !strings.ContainsAny(value, `/\\`)
	})
	if err != nil || len(c.TextExtensions) == 0 {
		return errors.New("text_extensions must contain unique dot-prefixed extensions")
	}
	c.ImageMediaTypes, err = normalizedUnique(c.ImageMediaTypes, func(value string) (string, bool) {
		value = strings.ToLower(strings.TrimSpace(value))
		return value, slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}, value)
	})
	if err != nil || len(c.ImageMediaTypes) == 0 {
		return errors.New("image_media_types must contain supported unique image media types")
	}
	positiveInts := map[string]int{
		"max_items_per_scope":       c.Limits.MaxItemsPerScope,
		"max_image_dimension":       c.Limits.MaxImageDimension,
		"max_basename_bytes":        c.Limits.MaxBasenameBytes,
		"max_display_code_points":   c.Limits.MaxDisplayCodePoints,
		"max_concurrent_admissions": c.Limits.MaxConcurrentAdmissions,
	}
	for name, value := range positiveInts {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	positiveInt64s := map[string]int64{
		"max_source_bytes_per_item":      c.Limits.MaxSourceBytesPerItem,
		"max_source_bytes_per_scope":     c.Limits.MaxSourceBytesPerScope,
		"max_global_source_bytes":        c.Limits.MaxGlobalSourceBytes,
		"max_text_bytes_per_item":        c.Limits.MaxTextBytesPerItem,
		"max_text_bytes_per_scope":       c.Limits.MaxTextBytesPerScope,
		"max_image_pixels":               c.Limits.MaxImagePixels,
		"max_normalized_bytes_per_item":  c.Limits.MaxNormalizedBytesPerItem,
		"max_normalized_bytes_per_scope": c.Limits.MaxNormalizedBytesPerScope,
		"unclaimed_expiry_seconds":       c.Limits.UnclaimedExpirySeconds,
	}
	for name, value := range positiveInt64s {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.Limits.MaxTextBytesPerItem > c.Limits.MaxSourceBytesPerItem {
		return errors.New("max_text_bytes_per_item cannot exceed max_source_bytes_per_item")
	}
	if c.Limits.MaxSourceBytesPerItem > c.Limits.MaxSourceBytesPerScope || c.Limits.MaxSourceBytesPerScope > c.Limits.MaxGlobalSourceBytes {
		return errors.New("source byte limits must increase from item to scope to global")
	}
	if time.Duration(c.Limits.UnclaimedExpirySeconds)*time.Second <= 0 {
		return errors.New("unclaimed expiry overflows duration")
	}
	return nil
}

func normalizedUnique(values []string, normalize func(string) (string, bool)) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		value, valid := normalize(raw)
		if !valid {
			return nil, fmt.Errorf("invalid value %q", raw)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("duplicate value %q", value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}
