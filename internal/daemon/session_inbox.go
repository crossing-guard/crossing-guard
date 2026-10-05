package daemon

// Generic inbox resolution (session-message-layer plan §5.1): dispatch one
// canonical session identity to the runtime's own resolver and refuse
// everything the resolver does not attest. This file names no runtime and
// reads no vendor registry: the resolver port carries the vendor's facts and
// this file owns the runtime-neutral rules — one exact identity, a liveness
// cross-check that fails closed on a recycled PID, and refusal to use a path
// the resolver did not attest. Adding a runtime with an inbox is one vendor
// resolver file plus its tests; nothing here changes.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// resolveSessionInbox resolves one identity through the runtime's resolver
// port. The runtime names no inbox resolver: the answer is a result, never an
// error to retry around.
func resolveSessionInbox(identity SessionIdentity) (sessionInbox, bool, string) {
	driver, ok := chatDrivers[identity.Runtime]
	if !ok {
		return sessionInbox{}, false, fmt.Sprintf("runtime %q is not registered", identity.Runtime)
	}
	resolver, ok := driver.(sessionInboxResolver)
	if !ok {
		return sessionInbox{}, false, fmt.Sprintf("runtime %q publishes no inbox resolver", identity.Runtime)
	}
	inbox, usable, reason := resolver.ResolveSessionInbox(identity)
	if !usable {
		return sessionInbox{}, false, reason
	}
	if inbox.SocketPath == "" {
		return sessionInbox{}, false, "resolver returned no socket path"
	}
	// Liveness cross-check (delivery-design proof obligation 1): a recycled
	// PID whose process start no longer matches the registry row is stale,
	// never a stranger to deliver to. A row without a PID, or without a
	// recorded process start, cannot prove the PID is the session it names
	// and is refused: the vendor registry always carries both, so their
	// absence is a defect, not a shape to trust.
	if !processStartTimeMatches(inbox.RegistryPID, inbox.ProcStart) {
		return sessionInbox{}, false, "stale-registry"
	}
	return inbox, true, ""
}

// processStartTimeMatches reads the live process's start time rendered the way
// the vendor registry records it (UTC) and compares the two strings. A missing
// process, an unreadable start, or any mismatch fails closed: the PID may have
// been recycled, and a stranger must never receive a session's message.
func processStartTimeMatches(pid int, recordedProcStart string) bool {
	if pid <= 0 || recordedProcStart == "" {
		return false
	}
	cmd := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	rendered := strings.TrimSpace(string(out))
	if rendered == "" {
		return false
	}
	return rendered == strings.TrimSpace(recordedProcStart)
}
