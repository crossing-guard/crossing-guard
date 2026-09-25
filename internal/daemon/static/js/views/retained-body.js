import { api, el } from '../core.js';
import { renderDiff } from '../diff/diff-viewer.js';
import { parseUnifiedDiff } from '../diff/unified-diff.js';
import { sessionQuery } from './session-evidence.js';

const selectionRoot = ctx => ctx?.selection?.cwd || ctx?.selection?.project || ctx?.selection?.root || '';

// rendersAsDiff is the whole rule for which retained bodies reach the parser.
// Exported so it can be pinned without a DOM: everything else on this path needs
// a document and a fetch.
export function rendersAsDiff(bodyKind, text) {
  return bodyKind === 'effect_diff' && !!text;
}

// checkoutRootOf recovers the root the daemon already joined. The server issues
// both halves and builds the absolute one by joining the root with the relative
// one, so this removes a suffix rather than resolving anything. If the two do not
// correspond it yields nothing, and the caller offers nothing: falling back to the
// session's working directory there would quietly substitute the weaker basis this
// path exists to stop using.
function checkoutRootOf(absolutePath, path) {
  if (!absolutePath || !path) return '';
  const suffix = '/' + path;
  return absolutePath.endsWith(suffix) ? absolutePath.slice(0, -suffix.length) : '';
}

function openFile(root, target) {
  document.dispatchEvent(new CustomEvent('cg:open-editor', { detail: { ...target, root } }));
}

// What a retained path means on the far side of the change depends on what the
// runtime did. A deleted file has no far side, and for a move the retained path
// is the one the file left, while the diff's line numbers belong to wherever it
// landed — which was never captured. Neither may be offered as a place to open.
// Everything else, including an operation the harvesters recorded as unknown,
// keeps the path: "unknown" here means the runtime did not name the operation,
// not that the file might have been deleted.
export function newPathFor(operation, path) {
  if (operation === 'delete') return '/dev/null';
  if (operation === 'move') return '';
  return path;
}

// hints carries what the CALLER already knows from typed evidence: a
// server-issued repository-relative path and its absolute twin. A caller with no
// such facts passes nothing and gets today's behaviour, which is the honest
// outcome — this module never derives a path of its own.
export async function revealRetainedBody(ctx, button, extra, hints = {}) {
  if (button.disabled) return;
  button.disabled = true;
  const host = el('div', 'evidence-retained-body-host');
  host.appendChild(el('div', 'sub', 'Loading retained text…'));
  button.after(host);
  try {
    const response = await api('/api/govern/session?' + sessionQuery(ctx, { section: 'body', ...extra }));
    if (!host.isConnected) return;
    const body = response.body || {};
    const value = body.text || '';
    if (rendersAsDiff(extra.body_kind, value)) {
      const path = typeof hints.path === 'string' ? hints.path : '';
      // A caller that supplies typed facts is held to them. Only a caller with no
      // facts at all falls back to the session's working directory, which is what
      // Activity has always done and what this does not extend.
      const root = path ? checkoutRootOf(hints.absolutePath, path) : selectionRoot(ctx);
      const parsed = parseUnifiedDiff(value, {}, path ? { path, newPath: newPathFor(hints.operation, path) } : {});
      renderDiff(host, parsed, { openFile: root ? target => openFile(root, target) : null });
      return;
    }
    host.replaceChildren(el('pre', 'evidence-retained-body', value || '? body unavailable'));
  } catch {
    if (host.isConnected) host.replaceChildren(el('div', 'evidence-state', 'Retained text unavailable'));
  }
}

export function retainedBodyButton(ctx, label, extra, hints = {}) {
  const button = el('button', '', label);
  button.type = 'button';
  button.addEventListener('click', () => void revealRetainedBody(ctx, button, extra, hints));
  return button;
}
