package store

import "fmt"

// handoffDDL is the device's copy of a handoff (plan §6.7). The row is keyed
// (organization_id, id); organization_id is ” for a local, same-device handoff.
// sent_here, to_me and from_me are three facts, not one direction: a self-send's
// originating device has sent_here and to_me; the sender's other device has from_me
// alone. peer_* is the other party — the recipient on a row this user sent, the sender
// on a row addressed to this user. wire_body is the frozen document (blank for a
// sender's other device, for a landed expiry or withdrawal this device never held, and
// after a withdrawal blanks an unopened copy); held says this device held the document
// at some time, which tells an erased copy from a handoff that never reached it. state_seq is the server's handoff
// sequence the state was landed at; a stale page never lowers it. refusal_code is why
// a send was refused; receipt_code is the code of the last receipt of this device the
// server rejected. link_ended marks rows of a link that ended.
const handoffDDL = `
CREATE TABLE IF NOT EXISTS handoff(
  organization_id TEXT NOT NULL DEFAULT '',
  id TEXT NOT NULL,
  sent_here INTEGER NOT NULL DEFAULT 0 CHECK(sent_here IN (0,1)),
  to_me INTEGER NOT NULL DEFAULT 0 CHECK(to_me IN (0,1)),
  from_me INTEGER NOT NULL DEFAULT 0 CHECK(from_me IN (0,1)),
  peer_user_id TEXT NOT NULL DEFAULT '',
  peer_name TEXT NOT NULL DEFAULT '',
  sender_device_id TEXT NOT NULL DEFAULT '',
  source_runtime TEXT NOT NULL DEFAULT '',
  source_native_id TEXT NOT NULL DEFAULT '',
  source_catalog_id TEXT NOT NULL DEFAULT '',
  source_resume_id TEXT NOT NULL DEFAULT '',
  source_wire_session TEXT NOT NULL DEFAULT '',
  repository_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  wire_body TEXT NOT NULL DEFAULT '',
  wire_hash TEXT NOT NULL DEFAULT '',
  held INTEGER NOT NULL DEFAULT 0 CHECK(held IN (0,1)),
  state TEXT NOT NULL,
  state_seq INTEGER NOT NULL DEFAULT 0,
  state_at INTEGER NOT NULL DEFAULT 0,
  refusal_code TEXT NOT NULL DEFAULT '',
  receipt_code TEXT NOT NULL DEFAULT '',
  opened_json TEXT NOT NULL DEFAULT '',
  other_opened_count INTEGER NOT NULL DEFAULT 0,
  hash_conflicts INTEGER NOT NULL DEFAULT 0,
  link_ended INTEGER NOT NULL DEFAULT 0 CHECK(link_ended IN (0,1)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(organization_id, id)
);
CREATE INDEX IF NOT EXISTS handoff_listing ON handoff(organization_id, to_me, sent_here, updated_at);

CREATE TABLE IF NOT EXISTS team_member(
  organization_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  is_self INTEGER NOT NULL DEFAULT 0 CHECK(is_self IN (0,1)),
  refreshed_at INTEGER NOT NULL,
  PRIMARY KEY(organization_id, user_id)
);`

func migrateHandoffV46(db schemaDB) error {
	if _, err := db.Exec(handoffDDL); err != nil {
		return fmt.Errorf("migrate v46: handoff: %w", err)
	}
	return nil
}
