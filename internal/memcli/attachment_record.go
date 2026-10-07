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
	// AddedByAction is true only when an attach WROTE the entry: one found
	// already present — added by hand, or by something else — is not this
	// record's to remove (team rest-of-release plan §17.1 F-2). Binary is the
	// executable that entry's command names, which is how a detach finds it.
	AddedByAction bool   `json:"added_by_action,omitempty"`
	Binary        string `json:"binary,omitempty"`
	// CreatedFile is true when the attach that wrote the entry also created the
	// settings file: there was none before. A detach that leaves such a file empty
	// removes it, so the home is as it was before the attach.
	CreatedFile bool `json:"created_file,omitempty"`
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

// recordAttachment writes the evidence of one attach. wrote says the attach wrote
// the entry for binary; when it did not, an earlier record of the same entry
// keeps what it said, and anything else is recorded as not this action's. createdFile
// says the attach created the settings file itself; it counts only with wrote.
func recordAttachment(vendor, configPath, binary string, wrote, createdFile bool) error {
	path := attachmentRecordPath(vendor)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	rec := attachmentRecord{Version: attachmentRecordVersion, Vendor: vendor, ConfigPath: configPath}
	if wrote {
		rec.AddedByAction, rec.Binary, rec.CreatedFile = true, binary, createdFile
	} else if prior, found, err := readAttachmentRecord(vendor); err == nil && found &&
		prior.ConfigPath == configPath && prior.Binary == binary {
		rec.AddedByAction, rec.Binary, rec.CreatedFile = prior.AddedByAction, prior.Binary, prior.CreatedFile
	}
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

// forgetAttachment removes the evidence of an attachment that was detached.
func forgetAttachment(vendor string) error {
	if err := os.Remove(attachmentRecordPath(vendor)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
