// The Files pane's pure state and builders (console-files-pane-plan §4): the
// tree state (which directories are expanded, their loaded children, the
// selection), entry ordering, a filter over loaded entries, and the problem →
// pane-state mapping. No fetch, no DOM.
import { formatBytes } from '../core.js';
import { problemState } from './workspace-diff-tree.js';

export const ENTRY_KINDS = ['dir', 'file', 'symlink', 'submodule', 'missing', 'nested-repository', 'special'];

export function emptyTreeState() {
  return { expanded: new Set(), children: new Map(), states: new Map(), selected: '', open: null, changed: new Set() };
}

// rememberListing stores one directory's listing; a directory listed empty
// keeps its reason (absent, ignored, empty, nested) instead of children.
export function rememberListing(state, listing) {
  const dir = listing?.dir || '';
  state.children.set(dir, Array.isArray(listing?.entries) ? listing.entries.map(normalizeEntry) : []);
  state.states.set(dir, { state: listing?.state || '', truncated: listing?.truncated === true, dropped: listing?.dropped || 0 });
  return state;
}

export function normalizeEntry(value = {}) {
  return {
    name: String(value.name || ''),
    path: String(value.path || value.name || ''),
    kind: ENTRY_KINDS.includes(value.kind) ? value.kind : 'file',
    untracked: value.untracked === true,
    size: Number.isSafeInteger(value.size) ? value.size : 0,
    mtime: Number.isSafeInteger(value.mtime) ? value.mtime : 0,
  };
}

// visibleRows flattens the expanded tree, depth-first, into rows the pane
// paints: directories first within each level, as the daemon orders them.
export function visibleRows(state, dir = '', depth = 0, out = []) {
  for (const entry of state.children.get(dir) || []) {
    out.push({ entry, depth, expanded: state.expanded.has(entry.path), changed: state.changed.has(entry.path) });
    if (entry.kind === 'dir' && state.expanded.has(entry.path)) visibleRows(state, entry.path, depth + 1, out);
  }
  return out;
}

export function toggleDir(state, path) {
  if (state.expanded.has(path)) state.expanded.delete(path); else state.expanded.add(path);
  return state.expanded.has(path);
}

// filterRows keeps the loaded rows whose path contains the query; ancestors of
// a match stay so the hierarchy still reads.
export function filterRows(rows, query) {
  const needle = String(query || '').trim().toLowerCase();
  if (!needle) return rows;
  const keep = new Set();
  for (const row of rows) {
    if (!row.entry.path.toLowerCase().includes(needle)) continue;
    const parts = row.entry.path.split('/');
    for (let index = 1; index <= parts.length; index++) keep.add(parts.slice(0, index).join('/'));
  }
  return rows.filter(row => keep.has(row.entry.path));
}

// listingState maps a directory that listed nothing to the copy the pane shows.
export function listingState(meta) {
  const copy = { 'not-in-checkout': 'Not in the checkout', ignored: 'Ignored by git', empty: 'Empty', 'nested-repository': 'A separate repository; not opened here' };
  return copy[meta?.state] || '';
}

export function truncationCopy(meta) {
  if (!meta?.truncated && !meta?.dropped) return '';
  return meta.dropped ? `${meta.dropped} more entr${meta.dropped === 1 ? 'y' : 'ies'} not shown` : 'Listing cut short';
}

// fileState maps a read response to the header's state line.
export function fileState(file) {
  if (!file) return '';
  if (file.kind === 'binary') return `Binary · ${formatSize(file.size)}`;
  if (file.kind === 'symlink') return `Symlink → ${file.target || '?'}`;
  if (file.kind === 'special') return 'Not a regular file';
  if (file.truncated) return `First ${formatSize(file.bytes)} of ${formatSize(file.size)}`;
  return formatSize(file.size);
}

export const formatSize = formatBytes;

// fileProblemState adds the Files reader's own codes (plan §7) over the Diff
// pane's shared table, and names a refused request rather than echoing its code.
export function fileProblemState(problem) {
  const code = problem?.code || '';
  const message = problem?.message || '';
  const own = {
    'file-not-in-tree': { title: 'Not in this checkout', detail: message || 'Git does not list this file.', disablesGit: false },
    'not-a-file': { title: 'Listed, but no file', detail: message || 'Git lists this path but nothing is on disk.', disablesGit: false },
    'not-a-directory': { title: 'Not a folder', detail: message, disablesGit: false },
    'unreadable': { title: 'Cannot read this file', detail: message, disablesGit: false },
    'unsupported-platform': { title: 'File reading is not available on this platform', detail: '', disablesGit: false },
    'invalid_request': { title: 'Not a valid request', detail: message, disablesGit: false },
    'invalid_subject': { title: 'Not a valid request', detail: message, disablesGit: false },
  };
  return own[code] || problemState(problem);
}

// nextFocus is the tree's keyboard rule (WAI-ARIA tree): Up/Down move, Home/End
// jump, Right opens a closed directory or steps into an open one, Left closes
// an open directory or steps to the parent. It returns the row index to focus
// and whether to toggle the current directory.
export function nextFocus(rows, index, key) {
  const row = rows[index];
  if (!row) return { index: 0, toggle: false };
  switch (key) {
    case 'ArrowDown': return { index: Math.min(rows.length - 1, index + 1), toggle: false };
    case 'ArrowUp': return { index: Math.max(0, index - 1), toggle: false };
    case 'Home': return { index: 0, toggle: false };
    case 'End': return { index: rows.length - 1, toggle: false };
    case 'ArrowRight':
      if (row.entry.kind !== 'dir') return { index, toggle: false };
      return row.expanded ? { index: Math.min(rows.length - 1, index + 1), toggle: false } : { index, toggle: true };
    case 'ArrowLeft': {
      if (row.entry.kind === 'dir' && row.expanded) return { index, toggle: true };
      const parent = row.entry.path.split('/').slice(0, -1).join('/');
      const parentIndex = rows.findIndex(candidate => candidate.entry.path === parent);
      return { index: parentIndex >= 0 ? parentIndex : index, toggle: false };
    }
  }
  return null;
}
