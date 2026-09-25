package daemon

// Finding the console was the product's worst break: the tokenized URL is printed
// to stdout, which under launchd goes to a log file nobody was told to read. So a
// user who installed successfully could not open the thing they installed.
//
// Locating it is NOT "read $data/api-token". There are two data-dir resolvers —
// cmd/crossing-guard's dataDir() (~/.crossing-guard) and the daemon's own
// defaultDataDir() (~/.crossing-guard/console) — and on a machine that has run
// both, BOTH directories hold an api-token and a daemon-addr. Picking one is
// wrong half the time, and its failure mode is the bad kind: a plausible URL
// that 401s, or points at a port nothing is listening on.
//
// The launchd plist is the daemon OF RECORD: it pins --data and --addr
// explicitly, so it answers which store the running daemon actually uses.
// scripts/reload.sh already reads it for the same reason ("anything else is a
// guess, and a guess here rebuilds a binary nobody runs").

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Location is where the console is, plus HOW we concluded that — the source is
// reported to the user because "which of your two data dirs am I reading" is
// exactly the question a stale token raises.
type Location struct {
	Addr    string // host:port the daemon listens on
	DataDir string // the store this daemon was started against
	Token   string // bearer token, read from <DataDir>/api-token
	Source  string // how DataDir/Addr were resolved, in human words
}

// URL is the console address with the token in the fragment. The token rides in
// the FRAGMENT, not the query, so it is never sent to the server in a request
// line and never lands in an access log.
func (l Location) URL() string { return "http://" + l.Addr + "/#t=" + l.Token }

// defaultAddr is the compiled listen default. main.go's --addr flag references it,
// so the two spellings of one decision cannot drift.
const defaultAddr = "127.0.0.1:7777"

// probeTimeout bounds the liveness probe. Short on purpose: this runs on
// interactive paths (console, doctor, init's closing report) where a hung probe
// reads as a hung command.
const probeTimeout = 2 * time.Second

// errAsTimeout reports whether a client error is a timeout (as opposed to a
// refused connection), with a short label for the diagnosis.
func errAsTimeout(err error) (string, bool) {
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out", true
	}
	return "", false
}

// LocateConsole resolves the running console, preferring the daemon of record.
// It reports what it found; it does NOT decide whether the daemon is alive —
// that is ProbeConsole's job, and the two are separate because a file on disk
// has never been evidence that a process is running (see ProbeConsole).
// ServiceTarget reports the data directory and address the installed service is
// configured for, so a take-over preserves the machine's own configuration instead
// of silently relocating its store. Falls back to this package's compiled defaults
// when no service is installed, which is the first-install case.
func ServiceTarget() (dataDir, addr string) {
	if home, err := os.UserHomeDir(); err == nil {
		plist := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		if data, listen, ok := serviceArgs(plist); ok {
			if listen == "" {
				listen = defaultAddr
			}
			return data, listen
		}
	}
	return defaultDataDir(), defaultAddr
}

func LocateConsole() (Location, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Location{}, err
	}
	// 1. The service definition, if one is installed.
	plist := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	if data, addr, ok := serviceArgs(plist); ok {
		loc := Location{Addr: addr, DataDir: data, Source: "the installed service definition (" + plist + ")"}
		if loc.Addr == "" {
			loc.Addr = publishedAddr(loc.DataDir)
		}
		if tok, err := readToken(loc.DataDir); err == nil {
			loc.Token = tok
			return loc, nil
		}
		return loc, noTokenErr(loc.DataDir)
	}
	// 2. No service: fall back to the data dirs a hand-run daemon may have used,
	//    in the order the CLI itself resolves them. First one holding a token wins,
	//    and we say which — so a stale second copy is visible rather than silent.
	//    The second entry is THIS package's own default resolver, not a re-spelled
	//    literal: this file exists because two copies of path policy diverged, and
	//    a third hardcoded copy here would be the same bug queued up again.
	for _, dir := range []string{
		filepath.Join(home, ".crossing-guard"),
		defaultDataDir(),
	} {
		tok, err := readToken(dir)
		if err != nil {
			continue
		}
		return Location{Addr: publishedAddr(dir), DataDir: dir, Token: tok,
			Source: "no service installed — found a token in " + dir}, nil
	}
	return Location{}, errors.New("no console found: no service is installed and no api-token exists.\n" +
		"The daemon has not run yet — start it with: crossing-guard serve")
}

// noTokenErr builds the missing-token diagnosis. (A function, named like one — the
// earlier `errNoToken` read as a sentinel and invited a useless errors.Is reflex.)
func noTokenErr(dir string) error {
	return errors.New("the service is installed but no api-token exists at " +
		filepath.Join(dir, "api-token") + " — the daemon has not completed a startup.\n" +
		"Check the log: " + filepath.Join(dir, "daemon.log"))
}

// ProbeConsole answers the only question that matters — is anything actually
// listening, and does it accept this token — by ASKING. File presence is not a
// liveness signal in either direction: daemon-addr is removed by a deferred
// call, so a crash, a SIGKILL or a launchctl bootout leaves a stale one behind.
// That is not hypothetical; a dead 127.0.0.1:7805 addr file outlived its daemon
// by five days on the author's machine and is what motivated this function.
//
// The returned string is the honest diagnosis for a human, empty when healthy.
// ProbeConsole depends on the health handler's nil-governor contract: that state is
// HTTP 200 with configured:false, not 503. A daemon without live capture is degraded
// but reachable; treating it as dead sends the user toward starting a second daemon.
func ProbeConsole(l Location) string {
	req, err := http.NewRequest("GET", "http://"+l.Addr+"/api/govern/health", nil)
	if err != nil {
		return "cannot form a request for " + l.Addr + ": " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
	if err != nil {
		// A timeout is not a refusal: something may be listening and wedged, and the
		// confident "not running — start it" wording invited starting a SECOND daemon
		// against the same store. Only a real connection error earns that advice.
		if netErr, ok := errAsTimeout(err); ok {
			return "the daemon on " + l.Addr + " did not answer within " + probeTimeout.String() +
				" (" + netErr + ") — it may be up but wedged. Check the log before starting another: " +
				filepath.Join(l.DataDir, "daemon.log")
		}
		return "nothing is listening on " + l.Addr + " — the daemon is not running.\n" +
			"Start it with: crossing-guard serve"
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "a daemon is listening on " + l.Addr + " but it REJECTS the token in " +
			filepath.Join(l.DataDir, "api-token") + ".\n" +
			"That token is stale: the running daemon was started against a different data dir.\n" +
			"Check which one: launchctl print gui/$(id -u)/" + launchdLabel + " | grep -A4 arguments"
	case resp.StatusCode != http.StatusOK:
		return "the daemon on " + l.Addr + " answered " + resp.Status + " — it is up but unhealthy.\n" +
			"Check the log: " + filepath.Join(l.DataDir, "daemon.log")
	}
	return ""
}

var (
	plistString  = regexp.MustCompile(`<string>([^<]*)</string>`)
	plistComment = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// serviceArgs extracts --data and --addr from an installed launchd plist,
// tolerant of whitespace and ordering. The honest contract: it reads plists WE
// wrote (launchdPlist escapes on write, xmlUnescape below reverses it, so the
// round trip is exact — including a path containing `&`) plus light hand edits.
// It strips XML comments first: a commented-out old --data pair read as live
// config is worse than a parse failure.
func serviceArgs(plistPath string) (dataDir, addr string, ok bool) {
	dataDir, addr, _, ok = serviceArgsOwner(plistPath)
	return dataDir, addr, ok
}

// serviceArgsOwner adds the one fact serviceArgs never needed and the ownership
// guard cannot work without: ProgramArguments[0], the binary launchd would run.
// It is the read half of launchdPlist's round trip, so the guard reuses this
// parser rather than growing a second one.
func serviceArgsOwner(plistPath string) (dataDir, addr, owner string, ok bool) {
	b, err := os.ReadFile(plistPath)
	if err != nil {
		return "", "", "", false
	}
	body := plistComment.ReplaceAllString(string(b), "")
	i := strings.Index(body, "<key>ProgramArguments</key>")
	if i < 0 {
		return "", "", "", false
	}
	args := body[i:]
	if end := strings.Index(args, "</array>"); end > 0 {
		args = args[:end]
	}
	var vals []string
	for _, m := range plistString.FindAllStringSubmatch(args, -1) {
		vals = append(vals, xmlUnescape(m[1]))
	}
	if len(vals) != 0 {
		owner = vals[0]
	}
	for i, v := range vals {
		if i+1 >= len(vals) {
			break
		}
		switch v {
		case "--data":
			dataDir = vals[i+1]
		case "--addr":
			addr = vals[i+1]
		}
	}
	// A plist with no --data is one WE did not write; it still tells us a service
	// exists, but not where its store is, so it is not an answer.
	return dataDir, addr, owner, dataDir != ""
}

// xmlUnescape reverses service.go's xmlEscape — the read half of the plist round
// trip. &amp; is deliberately LAST so an escaped ampersand cannot be re-expanded
// into a second entity (the classic double-decode bug, in reverse).
var xmlUnescaper = strings.NewReplacer(
	"&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&")

func xmlUnescape(s string) string { return xmlUnescaper.Replace(s) }

// GetJSON performs an authenticated GET against this located daemon and decodes
// the response. The one client for every CLI verb that talks to the daemon —
// doctor, verify and any successor. It exists because the contract (scheme, token
// header, timeout, status check) had been hand-built three times, twice in the
// same package, and a drift in any copy fails silently as a wrong diagnosis.
func (l Location) GetJSON(path string, v any) error {
	req, err := http.NewRequest("GET", "http://"+l.Addr+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	resp, err := (&http.Client{Timeout: clientTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New(path + ": " + resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// clientTimeout bounds ordinary API reads (GetJSON) — roomier than the liveness
// probe, since these run after ProbeConsole has already vouched for the daemon.
const clientTimeout = 5 * time.Second

// publishedAddr reads the address the daemon publishes for hooks. Absence is not
// an error here — it only means we fall back to the compiled default, and
// ProbeConsole is what decides whether anything is actually there.
func publishedAddr(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "daemon-addr"))
	if err != nil {
		return defaultAddr
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s
	}
	return defaultAddr
}

func readToken(dataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "api-token"))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("empty token file")
	}
	return tok, nil
}
