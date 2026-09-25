// Transcript view profiles (transcript-view-profiles plan). A profile is a module
// of rules the daemon publishes on GET /api/console/view-profiles; each rule
// decides whether a transcript row is shown, collapsed to one line, or hidden
// behind a count. Pure, so the rules are pinned without a DOM.

export const SHOW = 'show';
export const COLLAPSE = 'collapse';
export const HIDE = 'hide';

// rowDisplay returns the display of the first rule whose match holds, else show.
export function rowDisplay(event, profile) {
  for (const rule of profile?.rules || []) {
    if (ruleMatches(rule.match || {}, event)) return rule.display;
  }
  return SHOW;
}

function ruleMatches(match, event) {
  if (match.kind && match.kind !== event?.kind) return false;
  if (match.tool && match.tool !== event?.name) return false;
  if (match.min_chars > 0 && rowLength(event) < match.min_chars) return false;
  return true;
}

// followingDisplay decides a row that may belong to the tool call before it:
// a tool result is drawn inside its call's chip, so it goes wherever the call
// went. previousTool is 'shown', 'hidden', or empty when no call is pending.
export function followingDisplay(event, profile, previousTool) {
  if (event?.kind === 'tool_result' && previousTool === 'hidden') return HIDE;
  if (event?.kind === 'tool_result' && previousTool === 'shown') return SHOW;
  return rowDisplay(event, profile);
}

// rowLength is a row's whole length: a clipped payload declares it in full_len.
export function rowLength(event) {
  return Math.max(Number(event?.full_len) || 0, String(event?.text || '').length);
}

// selectProfile picks the module a session opens with: its role's, else the
// default, else none, which shows every row.
export function selectProfile(profiles, selection, role) {
  const byId = new Map((profiles || []).map(profile => [profile.id, profile]));
  const roleProfile = role ? selection?.role_profiles?.[role] : '';
  return byId.get(roleProfile) || byId.get(selection?.default_profile) || null;
}

// rowPeek is the one line a collapsed row shows: its first non-empty line.
export function rowPeek(text, limit = 140) {
  const line = String(text || '').split('\n').map(part => part.trim()).find(Boolean) || '';
  return line.length > limit ? line.slice(0, limit - 1) + '…' : line;
}

export function sizeLabel(chars) {
  if (chars < 1000) return chars + ' chars';
  return (chars / 1000).toFixed(chars < 10000 ? 1 : 0) + 'K chars';
}

// hiddenUnits counts what a reader would call a row: a tool call and its
// result are one.
export function hiddenUnits(events) {
  let units = 0;
  for (let i = 0; i < events.length; i++) {
    if (events[i]?.kind === 'tool_result' && events[i - 1]?.kind === 'tool_call') continue;
    units++;
  }
  return units;
}
