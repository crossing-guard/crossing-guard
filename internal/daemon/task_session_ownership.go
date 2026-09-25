package daemon

// Session ownership at task admission (in-turn-progress plan, Part A). A
// console turn into a session that another process is using right now would
// append a second, divergent context to the same transcript, and the other
// process would never see the reply. Two inputs, stated plainly:
//
//   - the status frame, folded ON DEMAND for the target session (so a
//     session outside the rail's cap is judged too): a turn open under
//     observed (hook) authority is the mid-turn case this rule exists for;
//   - the presence sampler's snapshot, which IS the capped rail picture
//     (at most max_rail_sessions, refreshed on the sampler interval): a
//     session it reads as open with a recent hook, under an authority that
//     is not ours, is in use even between turns. A session the sampler has
//     not reached is judged on the fold alone.
//
// Refused unless the request carries the person's explicit override.

import (
	"errors"
	"time"
)

// ErrSessionInUse is the typed refusal; its text is the one sentence the
// console shows, so it names the observed fact and its consequence, never a
// place we cannot observe.
var ErrSessionInUse = errors.New("another process is using this session right now; a reply sent from here will not reach it")

// taskSessionInUse is the seam tests replace; production is sessionInUseElsewhere.
var taskSessionInUse = sessionInUseElsewhere

// sessionInUseElsewhere answers "is someone other than us mid-turn or live in
// this session" from the two inputs above. Presence counts only on hook
// liveness: a held file handle alone (an idle vendor at its prompt, an editor)
// does not support "using it right now". A frame we cannot fold is reported,
// not guessed at.
func sessionInUseElsewhere(now time.Time, runtime, catalogID, nativeID string) (bool, error) {
	if catalogID == "" && nativeID == "" {
		return false, nil // a brand-new session has no other process yet
	}
	if catalogID == "" {
		catalogID = nativeID
	}
	if nativeID == "" {
		nativeID = catalogID
	}
	frame, err := foldSessionStatus(now, runtime, catalogID, nativeID)
	if err != nil {
		return false, err
	}
	if frame.Execution == "running" && frame.Authority == "observed" {
		return true, nil
	}
	service := sessionActivityService()
	if service == nil {
		return false, nil
	}
	for _, item := range service.Snapshot().Items {
		if item.Runtime != runtime || (item.CatalogSessionID != catalogID && item.NativeSessionID != nativeID) {
			continue
		}
		if item.Presence == "open" && item.Freshness == "live" && item.Evidence == "hook_liveness" && item.Authority != "owned" {
			return true, nil
		}
	}
	return false, nil
}
