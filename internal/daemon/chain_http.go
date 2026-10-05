package daemon

// chain_http.go — GET /api/chain/verify?session=<vendor/id>
//
// The one place outside the daemon's RAM that can say whether a session's event chain
// tail is the tail the daemon HELD. A disk-only walk (what the CLI can do on its own)
// proves internal consistency; only this route proves nobody recomputed the chain,
// because the same-user agent cannot reach the held anchor (ADR 0016). The response is
// engine.ChainReport verbatim — a typed contract, and the spans stay separate so a
// restart window is never reported as "verified".

import (
	"net/http"
	"strings"
)

func handleChainVerify(w http.ResponseWriter, r *http.Request) {
	session := strings.TrimSpace(r.URL.Query().Get("session"))
	if session == "" {
		http.Error(w, "session is required (vendor/id)", http.StatusBadRequest)
		return
	}
	if governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	report, err := governor.ChainVerify(session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, report)
}
