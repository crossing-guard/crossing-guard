// The Diff pane's chrome as pure builders (workspace-panes design §3.1, §3.5):
// scope menu data, the problem → pane-state mapping, the merged-prefix file
// tree, the changed-files filter, and the viewer's presentation preferences.
// DOM renderers for the tree and the picker sit at the bottom; everything above
// them runs without a document so the rules can be pinned.
import { el } from '../core.js';
import { countChanges } from '../diff/diff-model.js';
import { fileDir, fileName } from '../diff/diff-viewer.js';

export const SCOPES = [
  { id: 'session', label: 'This session', git: false },
  { id: 'working', label: 'Working tree', git: true },
  { id: 'staged', label: 'Staged', git: true },
  { id: 'branch', label: 'Branch vs base', git: true },
  { id: 'compare', label: 'Compare against…', git: true, picker: true },
];

export const scopeById = id => SCOPES.find(scope => scope.id === id) || SCOPES[0];

export function scopeLabel(id, base) {
  if (id === 'compare' && base) return `Against ${base}`;
  return scopeById(id).label;
}

// scopeMenuItems: every scope is listed; git scopes carry the reason they are
// disabled when the session has no usable checkout, so the reader learns why
// rather than finding a menu that shrank.
export function scopeMenuItems(scope, gitDisabledReason = '') {
  return SCOPES.map(item => ({
    id: item.id, label: item.label, on: item.id === scope,
    disabled: item.git && !!gitDisabledReason,
    reason: item.git && gitDisabledReason ? gitDisabledReason : '',
  }));
}

// problemState maps a typed problem from the workspace routes to what the pane
// body says (design §3.5). `disablesGit` marks the conditions under which every
// git scope is unusable, as opposed to one scope's own failure.
export function problemState(problem) {
  const code = problem?.code || '';
  const message = problem?.message || '';
  const states = {
    'no-recorded-folder': { title: 'No folder for this session', detail: message || 'No checkout was recorded for this session, so there is nothing to compare against.', disablesGit: true },
    'ambiguous-folder': { title: 'Several folders for this session', detail: message || 'Choose the checkout to compare in the breadcrumb.', disablesGit: true },
    'not-a-repository': { title: `Not a git repository${problem?.folder ? ` · ${problem.folder}` : ''}`, detail: message || 'The session folder is not inside a git repository.', disablesGit: true },
    'git-failed': { title: 'git failed', detail: message || 'git returned an error.', disablesGit: false },
    'git-timeout': { title: 'git timed out', detail: message || 'git did not answer within the configured time.', disablesGit: false },
    'base-ref-missing': { title: 'Base ref not found', detail: message || 'The base ref does not exist in this repository.', disablesGit: false },
    'route-missing': { title: 'Git scopes are not served', detail: message || 'This daemon build does not serve git scopes.', disablesGit: true },
    'unreachable': { title: 'Daemon unreachable', detail: message || 'The last render is kept; Refresh retries.', disablesGit: false },
    'invalid-path': { title: 'Not a repository path', detail: message, disablesGit: false },
    'file-not-in-scope': { title: 'Not changed in this scope', detail: message, disablesGit: false },
  };
  if (!code) return null;
  return states[code] || { title: message || 'Unavailable', detail: '', disablesGit: false };
}

// mergedTree: folders first, shared prefixes merged, single-child chains folded
// into one label (app/Services/FraudReview), each file with its counts.
export function mergedTree(files) {
  const root = { dirs: new Map(), files: [] };
  for (const file of files) {
    let cursor = root;
    for (const part of fileDir(file.displayPath).split('/').filter(Boolean)) {
      if (!cursor.dirs.has(part)) cursor.dirs.set(part, { dirs: new Map(), files: [] });
      cursor = cursor.dirs.get(part);
    }
    cursor.files.push(file);
  }
  const out = [];
  const walk = (dir, depth) => {
    for (const [name, child] of dir.dirs) {
      let label = name, current = child;
      while (!current.files.length && current.dirs.size === 1) {
        const [next, node] = current.dirs.entries().next().value;
        label += '/' + next; current = node;
      }
      out.push({ type: 'dir', label, depth });
      walk(current, depth + 1);
    }
    for (const file of dir.files) out.push({ type: 'file', file, depth });
  };
  walk(root, 0);
  return out;
}

export function flatList(files) {
  return files.map(file => ({ type: 'file', file, depth: 0 }));
}

export function filterFiles(files, query) {
  const needle = String(query || '').trim().toLowerCase();
  if (!needle) return files.slice();
  return files.filter(file => file.displayPath.toLowerCase().includes(needle));
}

// normalizeDiffPrefs: a viewer's own toggles over the configured defaults. A
// missing or malformed preference falls back to the configured value, never to
// a value compiled in here.
export function normalizeDiffPrefs(raw, defaults = {}) {
  const source = raw && typeof raw === 'object' ? raw : {};
  const pick = (key, fallback) => typeof source[key] === 'boolean' ? source[key] : fallback === true;
  return {
    tree: pick('tree', false),
    group: pick('group', defaults.group_by_folder),
    wrap: pick('wrap', defaults.word_wrap),
    words: pick('words', defaults.highlight_words),
    whitespace: pick('whitespace', defaults.hide_whitespace),
    side: pick('side', false),
  };
}

export function checkoutLabel(checkout) {
  if (!checkout?.root) return '';
  const parts = [fileName(checkout.root.replace(/\/$/, '')) || checkout.root];
  if (checkout.branch) parts.push(checkout.branch);
  else if (checkout.head) parts.push(String(checkout.head).slice(0, 7));
  return parts.join(' · ');
}

// dirtyLabel reads the checkout's DirtyFacts as the daemon sends them
// (staged, unstaged, untracked, conflicted); "modified" is the reader's word
// for unstaged.
export function dirtyLabel(dirty) {
  if (!dirty) return '';
  const parts = [];
  if (dirty.unstaged) parts.push(`${dirty.unstaged} modified`);
  if (dirty.staged) parts.push(`${dirty.staged} staged`);
  if (dirty.untracked) parts.push(`${dirty.untracked} untracked`);
  if (dirty.conflicted) parts.push(`${dirty.conflicted} conflicted`);
  return parts.join(' · ');
}

// ---- DOM ----------------------------------------------------------------------

const countsNode = file => {
  const counts = countChanges(file);
  const tally = el('span', 'diff-counts');
  tally.append(el('span', 'diff-count-add', `+${counts.added}`), ' ', el('span', 'diff-count-delete', `−${counts.removed}`));
  return tally;
};

export function renderTree(container, files, grouped, onPick) {
  container.replaceChildren();
  for (const item of grouped ? mergedTree(files) : flatList(files)) {
    const row = el('div', `diff-tree-node diff-tree-${item.type} diff-tree-depth-${Math.min(item.depth, 4)}`);
    if (item.type === 'dir') { row.appendChild(el('span', 'diff-tree-name', item.label)); container.appendChild(row); continue; }
    row.appendChild(el('span', 'diff-tree-name', fileName(item.file.displayPath)));
    if (!grouped) row.appendChild(el('span', 'diff-tree-dir', fileDir(item.file.displayPath)));
    row.appendChild(countsNode(item.file));
    row.title = item.file.displayPath;
    row.addEventListener('click', () => onPick(item.file));
    container.appendChild(row);
  }
}

// renderPicker is the searchable list the toolbar opens for changed files and
// for refs; `rows(query)` yields {label, detail, extra?, value} entries.
export function renderPicker(host, { placeholder, rows, onPick, onClose }) {
  const picker = el('div', 'diff-picker');
  const input = document.createElement('input');
  input.type = 'search'; input.placeholder = placeholder; input.setAttribute('aria-label', placeholder);
  const list = el('div', 'diff-picker-rows');
  const draw = () => {
    list.replaceChildren();
    const found = rows(input.value);
    if (!found.length) list.appendChild(el('div', 'diff-picker-empty', 'No matches'));
    for (const entry of found) {
      const row = el('div', 'diff-picker-row');
      row.append(el('span', 'diff-picker-name', entry.label), el('span', 'diff-picker-detail', entry.detail || ''));
      if (entry.extra) row.appendChild(entry.extra);
      row.addEventListener('click', () => onPick(entry.value));
      list.appendChild(row);
    }
  };
  input.addEventListener('input', draw);
  input.addEventListener('keydown', event => { if (event.key === 'Escape') onClose(); });
  picker.append(input, list);
  host.appendChild(picker);
  draw();
  setTimeout(() => input.focus(), 0);
  return picker;
}
