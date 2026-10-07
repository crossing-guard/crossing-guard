// Pure primitives + shared constants — no view logic, imports nothing.
const $ = sel => document.querySelector(sel);
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
const cpHeaders = () => ({ 'X-CG-Token': localStorage.getItem('cg_token') || localStorage.getItem('cp_token') || '' });
// The auth token arrives once, in the fragment of the printed link (#t=…), and is
// kept in localStorage. It is taken HERE, while this module is evaluated, because
// modules are evaluated dependencies-first: several read the API as they load
// (the rail's settings, the keymap), every one of them imports this module, and
// the entry module's own body runs only after all of them. Taking the token there
// sent those first reads without it on a page opened from its link, and they were
// refused.
const storeTokenFromHash = hash => {
  const found = String(hash || '').match(/[#&]t=([A-Za-z0-9_-]+)/);
  if (!found) return false;
  localStorage.setItem('cg_token', found[1]);
  return true;
};
if (typeof location !== 'undefined' && typeof localStorage !== 'undefined') storeTokenFromHash(location.hash);
// The console dogfoods the versioned contract (D16): a bare /api/… path is rewritten
// to the canonical /api/v1/… so the reference client uses exactly what a BYO-GUI is
// told to build against. The server still serves both, so this is transparent.
const apiPath = path => path.startsWith('/api/') && !path.startsWith('/api/v1/')
  ? path.replace('/api/', '/api/v1/') : path;
const api = async (path, opts = {}) => {
  const r = await fetch(apiPath(path), { ...opts, headers: { ...(opts.headers || {}), ...cpHeaders() } });
  if (!r.ok) {
    const err = new Error((await r.text()).trim() || `HTTP ${r.status}`);
    err.status = r.status;
    throw err;
  }
  return r.json();
};
const fmtTime = iso => { const d = new Date(iso); return isNaN(d) ? '' : d.toLocaleString(); };
const normMemId = id => String(id).replace(/^(bash:)?automem:/, '').replace(/\.md$/i, '').toLowerCase();
const getDefaults = () => JSON.parse(localStorage.getItem('cp_defaults') || '{}');
const setDefaults = d => localStorage.setItem('cp_defaults', JSON.stringify(d));
// formatBytes renders a byte count for a header or a meta line; one rule for
// attachments and the Files pane.
const formatBytes = value => {
  const bytes = Number(value) || 0;
  if (bytes >= 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  if (bytes >= 1024) return (bytes / 1024).toFixed(1) + ' KB';
  return bytes + ' B';
};
const fmtTok = n => n >= 1e9 ? (n / 1e9).toFixed(1) + 'B' : n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1e3 ? (n / 1e3).toFixed(1) + 'k' : String(n);
// Usage formatters (runtime-model-catalog-and-usage design §4): absent is
// "unknown", never 0; a cost prints its own unit code, never an assumed
// currency; a runtime-stated cost says so, because it is not a bill.
const COST_BASIS_LABEL = { runtime: 'runtime-stated', estimated: 'estimated' };
const fmtAmount = amount => {
  const value = Number(amount) || 0;
  return value !== 0 && Math.abs(value) < 0.01 ? value.toFixed(4) : value.toFixed(2);
};
// fmtCost takes one cost ({amount, unit, basis}) or a list of per-unit lines.
const fmtCost = cost => {
  const lines = Array.isArray(cost) ? cost : cost ? [cost] : [];
  if (!lines.length) return 'cost unknown';
  return lines.map(line => fmtAmount(line.amount) + ' ' + String(line.unit || '')).join(' + ');
};
const fmtCostBasis = cost => {
  const lines = Array.isArray(cost) ? cost : cost ? [cost] : [];
  const bases = [...new Set(lines.map(line => COST_BASIS_LABEL[line.basis] || String(line.basis || '')))];
  return bases.join(', ');
};
const fmtLimit = tokens => (Number(tokens) > 0 ? fmtTok(Number(tokens)) + ' ctx' : 'ctx unknown');
// fmtPrice summarises a stated price as "input/output per N"; the full rate
// list belongs in a title or a table.
const fmtPrice = price => {
  if (!price || !Array.isArray(price.rates) || !price.rates.length) return 'price unknown';
  const rate = cls => price.rates.find(item => item.class === cls && item.above_context_tokens == null);
  const input = rate('input'), output = rate('output');
  const per = Number(price.per_tokens) >= 1e6 ? fmtTok(Number(price.per_tokens)) : String(price.per_tokens);
  const parts = [input, output].filter(Boolean).map(item => fmtAmount(item.amount));
  return (parts.length ? parts.join('/') : fmtAmount(price.rates[0].amount)) + ' ' + price.unit + ' per ' + per;
};

/* ---------- markdown (tiny, safe: escapes first) ---------- */
function escapeHtml(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

/* ---------- reference linkify (console-and-info-panel §5 / §7 L0) ----------
   Turns a token that is a REFERENCE — a path, a file:line, a D-id, a URL — into
   something clickable, and anything that does not resolve into a dim, honest
   non-link (INV: never a link that 404s).

   Two rules hold this code together:

   1. ESCAPE FIRST, then inject only the anchors we build ourselves. User turns
      previously rendered via textContent and were injection-safe by construction;
      linkifying them means producing HTML, so the escaping is what keeps that
      safety (impl-plan R1). Never interpolate raw transcript text into markup.
   2. The resolver is INJECTED, not imported. core.js imports nothing, and
      refs.js imports core.js — taking a resolve() argument keeps that one-way
      (impl-plan §4 hazard: do not deepen the module cycle). With no resolver we
      render text, never a guess. */

// Tokens worth resolving. Conservative on purpose: loose path matching turns
// ordinary prose into links (§13 false-positive risk).
const REF_TOKEN = new RegExp([
  /https?:\/\/[^\s<>"')\]]+/.source,                                  // url
  /\b(?:D\d+|ADR\s?\d+)\b/.source,                                    // our ids
  /\/?[\w.@-]+(?:\/[\w.@-]+)+(?::\d+)?/.source,                        // [/]a/b/c.go[:12]
  /\b[\w-]+\.(?:go|js|mjs|css|html|md|json|ya?ml|sh|sql)(?::\d+)?\b/.source, // file.go[:12]
].join('|'), 'g');

const attrEscape = s => String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');

// refMarkup renders one resolved reference. Resolved → anchor carrying the target
// as data-* for the delegated click handler; every miss → a dim span stating WHY,
// which is the whole honesty contract (§5).
function refMarkup(target, shownEscaped) {
  const t = target || {};
  if (t.state === 'external') {
    return `<a class="ref ref-ext" href="${attrEscape(t.url || t.token)}" target="_blank" rel="noopener">${shownEscaped}</a>`;
  }
  if (t.state === 'resolved') {
    return `<a class="ref ref-ok" href="#" data-ref-kind="${attrEscape(t.kind || '')}"` +
      ` data-ref-path="${attrEscape(t.path || '')}" data-ref-line="${attrEscape(t.line || 0)}"` +
      ` data-ref-token="${attrEscape(t.token || '')}">${shownEscaped}</a>`;
  }
  if (t.state === 'indexing') {
    // Stay visually neutral while retaining the exact token so the session can
    // hydrate it in place when its root-bound manifest arrives. A plain string
    // lost that identity and forced either a blocking first paint or a complete
    // transcript re-render.
    return `<span class="ref-pending" data-ref-token="${attrEscape(t.token || '')}">${shownEscaped}</span>`;
  }
  const cls = t.state === 'stale' ? 'ref-stale' : 'ref-miss';
  return `<span class="ref ${cls}" title="${attrEscape(t.reason || t.state || 'unresolved')}">${shownEscaped}</span>`;
}

// DOM counterpart to refMarkup for views that already hold a resolved repository path.
// Resolution and click behavior remain with refs.js and app.js respectively.
function refAnchor(path, label = path) {
  const a = document.createElement('a');
  a.className = 'ref ref-ok';
  a.href = '#';
  a.dataset.refKind = String(path).endsWith('.md') ? 'doc' : 'path';
  a.dataset.refPath = path;
  a.dataset.refLine = '0';
  a.dataset.refToken = path;
  a.textContent = label;
  return a;
}

// linkifyEscaped scans ALREADY-ESCAPED text for reference tokens. Callers must
// have stashed code spans and existing anchors first (see mdInline).
function linkifyEscaped(s, resolve) {
  if (!resolve) return s;
  return s.replace(REF_TOKEN, tok => {
    // Trailing sentence punctuation belongs to the prose, not the reference.
    const trail = /[.,;:)\]]+$/.exec(tok);
    const bare = trail ? tok.slice(0, -trail[0].length) : tok;
    const tail = trail ? trail[0] : '';
    let target;
    try { target = resolve(bare); } catch { return tok; }
    if (!target) return tok;
    return refMarkup(target, bare) + tail;
  });
}

// linkify takes PLAIN text (a user turn), escapes it, and linkifies it.
function linkify(text, resolve) {
  return linkifyEscaped(escapeHtml(String(text ?? '')), resolve);
}

function mdInline(s, resolve) {
  // Stash the spans that must never be re-scanned — inline code and the anchors
  // we build from markdown links — so bare-token linkify cannot match inside
  // them and produce nested markup (impl-plan R1).
  const stash = [];
  const keep = html => `\u0000${stash.push(html) - 1}\u0000`;
  let out = String(s).replace(/\u0000/g, ''); // sentinel hygiene: input must not forge one
  out = out.replace(/`([^`]+)`/g, (_, code) => keep(`<code>${code}</code>`));
  out = out.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_, label, href) => keep(mdLink(label, href, resolve)));
  out = out
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|\W)\*([^*\n]+)\*(?=\W|$)/g, '$1<em>$2</em>');
  out = linkifyEscaped(out, resolve);
  // Restore until stable: a stashed span can CONTAIN another placeholder (a
  // markdown link whose label holds inline code), and String.replace does not
  // rescan what it inserts — one pass left the inner placeholder in the DOM as an
  // invisible NUL and dropped the code span with it.
  for (let pass = 0; pass < 5 && out.includes('\u0000'); pass++) {
    out = out.replace(/\u0000(\d+)\u0000/g, (_, i) => stash[+i] ?? '');
  }
  return out.replace(/\u0000/g, ''); // never leak a sentinel into the DOM
}

// mdLink renders a markdown link. An http(s) target opens in a tab as before; a
// RELATIVE target is resolved against the reference index instead of being
// dropped — the old `https?:` guard rendered `[x](docs/guides/example.md)` as literal
// text, which is the bug that started this whole design (§3).
function mdLink(label, href, resolve) {
  if (/^https?:/i.test(href)) {
    return `<a class="ref ref-ext" href="${attrEscape(href)}" target="_blank" rel="noopener">${label}</a>`;
  }
  if (/^(javascript|data|vbscript):/i.test(href.trim())) return label; // never emit a script URL
  if (!resolve) return label; // cannot verify the target — show the words, not a guess
  const bare = href.replace(/^\.\//, '').replace(/#.*$/, '');
  let target;
  try { target = resolve(bare); } catch { return label; }
  // A href that was never a repo path (an anchor, a bare word) is not a broken
  // reference — it is not a reference. Dimming it would put "no file at this
  // path" under prose like "Node.js" and read as a defect that isn't one.
  if (!target) return label;
  // Name the href in the reason. The reader sees only the LABEL, so "no file at
  // this path" is unreadable without saying which path was meant.
  if (target.state !== 'resolved' && target.state !== 'external' && target.state !== 'indexing') {
    target = { ...target, reason: (target.reason || target.state) + ' → ' + bare };
  }
  return refMarkup(target, label);
}

function mdToHtml(src, resolve) {
  const lines = escapeHtml(src).split('\n');
  let html = '', para = [], list = null, code = null, table = null;
  const flushPara = () => { if (para.length) { html += '<p>' + mdInline(para.join('\n'), resolve) + '</p>'; para = []; } };
  const flushList = () => { if (list) { html += `<${list.tag}>` + list.items.map(i => '<li>' + mdInline(i, resolve) + '</li>').join('') + `</${list.tag}>`; list = null; } };
  // Tables matter here specifically: this repo's tracker docs define every D-item
  // as a table ROW, so without this a reference to D18 lands on one undifferentiated
  // blob of pipe characters instead of its own row.
  const flushTable = () => {
    if (!table) return;
    const cells = row => row.replace(/^\s*\|/, '').replace(/\|\s*$/, '').split('|').map(c => c.trim());
    const [head, ...body] = table;
    html += '<table><thead><tr>' + cells(head).map(c => '<th>' + mdInline(c, resolve) + '</th>').join('') +
      '</tr></thead><tbody>' +
      body.map(r => '<tr>' + cells(r).map(c => '<td>' + mdInline(c, resolve) + '</td>').join('') + '</tr>').join('') +
      '</tbody></table>';
    table = null;
  };
  const flushAll = () => { flushPara(); flushList(); flushTable(); };
  for (const line of lines) {
    if (code) {
      if (/^```/.test(line)) { html += '<pre><code>' + code.body.join('\n') + '</code></pre>'; code = null; }
      else code.body.push(line);
      continue;
    }
    const fence = line.match(/^```(\w*)/);
    if (fence) { flushAll(); code = { body: [] }; continue; }
    const h = line.match(/^(#{1,3})\s+(.*)/);
    if (h) {
      flushAll();
      // Headings carry a slug id so the doc viewer can anchor to a section — a
      // link to "#Dn" needs something to land on, and the renderer emitted no
      // ids at all before (impl-plan R8).
      const depth = h[1].length, id = headingSlug(h[2]);
      html += `<h${depth} id="${attrEscape(id)}">` + mdInline(h[2], resolve) + `</h${depth}>`;
      continue;
    }
    if (/^(---+|\*\*\*+)\s*$/.test(line)) { flushAll(); html += '<hr>'; continue; }
    // A table row: | a | b |. The |---|---| separator is layout, not content.
    if (/^\s*\|.*\|\s*$/.test(line)) {
      flushPara(); flushList();
      if (!/^[\s|:-]+$/.test(line)) (table = table || []).push(line);
      continue;
    }
    flushTable();
    const ul = line.match(/^\s*[-*]\s+(.*)/);
    const ol = line.match(/^\s*\d+[.)]\s+(.*)/);
    if (ul || ol) {
      flushPara();
      const tag = ul ? 'ul' : 'ol';
      if (!list || list.tag !== tag) { flushList(); list = { tag, items: [] }; }
      list.items.push((ul || ol)[1]);
      continue;
    }
    const bq = line.match(/^&gt;\s?(.*)/);
    if (bq) { flushAll(); html += '<blockquote>' + mdInline(bq[1], resolve) + '</blockquote>'; continue; }
    if (line.trim() === '') { flushAll(); continue; }
    para.push(line);
  }
  if (code) html += '<pre><code>' + code.body.join('\n') + '</code></pre>';
  flushAll();
  return html;
}
// headingSlug builds a stable anchor id from heading text. A D-item heading keeps
// its id verbatim ("D5") so a `D5` reference can target it directly; anything
// else becomes a lowercase dashed slug, the convention GitHub established.
function headingSlug(text) {
  const plain = text.replace(/`([^`]+)`/g, '$1').replace(/\*\*?/g, '').trim();
  const id = /^(D\d+|ADR\s?\d+)\b/i.exec(plain);
  if (id) return id[1].replace(/\s+/g, '');
  return plain.toLowerCase().replace(/[^\w\s-]/g, '').trim().replace(/\s+/g, '-').slice(0, 64);
}

/* ---------- helpers ---------- */
function fillSelect(sel, pairs, value) {
  sel.innerHTML = '';
  for (const [v, label, title] of pairs) {
    const op = el('option', '', label);
    op.value = v;
    if (title) op.title = title;
    sel.appendChild(op);
  }
  if (value != null) sel.value = value;
}
function mkSelectKV(pairs, value) {
  const s = el('select');
  fillSelect(s, pairs, value);
  return s;
}
function mkSelect(options, value) {
  const s = el('select');
  for (const o of options) { const op = el('option', '', o || '(default)'); op.value = o; s.appendChild(op); }
  if (value != null) s.value = value;
  return s;
}
const SEV_CHIP = { high: 'st-disputed', medium: 'st-stale', low: 'st-draft' };
// data-class → water-mark ladder (mirrors engine.waterOrder) for the level meter.
const WATER_ORDER = ['public', 'internal', 'source-code', 'customer-data',
  'regulated', 'personal-data', 'credential-material'];
const CLASS_CHIP = v => WATER_ORDER.indexOf(v) >= 4 ? 'st-disputed'
  : WATER_ORDER.indexOf(v) >= 2 ? 'st-stale' : 'st-draft';
function lblWrap(label, input) {
  const f = el('div', 'field');
  f.appendChild(el('label', '', label));
  f.appendChild(input);
  return f;
}
function debounce(fn, ms) { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; }
function shortWhen(ts) { return (ts || '').replace('T', ' ').slice(5, 16); }

export { $, el, cpHeaders, storeTokenFromHash, api, apiPath, fmtTime, formatBytes, normMemId, escapeHtml, mdInline, mdToHtml, linkify, linkifyEscaped, refAnchor, headingSlug, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, fmtCost, fmtCostBasis, fmtLimit, fmtPrice, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults };
