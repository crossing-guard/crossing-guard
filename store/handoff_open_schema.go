package store

import "fmt"

// handoffOpenDDL is the open ticket (plan §6.3, §6.7): one row per console Open. A
// ticket has no expiry column — it ends by claim or by cancellation.
//
// launched_task is the console task Open's first prompt started; launched_native_id is
// the session that task's first session frame named (empty until the frame arrives, and
// for ever when the runtime never started a session). claimed_* is the (runtime, native
// id) of the session that claimed the ticket; the catalog and resume ids are read from
// the store's session row, never joined here. cancel_reason and cancel_detail say why
// a ticket ended without a claim, as data codes. launch_settled_at is when the launched
// turn was seen to have ended, whatever became of the ticket: after it there is nothing
// left to watch the task for.
//
// brief_text is the brief as armed, so a re-arm hands over the same text. brief_due is
// 1 while a delivery is owed (set by the claim, by a compaction of the claimed session
// and by Deliver again; cleared by a confirmation). brief_requested_at is when it was
// last asked for — handoff.brief_wait runs from it. brief_confirmed_at is set once, by
// the transaction that enqueues the one opened receipt. brief_given_up_at is set when
// the wait ran out.
const handoffOpenDDL = `
CREATE TABLE IF NOT EXISTS handoff_open(
  ticket_id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL DEFAULT '',
  handoff_id TEXT NOT NULL,
  runtime TEXT NOT NULL,
  checkout_root TEXT NOT NULL DEFAULT '',
  launched_task TEXT NOT NULL DEFAULT '',
  launched_native_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK(state IN ('waiting','claimed','cancelled')),
  cancel_reason TEXT NOT NULL DEFAULT '',
  cancel_detail TEXT NOT NULL DEFAULT '',
  claimed_runtime TEXT NOT NULL DEFAULT '',
  claimed_native_id TEXT NOT NULL DEFAULT '',
  claimed_transcript TEXT NOT NULL DEFAULT '',
  brief_text TEXT NOT NULL DEFAULT '',
  brief_due INTEGER NOT NULL DEFAULT 0 CHECK(brief_due IN (0,1)),
  brief_requested_at INTEGER NOT NULL DEFAULT 0,
  brief_confirmed_at INTEGER NOT NULL DEFAULT 0,
  brief_given_up_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  claimed_at INTEGER NOT NULL DEFAULT 0,
  ended_at INTEGER NOT NULL DEFAULT 0,
  launch_settled_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS handoff_open_handoff ON handoff_open(organization_id, handoff_id, state);
CREATE INDEX IF NOT EXISTS handoff_open_claimed ON handoff_open(claimed_runtime, claimed_native_id);
CREATE INDEX IF NOT EXISTS handoff_open_task ON handoff_open(launched_task);

CREATE TABLE IF NOT EXISTS handoff_runtime_readiness(
  runtime TEXT PRIMARY KEY,
  unclaimed_launch_at INTEGER NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT ''
);`

func migrateHandoffOpenV46(db schemaDB) error {
	if _, err := db.Exec(handoffOpenDDL); err != nil {
		return fmt.Errorf("migrate v46: handoff_open: %w", err)
	}
	return nil
}
