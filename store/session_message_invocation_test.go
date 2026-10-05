package store

import (
	"errors"
	"strings"
	"testing"
)

func invocationFixture(id string) SessionMessageInvocation {
	return SessionMessageInvocation{
		InvocationID: id, CallerRuntime: "codex", CallerNativeID: "thread-a",
		TargetRuntime: "claude", TargetNativeID: "sess-1",
		Message: "wrapped text " + id, State: SessionMessageInvocationPending,
		Digest: "sha256_" + id, CreatedAt: 1000, DeliveryRunID: DeliveryRunPrefix + id,
	}
}

// One record per invocation; the mint-time states are pending and refused
// only (plan C-1); the digest check refuses an identical non-refused record.
func TestMintSessionMessageInvocationLifecycle(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	record := invocationFixture("oinv_1")
	if _, err := ix.MintSessionMessageInvocation(record, nil, 0); err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, found, err := ix.SessionMessageInvocationByID("oinv_1")
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if got.State != SessionMessageInvocationPending || got.Digest != "sha256_oinv_1" {
		t.Fatalf("record %+v", got)
	}
	// D11: the identical digest inside the window is refused, citing the
	// first invocation (postwork PW-6).
	dup := record
	dup.InvocationID = "oinv_2"
	_, mintErr := ix.MintSessionMessageInvocation(dup, nil, 0)
	if !errors.Is(mintErr, ErrInvocationDuplicateDigest) {
		t.Fatalf("duplicate digest must be refused, got %v", err)
	}
	if got := InvocationIDFromDuplicateError(mintErr); got != "oinv_1" {
		t.Fatalf("the duplicate refusal carries the first invocation id: %q", got)
	}
	// A refused record never forms a digest edge: minting one with the same
	// digest is fine after the first was refused.
	refused := record
	refused.InvocationID = "oinv_3"
	refused.State = SessionMessageInvocationRefused
	refused.SettledAt = 1001
	if _, err := ix.MintSessionMessageInvocation(refused, nil, 0); err != nil {
		t.Fatalf("a refusal record carries the digest without blocking: %v", err)
	}
	if _, err := ix.MintSessionMessageInvocation(func() SessionMessageInvocation {
		r := invocationFixture("oinv_4")
		r.State = SessionMessageInvocationAccepted
		return r
	}(), nil, 0); err == nil || !strings.Contains(err.Error(), "pending or refused") {
		t.Fatalf("a minted record must start pending or refused, got %v", err)
	}
}

// Settlement is terminal: only terminal states, only from pending or accepted,
// and a repeat of the same settle is a no-op (the carrier and sweeper race).
func TestSettleSessionMessageInvocationIsTerminal(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	if _, err := ix.MintSessionMessageInvocation(invocationFixture("oinv_1"), nil, 0); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := ix.SettleSessionMessageInvocation("oinv_1", SessionMessageInvocationPending, "", 1001, "x"); err == nil {
		t.Fatal("pending is not terminal")
	}
	if err := ix.SettleSessionMessageInvocation("oinv_1", SessionMessageInvocationDelivered, "", 1001, "the boundary carried it"); err != nil {
		t.Fatalf("settle delivered: %v", err)
	}
	if err := ix.SettleSessionMessageInvocation("oinv_1", SessionMessageInvocationUnknown, "", 1002, "overwrite"); err != nil {
		t.Fatalf("a repeat settle is a no-op, not an error: %v", err)
	}
	got, _, _ := ix.SessionMessageInvocationByID("oinv_1")
	if got.State != SessionMessageInvocationDelivered || got.Detail == "overwrite" {
		t.Fatalf("terminal means terminal: %+v", got)
	}
}

// The carrier's pending row mints in the SAME transaction (RT-8): the cap is
// enforced across both writes, and a released/cap row leaves no invocation.
func TestMintWithPendingRowIsOneTransaction(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	// Fill the target's pending budget: maxPending=1 and one existing row.
	if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: "odel_existing", RunID: "orun_other",
		Runtime: "claude", NativeSessionID: "sess-1", Message: "earlier",
		CreatedAt: 900, ExpiresAt: 5000}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	record := invocationFixture("oinv_cap")
	pending := &SessionDelivery{DeliveryID: "odel_new", RunID: DeliveryRunPrefix + "oinv_cap",
		Runtime: "claude", NativeSessionID: "sess-1", Message: "wrapped text",
		CreatedAt: 1000, ExpiresAt: 5000}
	if _, err := ix.MintSessionMessageInvocation(record, pending, 1); !errors.Is(err, ErrSessionDeliveryCap) {
		t.Fatalf("the cap must refuse the mint, got %v", err)
	}
	if _, found, _ := ix.SessionMessageInvocationByID("oinv_cap"); found {
		t.Fatal("a capped mint must leave no invocation record (one transaction)")
	}
}

// The synthetic run id resolves its invocation and no real run id can match:
// managedID's hex remainder cannot begin "mcp_" (plan RT-10 + confirming pass).
func TestInvocationIDForDeliveryRun(t *testing.T) {
	id, ok := InvocationIDForDeliveryRun(DeliveryRunPrefix + "oinv_1")
	if !ok || id != "oinv_1" {
		t.Fatalf("prefix resolve: %q %v", id, ok)
	}
	if _, ok := InvocationIDForDeliveryRun("orun_deadbeef0123456789abcdef01234567"); ok {
		t.Fatal("a real hex run id must not match the mcp prefix")
	}
	if _, ok := InvocationIDForDeliveryRun("orun_mcp_"); ok {
		t.Fatal("a bare prefix is not a run id")
	}
}

// The sweeper's substrate: stuck pending and stuck accepted, inside the TTL
// window only.
func TestStuckSessionMessageInvocations(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	now := int64(100000)
	ttl := int64(1800)
	if _, err := ix.MintSessionMessageInvocation(func() SessionMessageInvocation {
		r := invocationFixture("oinv_stuck_pending")
		r.CreatedAt = now - ttl - 1
		return r
	}(), nil, 0); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := ix.MintSessionMessageInvocation(func() SessionMessageInvocation {
		r := invocationFixture("oinv_fresh")
		r.CreatedAt = now - 10
		return r
	}(), nil, 0); err != nil {
		t.Fatalf("mint fresh: %v", err)
	}
	// A TERMINAL accepted record is never stuck (postwork PW-2): the receipt
	// was recorded — sweeping it would overwrite a true outcome with a lie.
	if _, err := ix.MintSessionMessageInvocation(func() SessionMessageInvocation {
		r := invocationFixture("oinv_accepted_old")
		r.CreatedAt = now - ttl - 5
		return r
	}(), nil, 0); err != nil {
		t.Fatalf("mint accepted-seed: %v", err)
	}
	if err := ix.SettleSessionMessageInvocation("oinv_accepted_old", SessionMessageInvocationAccepted, "", now-ttl, "transport accepted"); err != nil {
		t.Fatalf("settle accepted: %v", err)
	}
	stuck, err := ix.StuckSessionMessageInvocations(now, ttl, 50)
	if err != nil {
		t.Fatalf("sweep query: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range stuck {
		ids[r.InvocationID] = true
	}
	if !ids["oinv_stuck_pending"] {
		t.Fatalf("stuck set incomplete: %v", ids)
	}
	if ids["oinv_fresh"] || ids["oinv_accepted_old"] {
		t.Fatalf("fresh and terminal-accepted records are not stuck: %v", ids)
	}
}

// Cycle edges: only accepted and delivered records form edges (plan RT-5).
func TestRecentAcceptedEdgesStatesOnly(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	mk := func(id, state string) SessionMessageInvocation {
		r := invocationFixture(id)
		r.State = state
		return r
	}
	seeds := []SessionMessageInvocation{mk("oinv_acc", SessionMessageInvocationPending), mk("oinv_del", SessionMessageInvocationPending)}
	for _, r := range seeds {
		if _, err := ix.MintSessionMessageInvocation(r, nil, 0); err != nil {
			t.Fatalf("mint %s: %v", r.InvocationID, err)
		}
	}
	// The accepted/delivered edges are reached through settlement; the other
	// states are minted directly where the vocabulary allows (refused) or
	// settled into (expired, unknown).
	if err := ix.SettleSessionMessageInvocation("oinv_acc", SessionMessageInvocationAccepted, "", 1001, "transport accepted"); err != nil {
		t.Fatalf("settle accepted: %v", err)
	}
	if err := ix.SettleSessionMessageInvocation("oinv_del", SessionMessageInvocationDelivered, "", 1001, "boundary carried"); err != nil {
		t.Fatalf("settle delivered: %v", err)
	}
	for _, r := range []SessionMessageInvocation{mk("oinv_ref", SessionMessageInvocationRefused)} {
		if _, err := ix.MintSessionMessageInvocation(r, nil, 0); err != nil {
			t.Fatalf("mint %s: %v", r.InvocationID, err)
		}
	}
	if _, err := ix.MintSessionMessageInvocation(mk("oinv_exp", SessionMessageInvocationPending), nil, 0); err != nil {
		t.Fatalf("mint exp: %v", err)
	}
	if err := ix.SettleSessionMessageInvocation("oinv_exp", SessionMessageInvocationExpired, "", 1002, "no boundary"); err != nil {
		t.Fatalf("settle expired: %v", err)
	}
	if _, err := ix.MintSessionMessageInvocation(mk("oinv_unk", SessionMessageInvocationPending), nil, 0); err != nil {
		t.Fatalf("mint unk: %v", err)
	}
	if err := ix.SettleSessionMessageInvocation("oinv_unk", SessionMessageInvocationUnknown, "", 1002, "lost"); err != nil {
		t.Fatalf("settle unknown: %v", err)
	}
	edges, err := ix.RecentAcceptedEdges(0, 200)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	got := map[string]bool{}
	for _, e := range edges {
		got[e.InvocationID] = true
	}
	if !got["oinv_acc"] || !got["oinv_del"] {
		t.Fatalf("accepted/delivered must form edges: %v", got)
	}
	for _, refused := range []string{"oinv_ref", "oinv_exp", "oinv_unk"} {
		if got[refused] {
			t.Fatalf("%s must form no edge", refused)
		}
	}
}

// The carrier row for an already-minted invocation (postwork PW-1) obeys the
// per-target cap in one transaction: over the cap, no row lands.
func TestEnqueueCarrierRowForInvocationCap(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: "odel_existing", RunID: "orun_other",
		Runtime: "claude", NativeSessionID: "sess-1", Message: "earlier",
		CreatedAt: 900, ExpiresAt: 5000}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	row := SessionDelivery{DeliveryID: "odel_new", RunID: DeliveryRunPrefix + "oinv_x",
		Runtime: "claude", NativeSessionID: "sess-1", Message: "wrapped",
		CreatedAt: 1000, ExpiresAt: 5000}
	if err := ix.EnqueueCarrierRowForInvocation(row, 1); !errors.Is(err, ErrSessionDeliveryCap) {
		t.Fatalf("cap: %v", err)
	}
	var n int
	if err := ix.db.QueryRow(`SELECT count(*) FROM session_delivery WHERE delivery_id='odel_new'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a capped enqueue leaves no row: n=%d err=%v", n, err)
	}
	if err := ix.EnqueueCarrierRowForInvocation(row, 2); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
}
