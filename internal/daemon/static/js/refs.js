// Reference resolution, client side — console-and-info-panel §5, impl-plan Step 1.2.
//
// linkify (core.js) runs while building a transcript and must decide clickable
// vs. dim SYNCHRONOUSLY, so this module fetches the whole manifest once per
// project and answers from memory afterwards.
//
// The honesty rule that shapes this file: until the manifest has arrived we
// answer 'indexing', NEVER 'missing'. They look the same to a renderer and mean
// opposite things — "we haven't looked yet" vs. "we looked and it isn't there" —
// and rendering the second while the first is true is the dead-link failure the
// index exists to prevent (impl-plan R2).
import { api } from './core.js';

// Resolution states. Mirrors RefState in internal/daemon/refindex.go, plus
// 'indexing', which is client-only: the server always knows.
export const REF_STATE = {
  resolved: 'resolved',
  missing: 'missing',
  stale: 'stale',
  unresolved: 'unresolved',
  external: 'external',
  indexing: 'indexing',
};

// One manifest per project root, so moving between sessions from different
// projects does not thrash a single slot.
const manifests = new Map(); // root -> { paths:Set, ids:Map, docs:[], builtAt }
const inflight = new Map();  // root -> Promise
let currentRoot = null;

// idShaped matches the ids we autolink: our D-items and ADRs. Shape is not
// existence — an id-shaped token with no definition resolves 'unresolved' WITH a
// reason, which is a different answer from "not a reference at all".
const idShaped = /^(D\d+|ADR\s*\d+)$/i;
// pathShaped is deliberately conservative: a token must look like a real repo
// path or a known source file before we even ask the index. Anything looser turns
// ordinary prose containing a slash into a link (§13 false-positive risk).
const pathShaped = /^[\w.@-]+(?:\/[\w.@-]+)+(?::\d+)?$/;
const fileShaped = /^[\w.-]+\.(?:go|js|mjs|css|html|md|json|ya?ml|sh|sql|txt)(?::\d+)?$/;

// setProject points the resolver at a project root and starts the fetch. Called
// when a session opens; safe to call repeatedly with the same root.
export function setProject(root) {
  currentRoot = root || null;
  if (currentRoot) load(currentRoot);
  return currentRoot;
}

// readyWaitMs caps how long a render may wait for the index.
//
// Two failure modes pull in opposite directions, and the number has to separate
// them rather than split the difference:
//
//   a project the daemon CANNOT read blocks until the server's 4s timeout, and
//   waiting for that made opening a session take eight seconds — a working
//   console traded for a cosmetic feature;
//
//   a large project that works fine is simply slow — a 7,700-file tree ships a
//   522KB manifest in ~0.9s — and cutting it off means the transcript renders
//   with nothing linked AND never recovers, because we do not re-render.
//
// So the cap sits above a slow-but-working index and below the server's own
// timeout: wait for real work, bound the broken case. A root that does fail is
// remembered, so it costs this once rather than on every session.
const readyWaitMs = 2500;

// failedRoots remembers a root whose index could not be built, so the second
// session you open from it does not pay the wait again.
//
// It EXPIRES, and that is not a detail. Without a cooldown one transient failure
// — a daemon mid-restart refusing a connection — marked the project dead for the
// life of the page, and every session opened afterwards reported "references
// unavailable" while the endpoint answered 200 to anything that actually asked.
// A cache of failures that never retries states yesterday's problem as today's.
const failedRoots = new Map(); // root -> { why, at }
const failedTTL = 20000;

// failureFor returns a remembered failure only while it is still fresh.
function failureFor(root) {
  const f = failedRoots.get(root);
  if (!f) return null;
  if (Date.now() - f.at > failedTTL) { failedRoots.delete(root); return null; }
  return f;
}

// ready resolves once the current project's manifest is in memory, or sooner if
// the cap expires or this root is known-bad. It never rejects: a missing index
// degrades linkify, it must not break opening a session.
export function ready() {
  return readyFor(currentRoot);
}

function readyFor(root) {
  if (!root) return Promise.resolve(null);
  if (failureFor(root)) return Promise.resolve(null); // still cooling off
  return Promise.race([
    load(root).catch(e => { failedRoots.set(root, { why: e.message || String(e), at: Date.now() }); return null; }),
    new Promise(r => setTimeout(() => r(null), readyWaitMs)),
  ]);
}

// forProject binds asynchronous manifest work and synchronous resolution to one
// immutable root without changing the selected project's global compatibility
// pointer. Session A can therefore remain interactive while B prepares, and a
// stale B request cannot poison C's resolver.
export function forProject(root) {
  const key = root || '';
  return Object.freeze({
    root: key,
    start: () => {
      if (!key) return Promise.resolve(null);
      if (failureFor(key)) return Promise.resolve(null);
      return load(key).catch(e => {
        failedRoots.set(key, { why: e.message || String(e), at: Date.now() });
        return null;
      });
    },
    ready: () => readyFor(key),
    status: () => status(key),
    isReady: () => !!(key && manifests.has(key)),
    resolve: (token, baseDir) => resolveFor(key, token, baseDir),
    boundTo: baseDir => token => resolveFor(key, token, baseDir),
    activate: () => setProject(key),
  });
}

// status reports whether references can be resolved for the current project, and
// why not when they cannot. A feature that silently does nothing is
// indistinguishable from a repo with no references in it — the console has to be
// able to say which (INV — honest misses).
export function status(root) {
  const key = root || currentRoot;
  if (!key) return { state: 'none', reason: 'no project for this session' };
  if (manifests.has(key)) return { state: 'ready' };
  const f = failureFor(key);
  if (f) return { state: 'unavailable', reason: f.why, retriesIn: failedTTL - (Date.now() - f.at) };
  return { state: 'indexing' };
}

// isReady reports whether resolve() can answer definitively right now.
export function isReady() {
  return !!(currentRoot && manifests.has(currentRoot));
}

// invalidate drops a cached manifest so the next resolve refetches. The daemon
// has no file-watcher yet (impl-plan R4), so this is how a caller says "the tree
// changed under us".
export function invalidate(root) {
  const key = root || currentRoot;
  if (key) { manifests.delete(key); inflight.delete(key); failedRoots.delete(key); }
}

// load fetches and indexes one manifest, de-duplicating concurrent callers.
function load(root) {
  if (manifests.has(root)) return Promise.resolve(manifests.get(root));
  if (inflight.has(root)) return inflight.get(root);
  const p = api('/api/refs/index?cwd=' + encodeURIComponent(root))
    .then(m => {
      const entry = {
        paths: new Set(m.paths || []),
        ids: new Map(Object.entries(m.ids || {})),
        docs: m.docs || [],
        builtAt: m.built_at,
        truncated: !!m.truncated,
        root: m.root,
      };
      manifests.set(root, entry);
      inflight.delete(root);
      return entry;
    })
    .catch(e => {
      // A failed index must not become a wall of confident "missing". Leave the
      // manifest absent so resolve() keeps answering 'indexing' — unknown, not
      // absent — and let the caller surface the error.
      inflight.delete(root);
      throw e;
    });
  inflight.set(root, p);
  return p;
}

// looksLikeRef reports whether a token is worth resolving at all. Recognition
// only flags; resolve() decides.
export function looksLikeRef(token) {
  if (!token) return false;
  if (/^https?:\/\//i.test(token)) return true;
  if (idShaped.test(token)) return true;
  return pathShaped.test(token) || fileShaped.test(token);
}

// joinPath resolves a relative path against a base directory, collapsing "." and
// "..". Design docs link each other relatively — `[ADR 25](../adr/0025-x.md)` from
// docs/guides/ — so a doc's links only resolve when read against the doc's OWN
// directory, not the repo root.
function joinPath(base, rel) {
  const segs = (base ? base.split('/') : []).concat(rel.split('/'));
  const out = [];
  for (const s of segs) {
    if (!s || s === '.') continue;
    if (s === '..') out.pop();
    else out.push(s);
  }
  return out.join('/');
}

// dirOf returns the directory part of a repo-relative path.
export function dirOf(path) {
  const i = (path || '').lastIndexOf('/');
  return i < 0 ? '' : path.slice(0, i);
}

// boundTo returns a resolver that reads relative references against baseDir —
// what the doc viewer needs so a doc's own links work.
export function boundTo(baseDir) {
  return token => resolve(token, baseDir);
}

// resolve turns a token into { token, kind, state, path, line, reason, defs }.
// Synchronous by design; see the module comment for why 'indexing' exists.
export function resolve(token, baseDir) {
  return resolveFor(currentRoot, token, baseDir);
}

function resolveFor(root, token, baseDir) {
  const raw = (token || '').trim();
  const out = { token: raw, kind: null, state: REF_STATE.unresolved, path: '', line: 0, reason: '' };
  if (!raw) { out.reason = 'empty reference'; return out; }

  if (/^https?:\/\//i.test(raw)) {
    return { ...out, kind: 'url', state: REF_STATE.external, url: raw };
  }
  const idx = root ? manifests.get(root) : null;
  if (!idx) {
    // Not "missing" — we have not looked yet.
    return { ...out, state: REF_STATE.indexing, reason: 'building the reference index…' };
  }
  if (idShaped.test(raw)) return resolveId(idx, out, raw);
  return resolvePath(idx, out, raw, baseDir, root);
}

// normalizeId folds "ADR 0025" / "adr25" / "D18" onto the manifest's key form.
function normalizeId(raw) {
  const adr = /^adr\s*0*(\d+)$/i.exec(raw);
  if (adr) return 'ADR ' + adr[1];
  const d = /^d(\d+)$/i.exec(raw);
  if (d) return 'D' + d[1];
  return raw;
}

function resolveId(idx, out, raw) {
  const id = normalizeId(raw);
  const defs = idx.ids.get(id);
  out.kind = 'id';
  if (!defs || !defs.length) {
    out.state = REF_STATE.unresolved;
    out.reason = id + ' is not defined in any doc in this repo';
    return out;
  }
  out.state = REF_STATE.resolved;
  out.defs = defs;
  out.path = defs[0].path;
  out.line = defs[0].line;
  return out;
}

// KNOWN_EXT is the signal that a slash-separated token is a FILE and not prose.
// Without it, "list/detail", "account/settings" and "Chat/Cowork/Code" — ordinary
// English using a slash for "or" — all read as broken paths and litter the
// transcript with dim spans claiming a file is missing. Observed on a real
// session: 340 such false positives against 123 genuine references.
const KNOWN_EXT = /\.(?:go|js|mjs|css|html|md|json|ya?ml|sh|sql|txt|tsx?|py|rb|php)$/i;

// suffixHits returns tree paths ending in `/<path>`, up to `limit`. ONE home for
// the suffix-match predicate: the bare-filename guard asks for 1 (does any exist?)
// and the ambiguity check asks for 2 (exactly one, or several?). Keeping the rule
// in a single function means a change to it — say handling `.min.js` variants —
// lands in both callers at once instead of drifting.
function suffixHits(idx, path, limit) {
  const hits = [];
  for (const p of idx.paths) {
    if (p.endsWith('/' + path)) { hits.push(p); if (hits.length >= limit) break; }
  }
  return hits;
}

function resolvePath(idx, out, raw, baseDir, root) {
  let path = raw.replace(/^\.\//, '');
  const withLine = /^(.+?):(\d+)$/.exec(path);
  if (withLine) { path = withLine[1]; out.line = parseInt(withLine[2], 10); }

  // An absolute path inside this project is the same file as its repo-relative
  // form; a transcript writes both. The leading slash is sometimes lost in
  // tokenizing, so try both spellings.
  if (root) {
    for (const cand of [path, '/' + path]) {
      if (cand.startsWith(root + '/')) { path = cand.slice(root.length + 1); break; }
    }
  }
  path = path.replace(/^\/+/, '');

  // NOT A REFERENCE vs. A BROKEN REFERENCE — the distinction that keeps the
  // transcript honest. Returning null means "this token was never a reference",
  // and linkify leaves it as plain prose. Only something that genuinely looks
  // like a file earns a dim "missing" marker.
  if (!KNOWN_EXT.test(path) && !idx.paths.has(path)) return null;

  // The same rule, applied to a token with NO directory in it. A bare
  // "something.js" is only a file reference if the tree actually holds one:
  // "Node.js" ends in .js and was being dimmed as a missing file, and so were
  // "package.json" and "plan.md" from projects that have neither. Measured on
  // four real sessions, this class was 86 dim spans — every one of them a claim
  // about a file that was never named.
  //
  // A path WITH a separator keeps the old behaviour: "src/thing.js" is a
  // reference whether or not it resolves, because nobody writes that by
  // accident, and "this file moved" is worth saying.
  if (!path.includes('/') && !idx.paths.has(path) && suffixHits(idx, path, 1).length === 0) return null;

  // Relative to the doc being read, when it does not resolve from the root.
  if (baseDir && !idx.paths.has(path) && (raw.startsWith('.') || !idx.paths.has(path))) {
    const joined = joinPath(baseDir, path);
    if (idx.paths.has(joined)) path = joined;
  }

  out.path = path;
  out.kind = path.endsWith('.md') ? 'doc' : 'path';

  if (idx.paths.has(path)) { out.state = REF_STATE.resolved; return out; }

  // A partial path resolves when exactly one tree path ends with it — transcripts
  // routinely write `js/app.js` for `internal/daemon/static/js/app.js`, and a
  // bare filename for anything. Ambiguity is reported, never guessed: picking
  // between two files is how a link opens the wrong one (mirrors the server).
  const hits = suffixHits(idx, path, 2);
  if (hits.length === 1) { out.path = hits[0]; out.state = REF_STATE.resolved; return out; }
  if (hits.length > 1) {
    out.state = REF_STATE.unresolved;
    out.reason = 'ambiguous: more than one file matches this name';
    return out;
  }
  // The client manifest cannot tell "moved" from "never existed" — only git can,
  // and that is the server's answer. Say the weaker, true thing here; the panel
  // asks the server for the precise state when a reader clicks.
  out.state = REF_STATE.missing;
  out.reason = 'no file at this path in the current tree';
  return out;
}

// resolveOnServer gets the authoritative answer for one token, including the
// stale-vs-missing distinction the client manifest cannot make.
export async function resolveOnServer(token, root) {
  const cwd = root || currentRoot;
  if (!cwd) throw new Error('no project root');
  return api('/api/refs/resolve?cwd=' + encodeURIComponent(cwd) +
    '&token=' + encodeURIComponent(token));
}

// backlinks asks "what else points here" — docs plus cross-session touches.
export async function backlinks(target, root) {
  const cwd = root || currentRoot;
  if (!cwd) throw new Error('no project root');
  return api('/api/refs/backlinks?cwd=' + encodeURIComponent(cwd) +
    '&target=' + encodeURIComponent(target));
}

// doc fetches one markdown document for the in-console viewer.
export async function doc(path, root) {
  const cwd = root || currentRoot;
  if (!cwd) throw new Error('no project root');
  return api('/api/refs/doc?cwd=' + encodeURIComponent(cwd) +
    '&path=' + encodeURIComponent(path));
}

// projectRoot exposes the root the resolver is currently pointed at.
export function projectRoot() { return currentRoot; }
