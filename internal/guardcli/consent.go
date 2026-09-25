package guardcli

// Consent: which runtimes the user actually agreed to have governed.
//
// The daemon re-attaches hooks on every boot, because "remember to run
// crossing-guard install" was an unowned human step that left the system
// silently unhooked. That fix is right and stays. But it also meant the product
// edited the config of a runtime the user had never said yes to — and its own
// design doc forbids exactly that (install-flow-v1 §4: "never silently take over
// an agent"). A governance product that attaches without asking is arguing
// against itself.
//
// The resolution is that REPAIR IS NOT ATTACH:
//   - a vendor in this record may be repaired silently, forever — the user
//     already said yes to this vendor, and keeping their yes true is our job;
//   - a vendor NOT in this record is reported and never edited, no matter how
//     obviously installed it is.
//
// The record is per-vendor, not a global "yes": attaching Claude has never
// implied attaching Codex.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ConsentRecord is the durable answer to "which runtimes did the user say yes
// to". Keyed by vendor name so a new vendor is a new key, never a schema change.
type ConsentRecord struct {
	Vendors map[string]VendorConsent `json:"vendors"`
}

// VendorConsent is one yes: what was agreed, to which file, and when. Config is
// LOAD-BEARING: EnsureHooks repairs the file named here, not the machine default,
// so a yes given for a custom --settings path never authorizes editing another
// file. Binary is evidence only — the hook binary observed (or installed) when
// the yes was recorded — kept for the audit trail, consulted by nothing yet.
type VendorConsent struct {
	Config      string `json:"config"`
	Binary      string `json:"binary"`
	ConsentedAt string `json:"consented_at"`
	// Grandfathered marks a yes we INFERRED rather than one the user gave us: our
	// hook was already in the config when the record was introduced. Recorded as a
	// distinct fact because it is weaker evidence, and a surface that reports
	// consent should be able to say which kind it has.
	Grandfathered bool `json:"grandfathered,omitempty"`
}

func consentPath() string { return filepath.Join(dataDir(), "attached.json") }

// LoadConsent reads the record. A missing file is not an error: it is a machine
// that predates the record (see GrandfatherExistingAttachments) or has never
// been through init.
func LoadConsent() ConsentRecord {
	rec := ConsentRecord{Vendors: map[string]VendorConsent{}}
	b, err := os.ReadFile(consentPath())
	if err != nil {
		return rec
	}
	if json.Unmarshal(b, &rec) != nil || rec.Vendors == nil {
		// A corrupt record must not be read as "consent to everything". Falling back
		// to an EMPTY record fails toward asking again, which is the safe direction
		// for a consent gate.
		return ConsentRecord{Vendors: map[string]VendorConsent{}}
	}
	return rec
}

// RecordConsent stores one vendor's yes. Idempotent; re-consenting refreshes the
// config path and binary without inventing a new timestamp for an old decision.
func RecordConsent(vendor, config, binary string, grandfathered bool) error {
	return withConsentLock(func() error {
		rec := LoadConsent()
		prev, existed := rec.Vendors[vendor]
		at := time.Now().UTC().Format(time.RFC3339)
		if existed && prev.ConsentedAt != "" {
			at = prev.ConsentedAt
		}
		rec.Vendors[vendor] = VendorConsent{Config: config, Binary: binary, ConsentedAt: at,
			Grandfathered: grandfathered && (!existed || prev.Grandfathered)}
		return writeConsent(rec)
	})
}

// ForgetConsent drops one vendor's yes — the uninstall half. A reinstall must
// ask again rather than inherit a decision the user already reversed.
func ForgetConsent(vendor string) error {
	return withConsentLock(func() error {
		rec := LoadConsent()
		if _, ok := rec.Vendors[vendor]; !ok {
			return nil
		}
		delete(rec.Vendors, vendor)
		return writeConsent(rec)
	})
}

func writeConsent(rec ConsentRecord) error {
	if err := os.MkdirAll(filepath.Dir(consentPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	// Temp file + rename, not a truncate-in-place: the daemon (grandfathering at
	// boot) and a user running init can both write this file, and a crash mid-write
	// would leave it corrupt. Corrupt reads as "no consent" (safe direction) — but
	// then the next boot's grandfathering re-mints every explicit yes as INFERRED,
	// silently downgrading the provenance this record exists to preserve. Rename is
	// atomic on one filesystem, which a same-directory temp guarantees.
	tmp, err := os.CreateTemp(filepath.Dir(consentPath()), ".attached-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), consentPath())
}

// IsConsented reports whether this vendor may be attached or repaired without
// asking again.
func IsConsented(vendor string) bool {
	_, ok := LoadConsent().Vendors[vendor]
	return ok
}

// GrandfatherExistingAttachments writes a consent entry for every vendor whose
// config ALREADY carries our hook, and returns the vendors it adopted.
//
// This exists so introducing the consent gate does not silently un-govern a
// working install. Without it, the first daemon restart after this change would
// stop repairing hooks it had been maintaining for weeks — a governance
// regression delivered as a privacy improvement, and exactly the class of
// silent capture loss the store is supposed to make visible.
//
// The inference's actual evidence, stated exactly: a PreToolUse hook whose binary
// is NAMED like ours, invoking our `hook` verb. That is strong but not certain —
// a foreign tool that happens to share the name would be adopted too — which is
// why the entry is marked Grandfathered rather than recorded as a typed yes, and
// why the recorded Binary is the one OBSERVED in the config, never this process's
// own path (the record must state the evidence, not improve on it).
func GrandfatherExistingAttachments() []string {
	var adopted []string
	for _, name := range installerNames() {
		if IsConsented(name) {
			continue
		}
		inst := hookInstallers[name]
		cfg := inst.ResolveConfig()
		if cfg == "" {
			continue
		}
		// HookBinary, not IsCurrent: a hook pointing at an older binary path is still
		// evidence of a past yes, and demanding currency here would refuse to adopt
		// exactly the installs that most need repairing. The vendor answers, because
		// only the vendor knows where its config file is.
		observed := inst.HookBinary(cfg)
		if observed == "" {
			continue
		}
		if RecordConsent(name, cfg, observed, true) == nil {
			adopted = append(adopted, name)
		}
	}
	return adopted
}
