// Package speech owns the speech.json operator file: its path, format version,
// decoding, validation, and the readiness verdict for the selected transcription
// backend. It hands typed sections to the packages that consume them and is
// imported only by the daemon composition root.
package speech

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"crossing-guard/internal/transcription"
)

// FormatVersion is the only speech.json format this build reads.
const FormatVersion = 1

// File is the whole speech.json document. Synthesis will be a second section
// owned here when that plan lands.
type File struct {
	FormatVersion int                  `json:"format_version"`
	Transcription transcription.Config `json:"transcription"`
}

// Loaded is a validated file plus the runtime facts Settings renders. Ready is
// false whenever Problems is non-empty.
type Loaded struct {
	Path     string
	File     File
	Backend  transcription.Backend
	Problems []string
	Ready    bool
}

const maxFileBytes = 256 << 10

// Path is where the daemon expects speech.json.
func Path(dataDir string) string { return filepath.Join(dataDir, "speech.json") }

// ClipRoot is the private directory for in-flight dictation audio.
func ClipRoot(dataDir string) string { return filepath.Join(dataDir, "speech-clips") }

// Load reads and validates speech.json. A missing or malformed file is an
// error; a valid file whose backend cannot run yet returns Problems so the
// Settings page can say exactly why. The backend's own factory performs its
// checks; this package never names a backend.
func Load(dataDir string) (Loaded, error) {
	path := Path(dataDir)
	handle, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Loaded{}, fmt.Errorf("dictation is disabled until %s is configured", path)
		}
		return Loaded{}, fmt.Errorf("open speech configuration: %w", err)
	}
	defer func() { _ = handle.Close() }()
	decoder := json.NewDecoder(io.LimitReader(handle, maxFileBytes))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return Loaded{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Loaded{}, fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if file.FormatVersion != FormatVersion {
		return Loaded{}, fmt.Errorf("validate %s: unsupported format_version %d", path, file.FormatVersion)
	}
	if err := file.Transcription.Validate(); err != nil {
		return Loaded{}, fmt.Errorf("validate %s: transcription: %w", path, err)
	}
	loaded := Loaded{Path: path, File: file}
	registered := transcription.Names()
	for name := range file.Transcription.Backends {
		if !slices.Contains(registered, name) {
			loaded.Problems = append(loaded.Problems, fmt.Sprintf("backends.%s is not a registered backend; available: %v", name, registered))
		}
	}
	backend, err := transcription.New(file.Transcription)
	if err != nil {
		loaded.Problems = append(loaded.Problems, err.Error())
	}
	if len(loaded.Problems) > 0 {
		return loaded, nil
	}
	loaded.Backend, loaded.Ready = backend, true
	return loaded, nil
}

// PrepareClipRoot empties and recreates the private clip directory. It runs at
// boot so a crash mid-dictation never leaves audio behind.
func PrepareClipRoot(dataDir string) (string, error) {
	root := ClipRoot(dataDir)
	if err := os.RemoveAll(root); err != nil {
		return "", fmt.Errorf("clear speech clip directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create speech clip directory: %w", err)
	}
	return root, nil
}
