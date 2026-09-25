package daemon

// Keeping the daemon alive is a PRODUCT REQUIREMENT, not an operator preference:
// without it the software degrades to stateless regex on shell commands — no live
// capture, no session/entity state, no stateful gates, no console. State is the
// product and the daemon owns the state.
//
// So the daemon installs itself as a user service. "Remember to run crossing-guard serve" is the
// same unowned-human-step failure that left the session index hours stale and left
// hooks uninstalled; we do not ship a third instance of it.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"crossing-guard/internal/guardcli"
)

// ServiceStatus mirrors guardcli.HookStatus so the startup report reads the same.
type ServiceStatus struct {
	Action string `json:"action"` // current | installed | unsupported | error
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
}

const launchdLabel = "com.crossing-guard.daemon"

// Ownership is the question the self-healing install never asked: the artifact I am
// about to rewrite — whose is it? Answering it is what stops a scratch build from
// taking the machine's real installation with it.
type Ownership string

const (
	// OwnershipNone: nothing is installed, or nothing legible enough to name an
	// owner. Treated as free to claim, because that is what happens today and a
	// permanent refusal with no repair verb is worse than the risk.
	OwnershipNone Ownership = "none"
	// OwnershipSelf: this executable already owns it. Repair it, as today.
	OwnershipSelf Ownership = "self"
	// OwnershipDead: another executable owns it, but that binary is gone. Nobody
	// owns it any more, so claiming it is how a machine left broken by a deleted
	// scratch build heals itself with no human step.
	OwnershipDead Ownership = "dead"
	// OwnershipForeign: another executable owns it and still exists. Leave it be.
	OwnershipForeign Ownership = "foreign"
)

// serviceOwnership reports who owns the installed launchd service, if anyone. The
// rule itself lives in guardcli so the service and the vendor hooks cannot disagree
// about what "mine" means.
func serviceOwnership(plistPath, self string) Ownership {
	_, _, owner, _ := serviceArgsOwner(plistPath)
	return Ownership(guardcli.OwnershipOf(owner, self))
}

// EnsureService makes this daemon survive logout/reboot. Idempotent: an already
// correct service is left alone. Honest on unsupported platforms rather than
// pretending — a silent no-op here would mean "always-on" was a lie.
// EnsureService is the self-healing install `serve` runs on startup. It refuses to
// rewrite a service another live binary owns; see AdoptService for the deliberate
// take-over a person asks for.
func EnsureService(dataDir, addr string) ServiceStatus {
	return ensureService(dataDir, addr, false)
}

// AdoptService takes ownership of the launchd service unconditionally. This is what
// `crossing-guard init` does, because taking over is the whole reason a person runs
// it — including to repair a machine whose service names a binary that is still
// present but no longer the one they want.
func AdoptService(dataDir, addr string) ServiceStatus {
	return ensureService(dataDir, addr, true)
}

func ensureService(dataDir, addr string, adopt bool) ServiceStatus {
	exe, err := os.Executable()
	if err != nil {
		return ServiceStatus{Action: "error", Detail: "cannot resolve own path: " + err.Error()}
	}
	exe, _ = filepath.Abs(exe)

	if runtime.GOOS != "darwin" {
		return ServiceStatus{Action: "unsupported",
			Detail: runtime.GOOS + ": no service manager wired yet — the daemon must be kept " +
				"running another way, or live capture stops when it exits"}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ServiceStatus{Action: "error", Detail: err.Error()}
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	want := launchdPlist(exe, dataDir, addr, filepath.Join(dataDir, "daemon.log"))

	if cur, err := os.ReadFile(plistPath); err == nil && string(cur) == want {
		return ServiceStatus{Action: "current", Path: plistPath}
	}
	if !adopt {
		if owned := serviceOwnership(plistPath, exe); owned == OwnershipForeign {
			_, _, owner, _ := serviceArgsOwner(plistPath)
			return ServiceStatus{Action: "foreign", Path: plistPath,
				Detail: "this service belongs to " + owner + " and was left alone. " +
					"To take it over, run: crossing-guard init --yes"}
		}
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return ServiceStatus{Action: "error", Path: plistPath, Detail: err.Error()}
	}
	if err := os.WriteFile(plistPath, []byte(want), 0o644); err != nil {
		return ServiceStatus{Action: "error", Path: plistPath, Detail: err.Error()}
	}
	uid := strconv.Itoa(os.Getuid())

	// If launchd is our parent we ARE the managed job, and `bootout` would kill
	// this process mid-call — the bootstrap below would never run, leaving the
	// service unregistered until someone bootstraps it by hand. (That is not
	// hypothetical: it happened the first time the plist content ever changed.)
	// Write the plist and say plainly that it needs a restart to take effect,
	// rather than committing suicide to apply it.
	if os.Getppid() == 1 {
		return ServiceStatus{Action: "updated", Path: plistPath,
			Detail: "service definition updated; the running process still uses the old one. " +
				"Apply with: launchctl bootout gui/" + uid + "/" + launchdLabel +
				" && launchctl bootstrap gui/" + uid + " " + plistPath}
	}

	// Started some other way (hand-run, first install): re-registering is safe
	// because the process we boot out is not this one. Failures are reported,
	// never swallowed — a written-but-unloaded plist would look installed and
	// not survive a reboot.
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).Run() // ok if absent
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput(); err != nil {
		return ServiceStatus{Action: "error", Path: plistPath,
			Detail: fmt.Sprintf("plist written but launchctl bootstrap failed (%v): %s", err, out)}
	}
	return ServiceStatus{Action: "installed", Path: plistPath,
		Detail: "daemon will start at login and be restarted if it exits"}
}

// RemoveService reverses EnsureService: it boots the launchd job out and deletes the
// plist, so the daemon no longer survives logout/reboot. This is half of bar 2 (a user
// can UNINSTALL completely) — a service you can install but not remove is a one-way
// door on someone else's machine (D11). Honest on unsupported platforms rather than
// claiming a removal that did nothing.
//
// It does NOT stop the current process: if we are the managed job, booting ourselves
// out mid-call would kill this process before it could report — the same suicide
// EnsureService avoids. The plist is removed so the job does not come back after this
// process exits; `crossing-guard uninstall` runs from a SEPARATE short-lived process, so it can and
// does boot the running daemon out.
func RemoveService() ServiceStatus {
	if runtime.GOOS != "darwin" {
		return ServiceStatus{Action: "unsupported",
			Detail: runtime.GOOS + ": no service manager wired here — nothing to remove " +
				"(if you kept the daemon alive another way, stop it there)"}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ServiceStatus{Action: "error", Detail: err.Error()}
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	_, statErr := os.Stat(plistPath)
	if os.IsNotExist(statErr) {
		return ServiceStatus{Action: "absent", Path: plistPath,
			Detail: "no launchd plist installed — nothing to remove"}
	}
	// Boot the job out first, so a KeepAlive daemon does not keep running against a
	// deleted plist. Ignore the error: the job may already be stopped, and the plist
	// removal below is what actually prevents it coming back at next login.
	uid := strconv.Itoa(os.Getuid())
	bootout := exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchdLabel).CombinedOutput
	bootoutOut, bootoutErr := bootout()
	if err := os.Remove(plistPath); err != nil {
		return ServiceStatus{Action: "error", Path: plistPath,
			Detail: "could not delete plist: " + err.Error()}
	}
	detail := "service removed; it will not start at next login"
	if bootoutErr != nil {
		// The plist is gone (the durable fix); a bootout failure just means the
		// current process keeps running until it exits. Say so plainly.
		detail += " (the running process was not booted out: " + string(bootoutOut) + ")"
	}
	return ServiceStatus{Action: "removed", Path: plistPath, Detail: detail}
}

// launchdPlist keeps KeepAlive+RunAtLoad: the product needs the daemon ALWAYS up,
// so a crash or logout must not silently end live capture. The listen address is
// PINNED here rather than left to the default: an ad-hoc daemon squatting the
// default port otherwise leaves the managed service in a permanent bind-fail
// retry loop, looking installed while capturing nothing. Hooks are unaffected by
// the port choice — they read the address the daemon publishes.
func launchdPlist(exe, dataDir, addr, logPath string) string {
	// Every interpolated value is XML-escaped. Unescaped, a path containing `&`
	// (~/Q&A/…) writes invalid XML that launchd rejects — leaving the service dead
	// while EnsureService reports "installed", the exact looks-installed-does-
	// nothing failure this file exists to prevent. Escaping on write is also what
	// lets serviceArgs read our own plists back exactly.
	e := xmlEscape
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + e(exe) + `</string>
    <string>serve</string>
    <string>--data</string>
    <string>` + e(dataDir) + `</string>
    <string>--addr</string>
    <string>` + e(addr) + `</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>` + e(guardcli.RuntimeAgentPATH()) + `</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + e(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + e(logPath) + `</string>
</dict>
</plist>
`
}

// xmlEscape covers the five XML metacharacters. strings.NewReplacer rather than
// xml.EscapeText because the latter needs a writer and errors, for what is here a
// pure string-in string-out step.
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }
