// Renders the diff model (workspace-panes design §3.1): one stacked column of
// files, each under a sticky header with a collapse chevron and +n −m; hunks as
// line-anchored tables with an unmodified-run row between them; session edits
// as unanchored before/after blocks with a gutter and no line numbers (§3.3).
// Presentation flags (wrap, side by side, word marks, whitespace) arrive as
// options; their defaults are configuration the pane reads, never this file's.
import { countChanges, lineNavigation, normalizeDiffModel } from './diff-model.js';

const node = (tag, className, value) => {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (value !== undefined) element.textContent = value;
  return element;
};

export const fileName = path => String(path || '').split('/').pop();
export const fileDir = path => String(path || '').split('/').slice(0, -1).join('/');
const fileGlyph = path => /\.(c|cc|cpp|cs|go|java|js|jsx|kt|mjs|php|py|rb|rs|sh|swift|ts|tsx)$/i.test(path) ? '<>' : '▫';

function openButton(label, target, openFile) {
  const button = node('button', 'diff-open', label);
  button.type = 'button';
  button.addEventListener('click', event => { event.stopPropagation(); openFile?.(target); });
  return button;
}

// capturedTextVisibility decides what a reader sees when the rendered diff does
// not account for the whole captured body. When nothing could be rendered the
// text IS the answer and must be in front of them; when a diff did render, the
// text is a second copy and belongs behind a disclosure. Exported because the
// rule matters more than the markup, and this way it can be pinned without a DOM.
export function capturedTextVisibility(model) {
  if (!model || !model.rawFallback) return 'none';
  const hunks = (model.files || []).reduce((total, file) => total + (file.hunks ? file.hunks.length : 0), 0);
  return hunks > 0 ? 'collapsed' : 'shown';
}

const squash = value => value.replace(/\s+/g, '');

// foldWhitespace turns a delete/add pair that differ only in whitespace into one
// context line showing the new text, which is what "hide whitespace changes"
// promises. Unpaired lines are untouched.
export function foldWhitespace(lines) {
  const out = [];
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index], next = lines[index + 1];
    if (line.kind === 'delete' && next?.kind === 'add' && squash(line.text) === squash(next.text)) {
      out.push({ kind: 'context', oldLine: line.oldLine, newLine: next.newLine, text: next.text });
      index++;
      continue;
    }
    out.push(line);
  }
  return out;
}

// markChangedWords highlights the tokens that differ between an adjacent
// delete/add pair. Only paired lines are marked: a removed block and an added
// block are not line-paired, so an edit never gets marks.
export function changedTokens(before, after) {
  const split = value => value.split(/(\s+)/);
  const left = split(before), right = split(after);
  const inRight = new Set(right), inLeft = new Set(left);
  return {
    before: left.map(token => ({ token, changed: !!token.trim() && !inRight.has(token) })),
    after: right.map(token => ({ token, changed: !!token.trim() && !inLeft.has(token) })),
  };
}

function paintTokens(cell, tokens, markClass) {
  cell.replaceChildren();
  for (const { token, changed } of tokens) {
    if (!changed) { cell.append(document.createTextNode(token)); continue; }
    cell.append(node('mark', markClass, token));
  }
}

function markChangedWords(rows) {
  for (let index = 0; index + 1 < rows.length; index++) {
    const [before, after] = [rows[index], rows[index + 1]];
    if (before.kind !== 'delete' || after.kind !== 'add') continue;
    const tokens = changedTokens(before.code.textContent, after.code.textContent);
    paintTokens(before.code, tokens.before, 'diff-mark-delete');
    paintTokens(after.code, tokens.after, 'diff-mark-add');
    index++;
  }
}

function numberCell(file, line, openFile) {
  const cell = node('td', 'diff-ln');
  const shown = line.newLine ?? line.oldLine;
  const target = lineNavigation(file, line);
  if (target && typeof openFile === 'function') cell.appendChild(openButton(String(shown), target, openFile));
  else cell.textContent = shown ?? '';
  return cell;
}

const gutterCell = kind => node('td', 'diff-gutter', { add: '+', delete: '−' }[kind] || '');

function lineRow(file, line, options) {
  const row = document.createElement('tr');
  row.className = `diff-line diff-line-${line.kind}`;
  const code = node('td', 'diff-code', line.text);
  row.append(numberCell(file, line, options.openFile), gutterCell(line.kind), code);
  return { row, kind: line.kind, code };
}

function renderUnifiedHunk(file, hunk, options) {
  const table = node('table', 'diff-lines');
  const body = document.createElement('tbody');
  const rows = [];
  for (const line of options.hideWhitespace ? foldWhitespace(hunk.lines) : hunk.lines) {
    const built = lineRow(file, line, options);
    rows.push(built);
    body.appendChild(built.row);
  }
  if (options.highlightWords) markChangedWords(rows);
  table.appendChild(body);
  return table;
}

// pairLines zips a run of deletes with the run of adds that follows it so a
// side-by-side table shows the old and new text on one row where they pair.
export function pairLines(lines) {
  const pairs = [];
  let deletes = [], adds = [];
  const flush = () => {
    const count = Math.max(deletes.length, adds.length);
    for (let index = 0; index < count; index++) pairs.push({ old: deletes[index] || null, new: adds[index] || null });
    deletes = []; adds = [];
  };
  for (const line of lines) {
    if (line.kind === 'delete') { deletes.push(line); continue; }
    if (line.kind === 'add') { adds.push(line); continue; }
    flush();
    pairs.push({ old: line, new: line });
  }
  flush();
  return pairs;
}

function splitCells(file, line, side, options) {
  const number = node('td', 'diff-ln');
  const code = node('td', `diff-code${line ? ` diff-side-${line.kind}` : ' diff-side-blank'}`, line ? line.text : '');
  if (line) {
    const shown = side === 'old' ? line.oldLine : line.newLine;
    const target = side === 'new' ? lineNavigation(file, line) : null;
    if (target && typeof options.openFile === 'function') number.appendChild(openButton(String(shown), target, options.openFile));
    else number.textContent = shown ?? '';
  }
  return [number, code];
}

function renderSplitHunk(file, hunk, options) {
  const table = node('table', 'diff-lines diff-split');
  const body = document.createElement('tbody');
  const marks = [];
  for (const pair of pairLines(options.hideWhitespace ? foldWhitespace(hunk.lines) : hunk.lines)) {
    const row = document.createElement('tr'); row.className = 'diff-line';
    const [oldNumber, oldCode] = splitCells(file, pair.old, 'old', options);
    const [newNumber, newCode] = splitCells(file, pair.new, 'new', options);
    row.append(oldNumber, oldCode, newNumber, newCode);
    body.appendChild(row);
    if (pair.old?.kind === 'delete' && pair.new?.kind === 'add') marks.push({ kind: 'delete', code: oldCode }, { kind: 'add', code: newCode });
  }
  if (options.highlightWords) markChangedWords(marks);
  table.appendChild(body);
  return table;
}

const firstOld = hunk => hunk.lines.find(line => line.oldLine !== null)?.oldLine ?? null;
const lastOld = hunk => [...hunk.lines].reverse().find(line => line.oldLine !== null)?.oldLine ?? null;

// unmodifiedBefore is the run of untouched lines between the previous hunk and
// this one, read off the line numbers. Nothing is known past the last hunk.
export function unmodifiedBefore(previous, hunk) {
  const start = firstOld(hunk);
  if (start === null) return 0;
  const after = previous ? lastOld(previous) : 0;
  return after === null ? 0 : Math.max(0, start - after - 1);
}

function renderHunks(file, options) {
  const fragment = document.createDocumentFragment();
  file.hunks.forEach((hunk, index) => {
    const run = unmodifiedBefore(file.hunks[index - 1], hunk);
    if (run) fragment.appendChild(node('div', 'diff-unmodified', `⌃ ${run} unmodified line${run === 1 ? '' : 's'}`));
    if (hunk.header && options.showHunkHeaders) fragment.appendChild(node('div', 'diff-hunk-header', hunk.header));
    fragment.appendChild(options.sideBySide ? renderSplitHunk(file, hunk, options) : renderUnifiedHunk(file, hunk, options));
  });
  return fragment;
}

const blockLines = value => value === null ? [] : value.split('\n').slice(0, value.endsWith('\n') ? -1 : undefined);

function editRows(body, kind, value) {
  for (const text of blockLines(value)) {
    const row = document.createElement('tr');
    row.className = `diff-line diff-line-${kind}`;
    row.append(gutterCell(kind), node('td', 'diff-code', text));
    body.appendChild(row);
  }
}

function editState(edit) {
  if (edit.state === 'pending') return 'Loading…';
  if (edit.state === 'unavailable') return `Recorded ${edit.beforeBytes + edit.afterBytes} bytes; the text is not available to show.`;
  if (edit.kind === 'content' && edit.beforeBytes === 0) return `${blockLines(edit.after).length} lines written`;
  return '';
}

function renderEdit(file, edit, options) {
  const fragment = document.createDocumentFragment();
  if (edit.why) fragment.appendChild(node('div', 'diff-why', edit.why));
  const state = editState(edit);
  if (state) fragment.appendChild(node('div', 'diff-edit-state', state));
  if (edit.state !== 'loaded') return fragment;
  if (edit.hunks.length) {
    fragment.appendChild(renderHunks({ ...file, hunks: edit.hunks }, options));
    return fragment;
  }
  const table = node('table', 'diff-lines diff-edit');
  const body = document.createElement('tbody');
  editRows(body, 'delete', edit.before);
  editRows(body, 'add', edit.after);
  table.appendChild(body);
  fragment.appendChild(table);
  return fragment;
}

function renderFileHeader(file, options, collapsed) {
  const header = node('div', 'diff-file-header');
  header.setAttribute('role', 'button'); header.tabIndex = 0;
  header.setAttribute('aria-expanded', String(!collapsed));
  const counts = countChanges(file);
  header.append(
    node('span', 'diff-chevron', collapsed ? '▸' : '▾'),
    node('span', 'diff-glyph', fileGlyph(file.displayPath)),
    node('span', 'diff-name', fileName(file.displayPath)),
    node('span', 'diff-dir', fileDir(file.displayPath)));
  if (file.kind !== 'text' && file.kind !== 'unknown') header.appendChild(node('span', 'diff-file-kind', file.kind));
  const tally = node('span', 'diff-counts');
  tally.append(node('span', 'diff-count-add', `+${counts.added}`), ' ', node('span', 'diff-count-delete', `−${counts.removed}`));
  header.appendChild(tally);
  if (file.navigationPath && typeof options.openFile === 'function') header.appendChild(openButton('Open', { path: file.navigationPath, line: 1 }, options.openFile));
  return header;
}

function renderFileBody(file, options) {
  const body = node('div', 'diff-file-body');
  for (const edit of file.edits) body.appendChild(renderEdit(file, edit, options));
  body.appendChild(renderHunks(file, options));
  if (file.summary) body.appendChild(node('div', 'sub diff-file-summary', file.summary));
  if (file.truncated) body.appendChild(node('div', 'evidence-boundary', 'This file is truncated; omitted content is not reviewable.'));
  if (!file.hunks.length && !file.edits.length && !file.summary) body.appendChild(node('div', 'sub diff-file-summary', 'No renderable text hunks were retained.'));
  return body;
}

function renderFile(file, options) {
  const section = node('section', 'diff-file');
  section.dataset.path = file.displayPath; section.dataset.fileId = file.id;
  const collapsed = options.collapsed instanceof Set ? options.collapsed.has(file.id) : false;
  const header = renderFileHeader(file, options, collapsed);
  const body = renderFileBody(file, options);
  body.hidden = collapsed;
  const toggle = () => {
    if (typeof options.onToggle === 'function') { options.onToggle(file); return; }
    body.hidden = !body.hidden;
    header.setAttribute('aria-expanded', String(!body.hidden));
    header.querySelector('.diff-chevron').textContent = body.hidden ? '▸' : '▾';
  };
  header.addEventListener('click', toggle);
  header.addEventListener('keydown', event => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); toggle(); } });
  section.append(header, body);
  return section;
}

function renderSourceLine(model) {
  const boundary = node('div', 'diff-source');
  boundary.append(node('strong', '', model.source.label));
  if (model.source.observedAt) boundary.append(node('span', 'sub', ` · ${model.source.observedAt}`));
  if (model.source.freshness) boundary.append(node('span', 'sub', ` · ${model.source.freshness}`));
  return boundary;
}

function renderCapturedText(host, model) {
  const visibility = capturedTextVisibility(model);
  if (visibility === 'shown') {
    host.append(
      node('div', 'evidence-state', 'The runtime recorded this as text the console could not read as a diff.'),
      node('pre', 'evidence-retained-body', model.rawFallback));
  } else if (visibility === 'collapsed') {
    const more = document.createElement('details');
    more.className = 'evidence-detail-group';
    const summary = document.createElement('summary');
    summary.textContent = 'Show the text the runtime recorded';
    more.append(summary, node('pre', 'evidence-retained-body', model.rawFallback));
    host.appendChild(more);
  } else if (!model.files.length) {
    host.appendChild(node('div', 'evidence-state', 'Nothing was recorded for this change.'));
  }
}

export function renderDiff(host, value, options = {}) {
  const model = normalizeDiffModel(value);
  const settings = { showHunkHeaders: true, ...options };
  host.replaceChildren();
  host.classList.toggle('diff-nowrap', options.wordWrap === false);
  if (options.showSource !== false) host.appendChild(renderSourceLine(model));
  if (model.truncated) host.appendChild(node('div', 'evidence-boundary', '? Diff is truncated; omitted content is not reviewable.'));
  for (const file of model.files) host.appendChild(renderFile(file, settings));
  renderCapturedText(host, model);
  return model;
}
