package memcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const attachmentRecordVersion = 1

// attachmentRecord is evidence of the config path a successful memory attach wrote.
// It is not governance consent: guard attachment and memory injection are separate
// user choices and, for Codex, do not even use the same config file.
type attachmentRecord struct {
	Version    int    `json:"version"`
	Vendor     string `json:"vendor"`
	ConfigPath string `json:"config_path"`
}

func attachmentRecordPath(vendor string) string {
	return filepath.Join(filepath.Dir(memoryDir()), "memory-attachments", vendor+".json")
}

func readAttachmentRecord(vendor string) (attachmentRecord, bool, error) {
	raw, err := os.ReadFile(attachmentRecordPath(vendor))
	if os.IsNotExist(err) {
		return attachmentRecord{}, false, nil
	}
	if err != nil {
		return attachmentRecord{}, false, err
	}
	var rec attachmentRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return attachmentRecord{}, true, fmt.Errorf("attachment evidence unreadable: %w", err)
	}
	if rec.Version != attachmentRecordVersion || rec.Vendor != vendor || rec.ConfigPath == "" {
		return attachmentRecord{}, true, fmt.Errorf("attachment evidence unreadable: invalid %s record", vendor)
	}
	return rec, true, nil
}

func recordAttachment(vendor, configPath string) error {
	path := attachmentRecordPath(vendor)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	rec := attachmentRecord{Version: attachmentRecordVersion, Vendor: vendor, ConfigPath: configPath}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".attachment-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
