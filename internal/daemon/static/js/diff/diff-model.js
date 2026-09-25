// Pure presentation contract shared by retained, session-edit, and live diff
// sources (workspace-panes design §3.1, R20). It validates display/navigation
// facts only; it never creates mutation authority.
//
// A file carries either line-anchored `hunks` (git scopes, runtime-supplied
// patches) or unanchored `edits` (design §3.3: a recorded before/after pair with
// no line offsets), and may carry both when a session recorded a patch for one
// edit and a replacement for another.

const text = value => typeof value === 'string' ? value : '';
const integer = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
const textOrNull = value => typeof value === 'string' ? value : null;

export function navigableDiffPath(value) {
  const path = text(value);
  if (!path || path === '/dev/null' || path.includes('\0') || path.startsWith('/')) return '';
  const parts = path.replaceAll('\\', '/').split('/');
  if (parts.some(part => !part || part === '.' || part === '..')) return '';
  return parts.join('/');
}

function normalizeLine(value = {}) {
  const kind = ['context', 'add', 'delete', 'meta'].includes(value.kind) ? value.kind : 'meta';
  return {
    kind,
    oldLine: integer(value.oldLine),
    newLine: integer(value.newLine),
    text: text(value.text),
  };
}

function normalizeHunk(value = {}, index) {
  return {
    id: text(value.id) || `hunk-${index + 1}`,
    header: text(value.header),
    lines: (Array.isArray(value.lines) ? value.lines : []).map(normalizeLine),
  };
}

export const EDIT_KINDS = ['replacement', 'content', 'diff'];
export const EDIT_STATES = ['pending', 'loaded', 'unavailable'];

// An edit's bodies are strings once loaded and null until then or when the
// store never retained them; `state` says which, so the viewer never mistakes
// "not fetched yet" for "nothing was recorded". A runtime-supplied patch keeps
// its hunks on the edit: each such patch has its own baseline, so two of them
// are never merged into one file-wide hunk list.
function normalizeEdit(value = {}, index) {
  return {
    id: text(value.id) || `edit-${index + 1}`,
    kind: EDIT_KINDS.includes(value.kind) ? value.kind : 'replacement',
    state: EDIT_STATES.includes(value.state) ? value.state : 'pending',
    why: text(value.why),
    replaceAll: value.replaceAll === true,
    before: textOrNull(value.before),
    after: textOrNull(value.after),
    beforeBytes: integer(value.beforeBytes) ?? 0,
    afterBytes: integer(value.afterBytes) ?? 0,
    tool: text(value.tool),
    operation: text(value.operation),
    resultId: integer(value.resultId),
    ordinal: integer(value.ordinal),
    hunks: (Array.isArray(value.hunks) ? value.hunks : []).map(normalizeHunk),
  };
}

const lineCount = value => value ? value.split('\n').length - (value.endsWith('\n') ? 1 : 0) : 0;

function countHunkLines(hunks, counts) {
  for (const hunk of hunks || []) for (const line of hunk.lines || []) {
    if (line.kind === 'add') counts.added++;
    else if (line.kind === 'delete') counts.removed++;
  }
}

// countChanges derives the +n −m a header shows from what will be rendered, so a
// count never disagrees with the rows beneath it. Server-supplied counts win
// only while nothing has been loaded to count. It accepts the pane's raw file
// objects as well as normalized ones, so it never assumes an array exists.
export function countChanges(file) {
  const counts = { added: 0, removed: 0 };
  const hunks = file.hunks || [], edits = file.edits || [];
  countHunkLines(hunks, counts);
  for (const edit of edits) {
    if (edit.state !== 'loaded') continue;
    counts.added += lineCount(edit.after ?? null);
    counts.removed += lineCount(edit.before ?? null);
    countHunkLines(edit.hunks, counts);
  }
  if (!counts.added && !counts.removed && !hunks.length && !edits.some(edit => edit.state === 'loaded')) {
    return { added: file.added ?? 0, removed: file.removed ?? 0 };
  }
  return counts;
}

export const FILE_KINDS = ['text', 'binary', 'rename', 'mode', 'typechange', 'conflict', 'submodule', 'special', 'unknown'];

function normalizeFile(value = {}, index) {
  const oldPath = text(value.oldPath);
  const newPath = text(value.newPath);
  return {
    id: text(value.id) || `file-${index + 1}`,
    oldPath,
    newPath,
    displayPath: text(value.displayPath) || navigableDiffPath(newPath) || navigableDiffPath(oldPath) || newPath || oldPath || '(unknown file)',
    navigationPath: navigableDiffPath(newPath),
    kind: FILE_KINDS.includes(value.kind) ? value.kind : 'unknown',
    status: text(value.status),
    summary: text(value.summary),
    freshness: text(value.freshness),
    added: integer(value.added),
    removed: integer(value.removed),
    truncated: value.truncated === true,
    hunks: (Array.isArray(value.hunks) ? value.hunks : []).map(normalizeHunk),
    edits: (Array.isArray(value.edits) ? value.edits : []).map(normalizeEdit),
  };
}

export function normalizeDiffModel(value = {}) {
  const source = value.source && typeof value.source === 'object' ? value.source : {};
  return {
    source: {
      kind: source.kind === 'live' ? 'live' : source.kind === 'session' ? 'session' : 'retained',
      label: text(source.label) || (source.kind === 'live' ? 'Live workspace' : source.kind === 'session' ? 'This session' : 'Retained evidence'),
      observedAt: text(source.observedAt),
      freshness: text(source.freshness),
    },
    files: (Array.isArray(value.files) ? value.files : []).map(normalizeFile),
    parseGaps: (Array.isArray(value.parseGaps) ? value.parseGaps : []).map(text).filter(Boolean),
    truncated: value.truncated === true,
    rawFallback: text(value.rawFallback),
  };
}

export function lineNavigation(file, line) {
  const path = navigableDiffPath(file?.newPath);
  const number = integer(line?.newLine);
  if (!path || number === null || number === 0 || !['add', 'context'].includes(line?.kind)) return null;
  return { path, line: number };
}
