import assert from 'node:assert/strict';
import test from 'node:test';

import { conflictLine, deletionText, promoteAction, refusalText, sentLine, teamMemoryText, teamRecordLines } from './team-memory-text.js';

const quiet = { shared: 2, pulled: 5, weak_repository: 0, not_shareable: 0, conflicts: 0, discarded_edits: 0, shadowed: 0, aliased: 0, held: 0, import_candidates: 0, deletions_refused: 0 };

test('the memory block states counts, and a zero says nothing', () => {
  assert.deepEqual(teamMemoryText(quiet), ['2 records shared with the team · 5 records from teammates']);
  const loud = teamMemoryText({ ...quiet, weak_repository: 3, not_shareable: 291, conflicts: 1, discarded_edits: 1, shadowed: 1, aliased: 2, held: 1, import_candidates: 4, deletions_refused: 1 });
  assert.equal(loud.length, 9);
  assert.match(loud[1], /^3 repository records stay on this device: the repository is known only by its folder name$/);
  assert.match(loud[2], /^291 queued records were not sent/);
  assert.match(loud[3], /^1 conflict copy kept/);
  assert.match(loud[7], /^1 deletion was not taken by the team/);
  assert.match(teamMemoryText({ ...quiet, pull: { unlandable: 2 } })[1], /^2 team records could not be read by this version and were skipped/);
  assert.ok(!loud.some((line) => /waits? for the answer/.test(line)), 'a held revision is transient bookkeeping and is not narrated');
});

test('a deletion states its reach in four counts and never claims more than acknowledged', () => {
  assert.equal(deletionText({ slug: 'vpn', acknowledged: 3, outstanding: 0, stale: 0, revoked: 0 }), 'vpn: deleted on every device (0 outstanding, 0 stale, 0 revoked)');
  assert.equal(deletionText({ slug: 'vpn', acknowledged: 2, outstanding: 1, stale: 1, revoked: 1 }), 'vpn: deleted on 2 devices (1 outstanding, 1 stale, 1 revoked)');
  assert.match(deletionText({ slug: 'vpn', error: 'unreachable' }), /reach unknown — unreachable/);
});

test('a record states where it stands with the team', () => {
  assert.deepEqual(teamRecordLines({ origin: 'pulled', author: 'Ada', shared: true, in_sync: true, can_travel: true, server_revision: 4 }),
    ['from the team · last revised by Ada', "shared · matches the team's revision 4"]);
  assert.deepEqual(teamRecordLines({ origin: 'local', shared: true, rejected_here: true, can_travel: true }),
    ['rejected here — team version unchanged; returns if a teammate changes it']);
  assert.deepEqual(teamRecordLines({ origin: 'local', shared: false, can_travel: false, identity_note: 'the repository is known only by its folder name' }),
    ['stays on this device: the repository is known only by its folder name']);
  assert.deepEqual(teamRecordLines({ origin: 'pulled', shared: true, in_sync: true, can_travel: true, collision: 'shadowed', wire_slug: 'vpn', conflicts: 2 }),
    ['from the team', "shared · matches the team's revision", 'not recalled: a record of yours is already named vpn', '2 conflict copies kept']);
});

test('a conflict copy names its cause and who', () => {
  assert.equal(conflictLine({ reason: 'deleted', by_author: 'Ada' }), 'edit discarded: record was deleted by Ada');
  assert.equal(conflictLine({ reason: 'stale_base', by_author: '' }), 'not sent: a teammate revised the record first');
  assert.equal(conflictLine({ reason: 'pulled_over_edit', by_author: 'Ada' }), "replaced by Ada's revision");
});

test('sent content is counted in words', () => {
  assert.equal(sentLine(1), '1 content chunk on the team server');
  assert.equal(sentLine(3), '3 content chunks on the team server');
});

test('re-promoting a rejected record that differs from the team says it sends, and asks first (U-1)', () => {
  assert.deepEqual(promoteAction({ in_sync: true }), { label: 'Promote', confirm: '' });
  const diverged = promoteAction({ in_sync: false });
  assert.equal(diverged.label, 'Send this version to the team');
  assert.match(diverged.confirm, /replaces the team's current revision for every member/);
});

test('a repository record that cannot travel says why (criterion 50)', () => {
  assert.deepEqual(teamRecordLines({ origin: 'local', shared: false, can_travel: false, identity_note: 'more than one repository on this device is named twin; the folder name cannot say which' }),
    ['stays on this device: more than one repository on this device is named twin; the folder name cannot say which']);
});

test('a record says the team refused its edit, and why (FR-6)', () => {
  assert.deepEqual(teamRecordLines({ origin: 'pulled', author: 'Ada', shared: true, in_sync: false, can_travel: true, refused: 'organization_scope_admin_only' }),
    ['from the team · last revised by Ada', 'the team did not take the edit made here: only an owner or admin changes an organization record — the team still has its own version']);
  // once the record matches the team again the refusal is history and says nothing
  assert.deepEqual(teamRecordLines({ origin: 'pulled', shared: true, in_sync: true, can_travel: true, server_revision: 2, refused: 'organization_scope_admin_only' }),
    ['from the team', 'shared · matches the team\'s revision 2']);
  assert.equal(refusalText('some_new_code'), 'some_new_code');
});
