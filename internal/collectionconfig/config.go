// Package collectionconfig owns the one local selection that controls retained
// result payloads. It deliberately is not a generic configuration service.
package collectionconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	FormatVersion = 1

	MetadataOnly    Mode = "metadata-only"
	CodeEffects     Mode = "code-effects"
	CompleteBounded Mode = "complete-bounded"
)

type Mode string

func (m Mode) Valid() bool {
	switch m {
	case MetadataOnly, CodeEffects, CompleteBounded:
		return true
	default:
		return false
	}
}

type Document struct {
	FormatVersion     int  `json:"format_version"`
	ResultPayloadMode Mode `json:"result_payload_mode"`
}

type Loaded struct {
	Document Document `json:"document"`
	Path     string   `json:"path"`
	Origin   string   `json:"origin"`
}

func Default() Document {
	return Document{FormatVersion: FormatVersion, ResultPayloadMode: CodeEffects}
}

func Path(dataDir string) string {
	return filepath.Join(dataDir, "collection.json")
}

// Load returns the documented built-in default only when the artifact is absent.
// A present but malformed artifact is an operator-visible error, never a fallback.
func Load(dataDir string) (Loaded, error) {
	path := Path(dataDir)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Loaded{Document: Default(), Path: path, Origin: "builtin-default"}, nil
	}
	if err != nil {
		return Loaded{}, err
	}
	defer f.Close()
	var document Document
	decoder := json.NewDecoder(io.LimitReader(f, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return Loaded{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Loaded{}, fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if document.FormatVersion != FormatVersion {
		return Loaded{}, fmt.Errorf("%s: unsupported format_version %d", path, document.FormatVersion)
	}
	if !document.ResultPayloadMode.Valid() {
		return Loaded{}, fmt.Errorf("%s: unknown result_payload_mode %q", path, document.ResultPayloadMode)
	}
	return Loaded{Document: document, Path: path, Origin: "local-file"}, nil
}
