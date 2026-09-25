package taskinput

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

type admission struct {
	kind          Kind
	mediaType     string
	preparedPath  string
	preparedBytes int64
	extension     string
}

func validateName(raw string, limits Limits) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", inputError("invalid_name", "attachment name must be a basename")
	}
	if !utf8.ValidString(name) || len(name) > limits.MaxBasenameBytes {
		return "", inputError("invalid_name", "attachment name exceeds the configured UTF-8 limit")
	}
	for _, value := range name {
		if value < 0x20 || value == 0x7f {
			return "", inputError("invalid_name", "attachment name contains control characters")
		}
	}
	runes := []rune(name)
	if len(runes) > limits.MaxDisplayCodePoints {
		name = string(runes[:limits.MaxDisplayCodePoints-1]) + "…"
	}
	return name, nil
}

func admitFile(config Config, sourcePath, name string, makeTemp func() (*os.File, error)) (admission, error) {
	file, err := os.Open(sourcePath)
	if err != nil {
		return admission{}, err
	}
	defer file.Close()
	header := make([]byte, 512)
	read, err := io.ReadFull(file, header)
	if err != nil && err != io.ErrUnexpectedEOF {
		return admission{}, err
	}
	header = header[:read]
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(header), ";")[0]))
	extension := strings.ToLower(filepath.Ext(name))
	if slices.Contains(config.ImageMediaTypes, mediaType) {
		if expectedImageMediaType(extension) != mediaType {
			return admission{}, inputError("type_mismatch", "attachment extension does not match its image bytes")
		}
		prepared, bytes, err := normalizeImage(sourcePath, mediaType, config.Limits, makeTemp)
		return admission{kind: KindImage, mediaType: mediaType, preparedPath: prepared,
			preparedBytes: bytes, extension: ".png"}, err
	}
	if !slices.Contains(config.TextExtensions, extension) {
		return admission{}, inputError("unsupported_type", "attachment type %q is not enabled", extension)
	}
	if mediaType != "text/plain" && mediaType != "application/json" && mediaType != "application/xml" {
		return admission{}, inputError("type_mismatch", "attachment bytes do not match a safe text file")
	}
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return admission{}, err
	}
	if int64(len(raw)) > config.Limits.MaxTextBytesPerItem {
		return admission{}, inputError("text_too_large", "text attachment exceeds the configured item limit")
	}
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return admission{}, inputError("invalid_text", "text attachment must be UTF-8 without NUL bytes")
	}
	prepared, err := makeTemp()
	if err != nil {
		return admission{}, err
	}
	preparedPath := prepared.Name()
	if _, err := io.Copy(prepared, bytes.NewReader(raw)); err != nil {
		_ = prepared.Close()
		_ = os.Remove(preparedPath)
		return admission{}, err
	}
	if err := prepared.Sync(); err != nil {
		_ = prepared.Close()
		_ = os.Remove(preparedPath)
		return admission{}, err
	}
	if err := prepared.Close(); err != nil {
		_ = os.Remove(preparedPath)
		return admission{}, err
	}
	return admission{kind: KindText, mediaType: mime.TypeByExtension(extension), preparedPath: preparedPath,
		preparedBytes: int64(len(raw)), extension: extension}, nil
}

func expectedImageMediaType(extension string) string {
	switch extension {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

func copyBounded(destination *os.File, source io.Reader, maximum int64) (int64, error) {
	written, err := io.Copy(destination, io.LimitReader(source, maximum+1))
	if err != nil {
		return written, err
	}
	if written > maximum {
		return written, inputError("item_too_large", "attachment exceeds the configured item limit")
	}
	if written == 0 {
		return 0, inputError("empty_input", "attachment is empty")
	}
	if err := destination.Sync(); err != nil {
		return written, fmt.Errorf("sync staged attachment: %w", err)
	}
	return written, nil
}
