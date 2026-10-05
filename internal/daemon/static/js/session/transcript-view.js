// Transcript view profiles (transcript-view-profiles plan). A profile is a module
// of rules the daemon publishes on GET /api/console/view-profiles; each rule
// decides whether a transcript row is shown, collapsed to one line, or hidden
// behind an inspectable disclosure. Pure, so the rules are pinned without a DOM.

export const SHOW = 'show';
export const COLLAPSE = 'collapse';
export const HIDE = 'hide';

// ruleFor returns the index of the first rule whose match holds, or -1 when no
// rule decides the row. It is the one matcher: the transcript and the settings
// preview both read a row's rule from here.
export function ruleFor(event, profile) {
  return (profile?.rules || []).findIndex(rule => ruleMatches(rule.match || {}, event));
}

// rowDisplay returns the display of the first rule whose match holds, else show.
export function rowDisplay(event, profile) {
  const index = ruleFor(event, profile);
  return index < 0 ? SHOW : profile.rules[index].display;
}

function ruleMatches(match, event) {
  if (match.kind && match.kind !== event?.kind) return false;
  if (match.tool && match.tool !== event?.name) return false;
  if (match.fact && !(event?.facts || []).includes(match.fact)) return false;
  if (match.min_chars > 0 && rowLength(event) < match.min_chars) return false;
  if (match.max_chars > 0 && rowLength(event) > match.max_chars) return false;
  return true;
}

// followingDisplay decides a row that may belong to a tool call: a tool result
// is drawn inside its call's chip, so it goes wherever the call went. callDisplay
// is 'shown' or 'hidden' for the call this result answers, or empty when no call
// is waiting.
export function followingDisplay(event, profile, callDisplay) {
  if (event?.kind === 'tool_result' && callDisplay === 'hidden') return HIDE;
  if (event?.kind === 'tool_result' && callDisplay === 'shown') return SHOW;
  return rowDisplay(event, profile);
}

// createCallPairing matches results to calls in arrival order. Canonical events
// carry no call id, and calls fired together are recorded as call, call, result,
// result — so the oldest waiting call owns the next result. close() ends every
// wait and returns the calls that never got one.
export function createCallPairing() {
  let waiting = [];
  return {
    call(entry) { waiting.push(entry); },
    peek() { return waiting[0] || null; },
    result() { return waiting.shift() || null; },
    close() { const unanswered = waiting; waiting = []; return unanswered; },
  };
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

// A disclosure describes observed activity, not a profile. Execution is named
// only when the existing audit enrichment positively identifies it; missing
// facts leave an honest generic tool label (including runtimes with weaker
// detector coverage).
export function hiddenActivityKind(event) {
  if (event?.kind === 'tool_call') return (event.facts || []).includes('exec:run') ? 'command' : 'tool';
  if (event?.kind === 'tool_result') return 'result';
  if (event?.kind === 'thinking') return 'thinking';
  if (event?.kind === 'context') return 'context';
  return 'other';
}

export function hiddenActivityLabel(kind, count, pending = false) {
  if (kind === 'command') return count === 1
    ? (pending ? 'Running a command' : 'Ran a command')
    : (pending ? 'Running ' : 'Ran ') + count + ' commands';
  if (kind === 'tool') return count === 1
    ? (pending ? 'Using a tool' : 'Used a tool')
    : (pending ? 'Using ' : 'Used ') + count + ' tools';
  if (kind === 'thinking') return count === 1 ? 'Thinking' : count + ' thinking entries';
  if (kind === 'context') return count === 1 ? 'Context added' : count + ' context entries';
  if (kind === 'result') return count === 1 ? 'Result' : count + ' results';
  return count === 1 ? 'Hidden activity' : count + ' hidden entries';
}
