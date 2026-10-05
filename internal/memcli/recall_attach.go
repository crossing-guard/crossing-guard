package memcli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Memory recall as an action another surface performs (team rest-of-release plan
// §6.3, OD-22, §17.1 F-2): the adapter call plus the attachment record the doctor
// reads, returning an error instead of exiting. The `attach` verb and the daemon's
// one-click action share attachAt, so both leave the same evidence.
//
// Whether the caller MAY edit a runtime's settings is not decided here: the daemon
// checks that this home's lifecycle hook for the runtime names its own executable
// (OD-24) before it calls.

// ErrUnknownRecallRuntime is returned for a runtime with no memory adapter.
var ErrUnknownRecallRuntime = errors.New("this runtime has no memory recall hook")

// Why a detach removed nothing, as data codes.
const (
	// RecallNotAttached: no attachment is recorded and no entry is present.
	RecallNotAttached = "not_attached"
	// RecallNotAddedByAction: an entry is present that an attach did not write — it
	// was there already, added by hand or by something else. It is left alone.
	RecallNotAddedByAction = "not_added_by_action"
	// RecallEntryChanged: the recorded entry is no longer in the file as it was
	// written. Nothing matches exactly, so nothing is removed.
	RecallEntryChanged = "entry_changed"
)

// RecallAttachment is what is known about one runtime's memory hook entry.
type RecallAttachment struct {
	Runtime    string
	ConfigPath string
	// EntryPresent says a memory hook entry is in the file; HookBinary is the
	// executable it names.
	EntryPresent bool
	HookBinary   string
	// AddedByAction says the recorded attach wrote the entry that is present:
	// only then does a detach remove it.
	AddedByAction bool
}

// RecallAttachResult says what an attach did.
type RecallAttachResult struct {
	RecallAttachment
	// Wrote is false when an entry for this executable was already there: it was
	// left as it is and is not recorded as this action's.
	Wrote bool
}

// RecallDetachResult says what a detach did. LeftAlone is a data code when nothing
// was removed.
type RecallDetachResult struct {
	RecallAttachment
	Removed   bool
	LeftAlone string
}

// RecallRuntimes names the runtimes that have a memory adapter.
func RecallRuntimes() []string {
	names := make([]string, 0, len(adapters))
	for name := range adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// recallConfigPath is the settings file a runtime's memory hook lives in: the one a
// recorded attach wrote, else the runtime's default.
func recallConfigPath(adapter Adapter) (string, attachmentRecord, bool) {
	rec, found, err := readAttachmentRecord(adapter.Name())
	if err == nil && found {
		return rec.ConfigPath, rec, true
	}
	return adapter.DefaultConfigPath(), attachmentRecord{}, false
}

// RuntimeRecallAttachment reads one runtime's memory hook entry and whose it is.
func RuntimeRecallAttachment(runtime string) (RecallAttachment, error) {
	adapter, ok := adapters[runtime]
	if !ok {
		return RecallAttachment{Runtime: runtime}, ErrUnknownRecallRuntime
	}
	config, rec, recorded := recallConfigPath(adapter)
	out := RecallAttachment{Runtime: runtime, ConfigPath: config, HookBinary: adapter.MemoryHookBinary(config)}
	out.EntryPresent = out.HookBinary != ""
	out.AddedByAction = recorded && rec.AddedByAction && out.EntryPresent && rec.Binary == out.HookBinary
	return out, nil
}

// attachAt installs the hook at config for self and records the evidence.
func attachAt(adapter Adapter, config, self string) (bool, error) {
	_, statErr := os.Lstat(config)
	absentBefore := errors.Is(statErr, fs.ErrNotExist)
	wrote, err := adapter.Attach(config, self)
	if err != nil {
		return false, err
	}
	if err := recordAttachment(adapter.Name(), config, self, wrote, absentBefore); err != nil {
		return wrote, fmt.Errorf("hook may be installed at %s, but attachment evidence was not recorded: %w", config, err)
	}
	return wrote, nil
}

// AttachRuntime turns memory recall on for a runtime: it installs the existing memory
// hook through the runtime's adapter, in the settings file a recorded attach used or
// the runtime's default. executable is the path the hook command names — the caller's
// own executable, the same one that home's lifecycle hook for the runtime names
// (OD-24); it is passed in so the path that was checked is the path that is written.
// An entry already there is left alone and reported (Wrote false).
func AttachRuntime(runtime, executable string) (RecallAttachResult, error) {
	adapter, ok := adapters[runtime]
	if !ok {
		return RecallAttachResult{}, ErrUnknownRecallRuntime
	}
	self, err := filepath.Abs(executable)
	if err != nil || executable == "" {
		return RecallAttachResult{}, fmt.Errorf("the executable the memory hook runs could not be resolved: %q", executable)
	}
	config, _, _ := recallConfigPath(adapter)
	wrote, err := attachAt(adapter, config, self)
	if err != nil {
		return RecallAttachResult{}, err
	}
	state, err := RuntimeRecallAttachment(runtime)
	return RecallAttachResult{RecallAttachment: state, Wrote: wrote}, err
}

// DetachRuntime turns memory recall off: it removes exactly the entry a recorded
// attach wrote — and nothing else. An entry that attach found already present, or one
// that no longer matches what was written, is left alone and the result says why.
func DetachRuntime(runtime string) (RecallDetachResult, error) {
	adapter, ok := adapters[runtime]
	if !ok {
		return RecallDetachResult{}, ErrUnknownRecallRuntime
	}
	before, err := RuntimeRecallAttachment(runtime)
	if err != nil {
		return RecallDetachResult{}, err
	}
	rec, recorded, err := readAttachmentRecord(runtime)
	if err != nil {
		return RecallDetachResult{RecallAttachment: before}, err
	}
	switch {
	case !before.EntryPresent && !recorded:
		return RecallDetachResult{RecallAttachment: before, LeftAlone: RecallNotAttached}, nil
	case !recorded || !rec.AddedByAction:
		return RecallDetachResult{RecallAttachment: before, LeftAlone: RecallNotAddedByAction}, nil
	}
	removed, err := adapter.Detach(rec.ConfigPath, rec.Binary)
	if err != nil {
		return RecallDetachResult{RecallAttachment: before}, err
	}
	if !removed {
		return RecallDetachResult{RecallAttachment: before, LeftAlone: RecallEntryChanged}, nil
	}
	if rec.CreatedFile {
		if err := removeIfEmpty(rec.ConfigPath); err != nil {
			return RecallDetachResult{RecallAttachment: before, Removed: true}, fmt.Errorf("the entry was removed, but the settings file the attach created could not be: %w", err)
		}
	}
	if err := forgetAttachment(runtime); err != nil {
		return RecallDetachResult{RecallAttachment: before, Removed: true}, fmt.Errorf("the entry was removed, but its record was not: %w", err)
	}
	after, err := RuntimeRecallAttachment(runtime)
	return RecallDetachResult{RecallAttachment: after, Removed: true}, err
}

// removeIfEmpty removes a settings file that holds nothing but white space. A file
// with anything else in it — something was added since the attach created it — stays.
func removeIfEmpty(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) != "" {
		return nil
	}
	return os.Remove(path)
}

// RecallObserved reports how many of a runtime's most recent sessions were checked
// and in how many the memory hook was OBSERVED injecting: a nonce a real `memory
// index` emission logged, found in the runtime's injected-context position (the
// doctor's proof). A settings entry proves neither — Codex skips an untrusted hook
// with no message.
func RecallObserved(runtime string, recent int) (checked, injected int) {
	nonces := loggedNonces(memoryDir())
	sessions := ListSessions(runtime)
	if recent > 0 && len(sessions) > recent {
		sessions = sessions[:recent]
	}
	for _, session := range sessions {
		if sessionHasInjectedNonce(session, nonces) {
			injected++
		}
	}
	return len(sessions), injected
}
