package daemon

import (
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// daemonAddrProbeTimeout bounds the one loopback dial that asks whether the daemon a
// published address names is still answering.
const daemonAddrProbeTimeout = 300 * time.Millisecond

// publishDaemonAddr writes this daemon's listen address beside the store so hooks and
// the CLI find it on any port, and returns the function that withdraws it at exit.
//
// A second `serve` on a data directory whose daemon is running must not take the
// address away from the live one: it used to overwrite the file, fail to bind, and
// remove the file on its way out, after which every hook fell back to the default port
// while the first daemon was healthy (found on the journey rig, 2026-10-04). So an
// address that is already published and still answers is left alone, and the file is
// removed at exit only while it still holds what this process wrote.
func publishDaemonAddr(dataDir, addr string) func() {
	addrFile := filepath.Join(dataDir, "daemon-addr")
	if published, err := os.ReadFile(addrFile); err == nil {
		if live := strings.TrimSpace(string(published)); live != "" && daemonAddrAnswers(live) {
			log.Printf("warn: %s already names a daemon that answers at %s; leaving it published", addrFile, live)
			return func() {}
		}
	}
	mine := []byte(addr + "\n")
	if err := os.WriteFile(addrFile, mine, 0o644); err != nil {
		log.Printf("warn: could not publish daemon address (%v) — hooks will not find us", err)
		return func() {}
	}
	return func() {
		if current, err := os.ReadFile(addrFile); err == nil && string(current) == string(mine) {
			_ = os.Remove(addrFile) // best effort: a stale file is re-probed by the next daemon
		}
	}
}

// daemonAddrAnswers reports whether something accepts a connection at addr.
func daemonAddrAnswers(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, daemonAddrProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close() // the probe only needed the accept
	return true
}
