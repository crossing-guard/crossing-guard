import { $, el, cpHeaders, api, fmtTime, normMemId, escapeHtml, mdInline, mdToHtml, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from "../ui.js";
import { S } from "../state.js";
import { provider } from "../infopanel.js";
import { hidePaneHost, showPaneHost } from "../pane-host.js";
import { shareTeamMemory, takeTeamVersion } from "../team-link.js";
import { teamRecordLines, conflictLine, promoteAction } from "./team-memory-text.js";

const BLESSED = ['pending', 'active', 'verified', 'superseded'];
const STATUS_CHIP = { pending: 'st-stale', active: 'st-draft', verified: 'st-verified', superseded: 'st-superseded', rejected: 'st-superseded' };
const CLASS_NOTE = { // honesty: what each classification does and does NOT warrant (INV-1)
  'user-asserted': 'Stated by you. Trusted as intent — not independently verified.',
  'observed': 'Recorded from a session or tool. Evidence-backed.',
  'inferred': 'Derived by the agent. Weakest signal — verify before relying on it.',
  'verified': 'Confirmed against a source.',
};
const verifiedAge = v => { // "2026-07-16 by alice" → "verified 3d · alice"
  const m = String(v).match(/(\d{4}-\d{2}-\d{2})(?:\s+by\s+(.*))?/);
  if (!m) return 'verified';
  const days = Math.max(0, Math.floor((Date.now() - new Date(m[1])) / 86400000));
  return 'verified ' + days + 'd' + (m[2] ? ' · ' + m[2] : '');
};

/* ---------- right reference panel providers (surface 'memory', console-design §5) ----------
   Read-only context that informs the center editor: where the claim came from,
   what its classification does/doesn't warrant, and where it is in its lifecycle. */
provider({
  id: 'memory.provenance', order: 10, title: 'Provenance',
  match: ctx => ctx.surface === 'memory' && !!ctx.selection,
  render: (ctx, box) => {
    const m = ctx.selection;
    const line = (k, v) => { const d = el('div', 'sub'); d.append(el('span', 'chip st-draft', k), ' ' + (v || '—')); box.appendChild(d); };
    line('store', m.store + ' [' + (m.grade || 'fs') + ']');
    line('scope', m.scope || 'global');
    line('sources', (m.sources || []).join(', '));
    line('updated', fmtTime(m.updated_at));
  },
});
// renderTeamFacts states a record's standing with the team and lists its conflict copies.
async function renderTeamFacts(team, box) {
  for (const line of teamRecordLines(team)) box.appendChild(el('div', 'sub', line));
  if (!team.conflicts) return;
  let out;
  try { out = await api('/api/memory/conflicts?id=' + encodeURIComponent(team.global_id)); } catch (_) { return; }
  if (!box.isConnected) return;
  for (const c of out.conflicts || []) {
    const card = el('div', 'banner');
    card.appendChild(el('div', 'sub', conflictLine(c) + ' · ' + fmtTime(new Date(c.created_at / 1e6).toISOString())));
    const body = el('div', 'sub'); body.style.whiteSpace = 'pre-wrap'; body.textContent = c.body;
    card.appendChild(body);
    box.appendChild(card);
  }
}

// Team: where the record stands with the linked team, and the conflict copies kept for it
// (a local version a teammate's revision or deletion displaced — kept, never merged).
provider({
  id: 'memory.team', order: 15, title: 'Team',
  match: ctx => ctx.surface === 'memory' && !!ctx.selection && !!ctx.selection.team,
  render: (ctx, box) => renderTeamFacts(ctx.selection.team, box),
});
provider({
  id: 'memory.classification', order: 20, title: 'Classification',
  match: ctx => ctx.surface === 'memory' && !!ctx.selection,
  render: (ctx, box) => {
    const m = ctx.selection;
    box.appendChild(el('span', 'chip cl-' + m.classification, m.classification));
    box.appendChild(el('div', 'sub', CLASS_NOTE[m.classification] || 'Unrecognized classification.'));
  },
});
provider({
  id: 'memory.lifecycle', order: 30, title: 'Lifecycle',
  match: ctx => ctx.surface === 'memory' && !!ctx.selection,
  render: (ctx, box) => {
    const m = ctx.selection;
    box.appendChild(el('span', 'chip ' + (STATUS_CHIP[m.status] || 'st-draft'), m.status === 'verified' ? verifiedAge(m.verified) : m.status));
    if ((m.aliases || []).length) box.appendChild(el('div', 'sub', 'aliases: ' + m.aliases.join(', ')));
    if ((m.tags || []).length) box.appendChild(el('div', 'sub', 'tags: ' + m.tags.join(', ')));
  },
});

/* ---------- memory (list in the RAIL, detail/actions in the CENTER — §5) ---------- */
// confirmedButton is one action behind a question: asked when there is something to say,
// then run; done repaints, fail says why not.
function confirmedButton(label, question, run, done, fail) {
  const button = el('button', 'btn', label);
  button.onclick = async () => {
    if (question && !confirm(question)) return;
    try { await run(); done(); } catch (e) { fail(e); }
  };
  return button;
}

async function renderMemory() {
  S.selMemory = null; // fresh visit starts with no record selected (right panel hidden)
  const main = $('#main'), side = $('#sidebody');
  main.innerHTML = '<h2>Memory</h2><div class="sub">Evidence-backed claims — status + classification per claim. Select a record to view it; provenance drill-down arrives with the canonical store.</div>';
  const center = el('div'); main.appendChild(center);
  side.innerHTML = '';

  const placeholder = () => { center.innerHTML = ''; center.appendChild(el('div', 'empty', 'Select a memory record from the list, or add a claim.')); };
  placeholder();

  // --- rail: add-claim pin ---
  const addPin = el('div', 'railpin');
  addPin.innerHTML = '<svg class="ico" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 3.5v9M3.5 8h9"/></svg><span>Add claim</span>';
  addPin.onclick = showAdd; side.appendChild(addPin);

  // --- rail: inbox pin (if any pending) ---
  api('/api/memory/pending').then(pend => {
    if (!pend.length) return;
    const pin = el('div', 'railpin');
    pin.innerHTML = '<svg class="ico" viewBox="0 0 16 16" aria-hidden="true"><path d="M2.5 3h11v8H6l-3 2.5V3z"/></svg><span>Inbox</span><span class="chip st-stale">' + pend.length + '</span>';
    pin.onclick = () => showInbox(pend);
    side.insertBefore(pin, listBox);
  }).catch(() => {});

  // --- rail: records list ---
  const listBox = el('div'); side.appendChild(listBox);
  await withState(listBox, mkLoader('Loading memories…'), () => api('/api/memory'), mems => {
    listBox.innerHTML = '';
    if (!mems.length) { listBox.appendChild(el('div', 'empty', 'No memories yet — add the first claim.')); return; }
    let focusRow = null;
    for (const m of mems.sort((a, b) => (b.updated_at || '').localeCompare(a.updated_at || ''))) {
      const row = el('div', 'sess'); row.dataset.memId = normMemId(m.id);
      row.appendChild(el('div', 't', m.claim));
      const meta = el('div', 'm');
      meta.append(el('span', 'chip cl-' + m.classification, m.classification),
        el('span', 'chip ' + (STATUS_CHIP[m.status] || 'st-draft'), m.status),
        el('span', '', m.scope || ''));
      row.appendChild(meta);
      row.onclick = () => showDetail(m, row);
      listBox.appendChild(row);
      if (S.memoryFocus && row.dataset.memId === S.memoryFocus) focusRow = { m, row };
    }
    if (S.memoryFocus) { // consume a jump-from-facts
      S.memoryFocus = null;
      if (focusRow) { focusRow.row.classList.add('flash'); focusRow.row.scrollIntoView({ block: 'center' }); showDetail(focusRow.m, focusRow.row); }
    }
  });

  function showAdd() {
    S.selMemory = null; hidePaneHost(); // an action, not a record selection
    listBox.querySelectorAll('.sess').forEach(x => x.classList.remove('sel'));
    center.innerHTML = '';
    center.appendChild(el('h3', '', 'Add claim'));
    const claim = el('input'); claim.placeholder = 'Claim…'; claim.style.cssText = 'width:100%;max-width:640px;margin:8px 0';
    const cls = mkSelect(['user-asserted', 'observed', 'inferred', 'verified']);
    const scope = el('input'); scope.placeholder = 'scope (repo / global)'; scope.style.width = '200px';
    const add = el('button', 'btn primary', 'Add');
    add.onclick = async () => {
      if (!claim.value.trim()) return;
      await api('/api/memory', { method: 'POST', body: JSON.stringify({ claim: claim.value, classification: cls.value, scope: scope.value || 'global', sources: ['user'] }) });
      renderMemory();
    };
    const row = el('div', 'row'); row.append(cls, scope, add);
    center.append(claim, row);
  }

  function showDetail(m, rowEl) {
    listBox.querySelectorAll('.sess').forEach(x => x.classList.remove('sel'));
    if (rowEl) rowEl.classList.add('sel');
    S.selMemory = m; showPaneHost({ surface: 'memory', selection: m, api }); // right panel follows the record
    center.innerHTML = '';
    center.appendChild(el('h3', '', m.claim));
    const meta = el('div', 'row');
    meta.append(el('span', 'chip cl-' + m.classification, m.classification),
      el('span', 'chip ' + (m.store === 'crossing-guard' ? 'cl-observed' : 'st-draft'), m.store + ' [' + (m.grade || 'fs') + ']'),
      el('span', 'sub', 'scope: ' + (m.scope || '—') + ' · ' + fmtTime(m.updated_at)));
    center.appendChild(meta);
    if (m.body) { const b = el('div', 'sub'); b.style.whiteSpace = 'pre-wrap'; b.textContent = m.body; center.appendChild(b); }
    if ((m.aliases || []).length) center.appendChild(el('div', 'sub', 'aliases: ' + m.aliases.join(', ')));
    if ((m.tags || []).length) center.appendChild(el('div', 'sub', 'tags: ' + m.tags.join(', ')));
    const sr = el('div', 'row'); sr.style.marginTop = '10px'; sr.append(el('span', 'sub', 'status:'));
    if (m.store === 'crossing-guard') {
      sr.append(el('span', 'chip ' + (STATUS_CHIP[m.status] || 'st-draft'), m.status === 'verified' ? verifiedAge(m.verified) : m.status));
      if (!m.body) { // lazy full dossier body from the engine
        const bx = el('div', 'sub'); bx.style.whiteSpace = 'pre-wrap'; bx.textContent = 'Loading full record…'; center.appendChild(bx);
        api('/api/memory/record?id=' + encodeURIComponent(m.id))
          .then(rec => { bx.textContent = (rec.body || '(no body)') + (rec.verified ? '\n\nverified: ' + rec.verified : ''); })
          .catch(e => { bx.textContent = '✖ ' + e.message; });
      }
    } else {
      const opts = BLESSED.includes(m.status) ? BLESSED : [m.status, ...BLESSED];
      const sel = mkSelect(opts, m.status);
      sel.onchange = async () => { m.status = sel.value; await api('/api/memory', { method: 'POST', body: JSON.stringify(m) }); };
      sr.append(sel);
    }
    center.appendChild(sr);
    if (m.team) teamActions(m);
  }

  // Team actions on one record: promote a record rejected here, or share one that has
  // not been shared. Sending a version that differs from the team's is said before the click.
  function teamActions(m) {
    const facts = el('div'); facts.style.marginTop = '10px';
    center.appendChild(facts);
    renderTeamFacts(m.team, facts);
    const acts = el('div', 'row'); acts.style.marginTop = '8px';
    const fail = e => acts.appendChild(el('span', 'sub', '✖ ' + e.message));
    if (m.team.rejected_here) {
      const action = promoteAction(m.team);
      acts.appendChild(confirmedButton(action.label, action.confirm,
        () => api('/api/memory/promote', { method: 'POST', body: JSON.stringify({ id: m.id }) }), renderMemory, fail));
    } else if (!m.team.shared && m.team.can_travel && m.status !== 'pending') {
      acts.appendChild(confirmedButton('Share with the team',
        'Send this record to the team? It is checked against the secret patterns and matches redacted first. Every member\'s devices receive it.',
        () => shareTeamMemory([m.id]), renderMemory, fail));
    }
    if (m.team.refused && !m.team.in_sync) {
      acts.appendChild(confirmedButton('Take the team\'s version',
        'Replace this record with the team\'s current version? The edit made on this device is kept as a conflict copy and is not sent.',
        () => takeTeamVersion(m.id), renderMemory, fail));
    }
    if (acts.childNodes.length) center.appendChild(acts);
  }

  function showInbox(pend) {
    S.selMemory = null; hidePaneHost(); // an action, not a record selection
    listBox.querySelectorAll('.sess').forEach(x => x.classList.remove('sel'));
    center.innerHTML = '';
    center.appendChild(el('h3', '', 'Inbox — ' + pend.length + ' pending proposal' + (pend.length === 1 ? '' : 's')));
    center.appendChild(el('div', 'sub', 'Captured candidates. Nothing is projected into agent context until you promote it.'));
    for (const p of pend) {
      const card = el('div', 'banner'); card.style.borderLeftColor = 'var(--accent2)';
      const head = el('div', 'row');
      head.append(el('span', 'chip st-stale', 'pending [pkg]'), el('span', 'chip cl-' + (p.classification || 'user-asserted'), p.classification || 'user-asserted'),
        el('span', '', p.id + ' · ' + fmtTime(p.created_at)));
      card.appendChild(head);
      card.appendChild(el('div', '', p.claim));
      const acts = el('div', 'row'); acts.style.marginTop = '8px';
      const promote = el('button', 'btn primary', 'Promote to store');
      promote.onclick = async () => { promote.disabled = true; try { await api('/api/memory/promote', { method: 'POST', body: JSON.stringify({ id: p.id }) }); renderMemory(); } catch (e) { promote.disabled = false; card.appendChild(el('div', 'sub', '✖ ' + e.message)); } };
      const reason = el('input'); reason.placeholder = 'reject reason…'; reason.style.width = '220px';
      const reject = el('button', 'btn', 'Reject');
      reject.onclick = async () => { reject.disabled = true; try { await api('/api/memory/reject', { method: 'POST', body: JSON.stringify({ id: p.id, reason: reason.value }) }); renderMemory(); } catch (e) { reject.disabled = false; card.appendChild(el('div', 'sub', '✖ ' + e.message)); } };
      acts.append(promote, reject, reason); card.appendChild(acts);
      center.appendChild(card);
    }
  }
}

export { renderMemory };
