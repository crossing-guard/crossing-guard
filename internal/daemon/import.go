package daemon

// Phase 4 — the retrospective importer (governance plan). Past vendor sessions become
// analyzable in the SAME model, WITHOUT driving the design: it is second-class and
// lossy by construction, and everything it writes is stamped origin=imported so it is
// never confused with live truth.
//
// Declared lossiness (the plan's own list, made concrete):
//   - It CANNOT see blocked actions. A denied tool call never reached the vendor
//     transcript, so import has no row for it — imported events carry no decision.
//   - It lags real time and is best-effort ordered (folded by each event's timestamp;
//     an event we cannot timestamp is skipped, not guessed).
//   - It reconstructs only what the transcript kept: the tool NAME and a text blob, not
//     the structured file_path/url the live hook sends. So imported events light up
//     tool/skill/mcp/command-text detectors, but not path/url resource entities. This
//     is the honest ceiling of reading history back, stated rather than hidden.
//
// It shares THE ONE normalizer (R7) with the live path — Observe(Origin:"imported") —
// so live and replayed state cannot diverge on how an action is classified.

import (
	"fmt"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// ImportResult is the summary of one import run.
type ImportResult struct {
	SessionsScanned  int `json:"sessions_scanned"`
	SessionsImported int `json:"sessions_imported"`
	SessionsSkipped  int `json:"sessions_skipped"` // already captured LIVE — never clobbered
	EventsImported   int `json:"events_imported"`
	EventsUndated    int `json:"events_undated"` // no parseable timestamp → skipped
	Errors           int `json:"errors"`
}

// ImportSessions backfills past vendor sessions into the event log as imported events.
// Filters: vendor ("" = all), project substring ("" = all), limit (0 = all). It opens
// its OWN store handle (the daemon reads the store live under WAL, so imported rows are
// visible immediately) and builds a Governor over the layered detectors.
func ImportSessions(dataDir, vendor, project string, limit int) (ImportResult, error) {
	var res ImportResult
	// The SAME store + layered detector library the live governor uses (R7), resolved
	// identically — so imported events classify exactly as they would live, INCLUDING a
	// user's detector overlay.
	ix, dets, err := openGovernedStore(dataDir)
	if err != nil {
		return res, err
	}
	defer ix.Close()
	g := NewGovernor(ix, dets)

	sessions := harvest.ScanSessions()
	for _, s := range sessions {
		if vendor != "" && s.Runtime != vendor {
			continue
		}
		if project != "" && !containsFold(s.Project, project) {
			continue
		}
		res.SessionsScanned++
		if limit > 0 && res.SessionsImported >= limit {
			break
		}
		n, undated, err := importOneSession(g, ix, s, &res)
		switch {
		case err != nil:
			res.Errors++
		case n < 0:
			res.SessionsSkipped++ // had live events — left alone
		default:
			res.SessionsImported++
			res.EventsImported += n
			res.EventsUndated += undated
		}
	}
	return res, nil
}

// importOneSession imports a single session. Returns (events, undated, err); a return
// of (-1, 0, nil) means the session was SKIPPED because it already has live events.
func importOneSession(g *Governor, ix *store.Index, s harvest.SessionSummary, res *ImportResult) (int, int, error) {
	sid := harvest.CanonicalID(s)

	// Never clobber live truth: if we already captured this session as it happened,
	// the import has nothing better to offer and must not touch it.
	live, err := ix.CountEventsByOrigin(sid, "live")
	if err != nil {
		return 0, 0, err
	}
	if live > 0 {
		return -1, 0, nil
	}
	// Re-import replaces: clear this session's prior imported rows so a second run does
	// not duplicate them (only origin=imported is touched).
	if _, err := ix.DeleteImportedForSession(sid); err != nil {
		return 0, 0, err
	}

	events, _, _, err := harvest.NormalizeSummary(s, false)
	if err != nil {
		return 0, 0, err
	}
	imported, undated := 0, 0
	for _, ev := range events {
		if ev.Kind != "tool_call" || ev.Name == "" {
			continue
		}
		ts, ok := parseEventTime(ev.Ts)
		if !ok {
			undated++ // an event we cannot place in time is not guessed at
			continue
		}
		// Text is the ONLY channel history kept; feed it as the command so command/skill/
		// mcp/workflow detectors fire. No structured file_path/url — declared lossiness.
		o := Observation{SessionID: sid, Tool: ev.Name, Command: ev.Text, TS: ts, Origin: "imported"}
		if err := g.Observe(o); err != nil {
			return imported, undated, err
		}
		imported++
	}
	return imported, undated, nil
}

// parseEventTime accepts the timestamp shapes the vendors write (RFC3339 with or
// without fractional seconds). Returns ok=false for anything unparseable, so the
// importer skips it rather than folding a fabricated time.
func parseEventTime(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

// containsFold is a case-insensitive substring test for the --project filter.
func containsFold(haystack, needle string) bool {
	return needle == "" || strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// FmtImportResult renders the run for the CLI.
func FmtImportResult(r ImportResult) string {
	return fmt.Sprintf(
		"imported %d events from %d sessions (scanned %d, skipped %d already-live, %d undated events, %d errors)",
		r.EventsImported, r.SessionsImported, r.SessionsScanned, r.SessionsSkipped, r.EventsUndated, r.Errors)
}
