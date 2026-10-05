// Team memory in words (team item 5): pure functions over the daemon's facts, shared by
// Settings → Team, the session footer and the Memory view. No imports, so they are tested
// without a browser. Facts only — a zero says nothing, and nothing here narrates the system.

export const count = (n, one, many) => n + ' ' + (n === 1 ? one : many);

// teamMemoryText states the memory sync in lines; a zero says nothing.
export function teamMemoryText(m) {
  const lines = [count(m.shared, 'record', 'records') + ' shared with the team · ' + count(m.pulled, 'record', 'records') + ' from teammates'];
  if (m.weak_repository) lines.push(count(m.weak_repository, 'repository record stays', 'repository records stay') + ' on this device: the repository is known only by its folder name');
  if (m.not_shareable) lines.push(count(m.not_shareable, 'queued record was', 'queued records were') + ' not sent: not shared, not active, or identified only by a folder name');
  if (m.conflicts) lines.push(count(m.conflicts, 'conflict copy', 'conflict copies') + ' kept: a teammate\'s revision replaced an edit made here');
  if (m.discarded_edits) lines.push(count(m.discarded_edits, 'edit', 'edits') + ' kept as a conflict copy: the record was deleted by a teammate');
  if (m.shadowed) lines.push(count(m.shadowed, 'team record is', 'team records are') + ' not recalled: a record of yours has the same name');
  if (m.aliased) lines.push(count(m.aliased, 'team record is', 'team records are') + ' under a local name: another record here has the same name');
  if (m.deletions_refused) lines.push(count(m.deletions_refused, 'deletion was', 'deletions were') + ' not taken by the team (refused, or never delivered): what the team holds returns to this device');
  if (m.pull && m.pull.unlandable) lines.push(count(m.pull.unlandable, 'team record', 'team records') + ' could not be read by this version and ' + (m.pull.unlandable === 1 ? 'was' : 'were') + ' skipped; a newer version pulls again');
  if (m.import_candidates) lines.push(count(m.import_candidates, 'record', 'records') + ' — imported, or received from a team — can be shared one at a time, from Memory');
  return lines;
}

// deletionText states one deletion's reach in the O-5 vocabulary.
export function deletionText(d) {
  if (d.error) return d.slug + ': reach unknown — ' + d.error;
  const behind = d.outstanding + d.stale + d.revoked;
  const reach = behind === 0 ? 'deleted on every device' : 'deleted on ' + count(d.acknowledged, 'device', 'devices');
  return d.slug + ': ' + reach + ' (' + d.outstanding + ' outstanding, ' + d.stale + ' stale, ' + d.revoked + ' revoked)';
}

// sentLine states what the team server holds of one session's content from this device.
export function sentLine(chunks) {
  return count(chunks, 'content chunk', 'content chunks') + ' on the team server';
}

// refusalText words why the team did not take a record's latest edit.
export function refusalText(code) {
  if (code === 'organization_scope_admin_only') return 'only an owner or admin changes an organization record';
  if (code === 'slug_immutable') return 'a team record cannot be renamed';
  if (code === 'tombstoned') return 'the record was deleted for the team';
  if (code === 'foreign_organization') return 'the record belongs to another organization';
  return code;
}

// teamRecordLines states one record's standing with the team (GET /api/memory's team block).
export function teamRecordLines(team) {
  const lines = [];
  if (team.origin === 'pulled') lines.push('from the team' + (team.author ? ' · last revised by ' + team.author : ''));
  if (team.refused && !team.in_sync) lines.push('the team did not take the edit made here: ' + refusalText(team.refused) + ' — the team still has its own version');
  else if (team.rejected_here) lines.push('rejected here — team version unchanged; returns if a teammate changes it');
  else if (team.shared) lines.push(team.in_sync ? 'shared · matches the team\'s revision' + (team.server_revision ? ' ' + team.server_revision : '') : 'shared · edited here since the team\'s revision');
  else if (team.can_travel) lines.push('not shared — stays on this device until you share it');
  if (!team.can_travel && team.identity_note) lines.push('stays on this device: ' + team.identity_note);
  if (team.collision === 'shadowed') lines.push('not recalled: a record of yours is already named ' + team.wire_slug);
  if (team.collision === 'alias') lines.push('the team calls this record ' + team.wire_slug + '; another record here has that name');
  if (team.conflicts) lines.push(count(team.conflicts, 'conflict copy', 'conflict copies') + ' kept');
  return lines;
}

// conflictLine words one conflict copy's cause.
export function conflictLine(c) {
  const who = c.by_author || 'a teammate';
  if (c.reason === 'deleted') return 'edit discarded: record was deleted by ' + who;
  if (c.reason === 'stale_base') return 'not sent: ' + who + ' revised the record first';
  return 'replaced by ' + who + '\'s revision';
}

// promoteAction is the promote control for a team record rejected on this device (U-1):
// when this device's version differs from the team's, promoting SENDS it, so the label
// says so and the click is confirmed first; an unchanged record is simply promoted.
export function promoteAction(team) {
  if (team.in_sync) return { label: 'Promote', confirm: '' };
  return { label: 'Send this version to the team',
    confirm: 'Send the version on this device to the team? It replaces the team\'s current revision for every member. The team\'s version is not brought back here first.' };
}
