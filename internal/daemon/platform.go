package daemon

// The platform capability gate (item 8 / v1-remaining-work §0 "per-GOOS, not global").
//
// The owner decided (2026-07-20) how V1 handles Linux and Windows: a STATED "untested"
// label, not CI or VMs. So this table is the honest record of what has actually been
// DEMONSTRATED on the running GOOS versus what merely cross-compiles. It is the
// difference between the code saying "supported" and the code being able to prove it.
//
// Three capabilities matter for the deployability bars (§0):
//   - service:   the daemon installs itself to survive logout/reboot (bar 1, always-on)
//   - uninstall: hooks restored, service removed, data purgeable (bar 2, P-INST-3)
//   - storeACL:  the store is not world-readable by an OS mechanism (bar 3, D8)
//
// macOS has all three demonstrated. Linux and Windows are UNTESTED — they cross-compile
// and link, which proves neither. Phase 3 enforcement (item 17) reads this table and
// REFUSES to arm the stateful tier on a GOOS whose capabilities are not demonstrated,
// so "not met on this platform" is a check the binary makes, not a sentence in a doc.

import "runtime"

// CapabilityState is the honest status of one capability on one platform.
type CapabilityState string

const (
	// Demonstrated: exercised on this GOOS and observed to work, not merely compiled.
	Demonstrated CapabilityState = "demonstrated"
	// Untested: the code path exists and links, but has never been run here. A
	// cross-compile is exactly this — "it builds" is not "it works".
	Untested CapabilityState = "untested"
	// Unsupported: no implementation for this GOOS at all.
	Unsupported CapabilityState = "unsupported"
)

// PlatformSupport is the capability record for one GOOS.
type PlatformSupport struct {
	GOOS      string          `json:"goos"`
	Service   CapabilityState `json:"service"`   // self-installing keep-alive service
	Uninstall CapabilityState `json:"uninstall"` // full removal (hooks + service + data)
	StoreACL  CapabilityState `json:"store_acl"` // store not world-readable by an OS mechanism
	// Note explains, for a human, WHY a capability is not demonstrated here — the one
	// sentence a reader needs to decide whether to trust the platform.
	Note string `json:"note,omitempty"`
}

// StatefulEnforcementReady reports whether every capability this platform needs to
// SAFELY run stateful enforcement is demonstrated. The gate item 17 consults: no
// stateful tier on a platform whose uninstall (the way out) is not proven, or whose
// store cannot be locked down.
func (p PlatformSupport) StatefulEnforcementReady() bool {
	return p.Service == Demonstrated && p.Uninstall == Demonstrated && p.StoreACL == Demonstrated
}

// platformTable is keyed by GOOS. Only what has been DEMONSTRATED is recorded as such;
// everything else stays honest. macOS is the demonstrated platform; Linux and Windows
// carry the specific reasons they are not, from the plan's own D18 sub-edges.
var platformTable = map[string]PlatformSupport{
	"darwin": {
		GOOS: "darwin", Service: Demonstrated, Uninstall: Demonstrated, StoreACL: Demonstrated,
	},
	"linux": {
		GOOS: "linux", Service: Untested, Uninstall: Untested, StoreACL: Demonstrated,
		Note: "0600 chmod on the store works on linux (same syscall as darwin), but the " +
			"systemd user-unit service and its uninstall are unbuilt and unrun — untested, not supported.",
	},
	"windows": {
		GOOS: "windows", Service: Untested, Uninstall: Untested, StoreACL: Untested,
		Note: "chmod 0600 is a no-op on Windows (ACLs, not mode bits), so the store ACL is " +
			"NOT equivalent to macOS (D8); the service/uninstall are unbuilt. Untested across the board.",
	},
}

// PlatformSupportFor returns the capability record for a GOOS. An unknown GOOS is
// reported Unsupported across the board rather than assumed-fine — the safe default
// when we have never even considered a platform.
func PlatformSupportFor(goos string) PlatformSupport {
	if p, ok := platformTable[goos]; ok {
		return p
	}
	return PlatformSupport{GOOS: goos, Service: Unsupported, Uninstall: Unsupported,
		StoreACL: Unsupported, Note: "no capability record for this GOOS — never evaluated"}
}

// CurrentPlatformSupport is the record for the GOOS this binary is running on.
func CurrentPlatformSupport() PlatformSupport { return PlatformSupportFor(runtime.GOOS) }
