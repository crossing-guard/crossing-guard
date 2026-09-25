package guardcli

// chain.go — `crossing-guard chain verify <vendor/id>`: is this session's event log the
// one that was recorded?
//
// It asks the running daemon first, because only the daemon can compare the chain's
// tail against the anchor it HOLDS in RAM — an anchor you can read from disk, you can
// forge (ADR 0016). With no daemon reachable it walks the store itself and says plainly
// that the result is internal consistency only. Exit 2 on fork/gap so scripts can act.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"crossing-guard/engine"
	"crossing-guard/store"
)

func cmdChain(args []string) {
	if len(args) < 2 || args[0] != "verify" {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard chain verify <vendor/id>")
		os.Exit(1)
	}
	report, source, err := chainReport(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "chain verify:", err)
		os.Exit(1)
	}
	printChainReport(report, source)
	if report.Status == "fork" || report.Status == "gap" {
		os.Exit(2)
	}
}

// chainReport prefers the daemon (held anchor) and falls back to the store (disk only).
// The second return names which one answered, and the fallback is labeled as weaker.
func chainReport(session string) (engine.ChainReport, string, error) {
	if addr, token, ok := daemonEndpoint(); ok {
		req, err := http.NewRequest(http.MethodGet,
			"http://"+addr+"/api/chain/verify?session="+url.QueryEscape(session), nil)
		if err == nil {
			if token != "" {
				req.Header.Set("X-CG-Token", token)
			}
			resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
			if err == nil {
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode == http.StatusOK {
					var r engine.ChainReport
					if err := json.NewDecoder(resp.Body).Decode(&r); err == nil {
						return r, daemonSourceLabel(r), nil
					}
				}
			}
		}
	}
	ix, err := store.OpenRO(store.IndexPath(dataDir(), ""))
	if err != nil {
		return engine.ChainReport{}, "", err
	}
	defer ix.Close()
	if have, behind, err := ix.SchemaOnDisk(); err == nil && behind {
		return engine.ChainReport{}, "", fmt.Errorf("store schema %d is behind this binary's %d and a read-only command cannot migrate it; start the daemon (crossing-guard serve) or run any read-write command, then retry", have, store.SchemaVersion)
	}
	r, err := ix.VerifyEventChain(session, nil)
	return r, "store only (no daemon reachable — internal consistency; the tail was not held)", err
}

func printChainReport(r engine.ChainReport, source string) {
	fmt.Printf("session:  %s\n", r.Session)
	fmt.Printf("status:   %s\n", r.Status)
	fmt.Printf("rows:     %d (chained %d, legacy %d)\n", r.Rows, r.Chained, r.Legacy)
	if r.HeldSpan != [2]int64{} {
		fmt.Printf("held:     seq %d–%d\n", r.HeldSpan[0], r.HeldSpan[1])
	}
	if r.DiskSpan != [2]int64{} {
		fmt.Printf("disk:     seq %d–%d (not held)\n", r.DiskSpan[0], r.DiskSpan[1])
	}
	fmt.Printf("detail:   %s\n", r.Detail)
	fmt.Printf("source:   %s\n", source)
}

// daemonSourceLabel says what the daemon's answer actually proves. A daemon that has
// not seen the session since it booted holds no anchor for it (TailMatchesHeld is nil)
// and its walk is internal consistency only — the label must not claim more.
func daemonSourceLabel(r engine.ChainReport) string {
	if r.TailMatchesHeld == nil {
		return "daemon (no anchor held for this session since boot — internal consistency only)"
	}
	return "daemon (tail checked against the held anchor)"
}
