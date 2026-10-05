package platform

import "testing"

// TestPlatformGateOnlyArmsWhereDemonstrated pins the item-8 capability gate and the
// owner's "untested label" decision: stateful enforcement is ready ONLY on a GOOS
// whose service, uninstall, and store-ACL are all demonstrated. macOS qualifies;
// Linux and Windows are explicitly untested and must NOT report ready.
func TestPlatformGateOnlyArmsWhereDemonstrated(t *testing.T) {
	if !For("darwin").StatefulEnforcementReady() {
		t.Error("darwin is the demonstrated platform and must be enforcement-ready")
	}
	for _, goos := range []string{"linux", "windows"} {
		p := For(goos)
		if p.StatefulEnforcementReady() {
			t.Errorf("%s is untested and must NOT report enforcement-ready", goos)
		}
		if p.Note == "" {
			t.Errorf("%s must state WHY it is not demonstrated, not stay silent", goos)
		}
	}
}

// TestUnknownPlatformIsUnsupportedNotAssumedFine: a GOOS with no record is the safe
// default — Unsupported everywhere, never a silent pass.
func TestUnknownPlatformIsUnsupportedNotAssumedFine(t *testing.T) {
	p := For("plan9")
	if p.Service != Unsupported || p.Uninstall != Unsupported || p.StoreACL != Unsupported {
		t.Errorf("an unknown GOOS must be Unsupported across the board, got %+v", p)
	}
	if p.StatefulEnforcementReady() {
		t.Error("an unknown GOOS must never be enforcement-ready")
	}
}

// TestWindowsStoreACLIsHonestlyNotEquivalent pins the D8 sub-edge the plan named:
// chmod 0600 is a no-op on Windows, so the store ACL there is NOT the macOS
// guarantee, and the table must not pretend it is.
func TestWindowsStoreACLIsHonestlyNotEquivalent(t *testing.T) {
	if For("windows").StoreACL == Demonstrated {
		t.Error("windows store ACL is chmod-a-no-op; it must not be marked demonstrated (D8)")
	}
}
