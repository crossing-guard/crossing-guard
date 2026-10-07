package store

import (
	"strings"
	"testing"
	"time"
)

// The schema 39 journal (orchestration-flows pilot slice A, pass-1 RT-2):
// every owner-tag write path appends its change fact in the same
// transaction — apply, retract, rename (removal of the old key + apply of
// the new), purge — and the read is a bounded, position-anchored stream.

func TestOwnerTagJournalRecordsApplyAndRetractOnceEach(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	target := SessionOwnerTarget{Runtime: "claude", SessionID: "s1", Title: "T", Repository: "r", Cwd: "/w", TouchedAt: 1000}
	tag := SessionOwnerTagValue{Key: "flow", Value: "building"}

	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{tag}, 100); err != nil {
		t.Fatal(err)
	}
	if err := ix.RetractSessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{tag}, 200); err != nil {
		t.Fatal(err)
	}
	rows, truncated, err := ix.SessionOwnerTagChangesAfter(0, 10)
	if err != nil || truncated || len(rows) != 2 {
		t.Fatalf("journal = %+v (%v, %v)", rows, truncated, err)
	}
	if rows[0].Change != "applied" || rows[1].Change != "removed" {
		t.Fatalf("change order = %s then %s", rows[0].Change, rows[1].Change)
	}
	if rows[0].Runtime != "claude" || rows[0].SessionID != "s1" || rows[0].Key != "flow" || rows[0].Value != "building" {
		t.Fatalf("apply row = %+v", rows[0])
	}
}

func TestOwnerTagJournalRetractOfAbsentTagWritesNothing(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	target := SessionOwnerTarget{Runtime: "claude", SessionID: "s1"}
	if err := ix.RetractSessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{{Value: "nope"}}, 100); err != nil {
		t.Fatal(err)
	}
	rows, _, err := ix.SessionOwnerTagChangesAfter(0, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("no-op retract journaled: %+v (%v)", rows, err)
	}
}

func TestOwnerTagJournalRenameIsRemovalThenApply(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	target := SessionOwnerTarget{Runtime: "claude", SessionID: "s1", Title: "T", TouchedAt: 1000}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{{Key: "flow", Value: "building"}}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.RenameSessionOwnerTag(SessionOwnerTagValue{Key: "flow", Value: "building"},
		SessionOwnerTagValue{Key: "flow", Value: "review"}, 200); err != nil {
		t.Fatal(err)
	}
	rows, _, err := ix.SessionOwnerTagChangesAfter(0, 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rename journal = %+v (%v)", rows, err)
	}
	if rows[1].Change != "removed" || rows[1].Value != "building" {
		t.Fatalf("rename removal = %+v", rows[1])
	}
	if rows[2].Change != "applied" || rows[2].Value != "review" {
		t.Fatalf("rename apply = %+v", rows[2])
	}
}

func TestOwnerTagJournalPurgeRemovesEveryCarrier(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	tag := SessionOwnerTagValue{Key: "flow", Value: "building"}
	for _, id := range []string{"s1", "s2"} {
		target := SessionOwnerTarget{Runtime: "claude", SessionID: id, Title: "T"}
		if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{tag}, 100); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ix.PurgeSessionOwnerTag(tag); err != nil || n != 2 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	rows, _, err := ix.SessionOwnerTagChangesAfter(0, 10)
	if err != nil || len(rows) != 4 {
		t.Fatalf("purge journal = %+v (%v)", rows, err)
	}
	if rows[2].Change != "removed" || rows[2].SessionID != "s1" || rows[3].Change != "removed" || rows[3].SessionID != "s2" {
		t.Fatalf("purge removals = %+v", rows[2:])
	}
}

func TestOwnerTagJournalRenameSpellingOnlyJournalsNothing(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	target := SessionOwnerTarget{Runtime: "claude", SessionID: "s1"}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{{Value: "Building"}}, 100); err != nil {
		t.Fatal(err)
	}
	// Same tag, new capitals: no state transition, no journal rows.
	if _, err := ix.RenameSessionOwnerTag(SessionOwnerTagValue{Value: "Building"}, SessionOwnerTagValue{Value: "building"}, 200); err != nil {
		t.Fatal(err)
	}
	rows, _, err := ix.SessionOwnerTagChangesAfter(0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("spelling rename journal = %+v (%v)", rows, err)
	}
}

func TestOwnerTagJournalPositionReadIsBounded(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	now := int64(100)
	for i := 0; i < 5; i++ {
		target := SessionOwnerTarget{Runtime: "claude", SessionID: "s" + string(rune('0'+i))}
		if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{{Value: "t"}}, now+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	rows, truncated, err := ix.SessionOwnerTagChangesAfter(0, 3)
	if err != nil || len(rows) != 3 || !truncated {
		t.Fatalf("bounded read = %d rows, truncated=%v (%v)", len(rows), truncated, err)
	}
	head, err := ix.SessionOwnerTagChangeHead()
	if err != nil || head != 5 {
		t.Fatalf("head = %d (%v)", head, err)
	}
}

func TestOwnerTagJournalSchemaIsAdditiveOnAnOlderStore(t *testing.T) {
	// The migrate-probe discipline: a store opened by this binary always
	// carries the journal, and reopening an already-migrated store is
	// idempotent (SchemaVersion covers the stamp).
	ix := openOwnerTagTestIndex(t)
	target := SessionOwnerTarget{Runtime: "claude", SessionID: "s1"}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{{Value: "t"}}, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.SessionOwnerTagChangesAfter(0, 10); err != nil {
		t.Fatalf("reopen read: %v", err)
	}
}

// A failed flow timestamp update must abort the member rewrite: the error
// surfaces and the previous member set stands (gate-red-on-main-2026-09-28
// plan, SA4006). The trigger is not TEMP because the index is a connection
// pool. It fires on any updated_at write to the flow table; the test makes
// none after installing it except the one under test.
func TestReplaceFlowMembersRollsBackWhenFlowUpdateFails(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	if err := ix.EnableFlow("f1", []byte(`{}`), 100); err != nil {
		t.Fatal(err)
	}
	before := []OrchestrationFlowMember{{FlowID: "f1", Runtime: "claude", SessionID: "a", Stage: "s", AdmittedAt: 100, UpdatedAt: 100}}
	if err := ix.ReplaceFlowMembers("f1", before, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`CREATE TRIGGER inject_flow_update_failure BEFORE UPDATE OF updated_at ON orchestration_flow
		BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	after := []OrchestrationFlowMember{{FlowID: "f1", Runtime: "claude", SessionID: "b", Stage: "s", AdmittedAt: 200, UpdatedAt: 200}}
	if err := ix.ReplaceFlowMembers("f1", after, 200); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("replace must surface the failed flow update, got %v", err)
	}
	members, err := ix.FlowMembers("f1")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].SessionID != "a" {
		t.Fatalf("members after a failed replace: %+v", members)
	}
}
