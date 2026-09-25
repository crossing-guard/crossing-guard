// Governance › Capture — what the governor actually recorded (Track 3 Step 2).
//
// The capture has been running and writing PRIMARY TRUTH for days with no console
// surface at all: `grep api/govern internal/daemon/static/` returned nothing. This
// is that surface, and it is deliberately a READ of the event log — not a re-scan
// of vendor files. The rail lists sessions we hold rows for (which is not the same
// set as the harvested session list), the center shows one session's events, and
// the right panel says what we did NOT capture.
//
// INV-21 throughout: every empty region states why it is empty. The API was changed
// to make that possible — a bare [] could not distinguish "never observed" from
// "observed, nothing folded" (see console-track3-implementation.md Step 2).
import { $, el, api, shortWhen } from "../core.js";
import { mkLoader, withState } from "../ui.js";
import { S } from "../state.js";
import { provider } from "../infopanel.js";
import { railHead, railRow, railEmpty, crumb, markSelected, headRow, setGovernSession } from "./governance-ui.js";
import { openEntity } from "./governance-entity.js";
import { openSession } from "./sessions.js";

// Absent is UNKNOWN, never "allowed". Every event before commit 00032f0 carries an
// empty decision permanently, and rendering those as allowed would be a lie about
// enforcement in exactly the surface people will check it in.
const DECISION_CHIP = { allow: 'cl-observed', deny: 'st-disputed', ask: 'st-stale' };
const decisionChip = d => {
  const c = el('span', 'chip ' + (DECISION_CHIP[d] || 'st-draft'), d || 'unknown');
  if (!d) c.title = 'No decision was recorded for this event. Absent means UNKNOWN — '
    + 'not "allowed". Events captured before decision recording shipped have none.';
  return c;
};

// The target as a human asked about it, not as we key it.
const shortTarget = id => {
  if (!id) return '';
  if (id.startsWith('file:')) return id.slice(5).split('/').slice(-2).join('/');
  return id;
};

let sel = null;      // selected session id — module-local, survives keep-alive

// ago renders a unix ts as a human age. The capture-liveness detector (D13) is a
// timestamp: silence is the symptom of every silent-loss path, so "last captured N
// ago" is the honest health line, not a fabricated green light.
function ago(ts) {
  if (!ts) return 'never';
  const s = Math.max(0, Math.floor(Date.now() / 1000) - ts);
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

const STALE_SECONDS = 15 * 60; // idle this long → say "no recent capture", do not cry broken

// The liveness strip is time-sensitive AND the Governance view is keep-alive cached,
// so returning to it via nav restores a STALE strip — "last event 3s ago" frozen from
// minutes back, the exact freshness-lie D13 exists to kill. A single module-level poll
// re-fetches while the strip is connected to the DOM, and no-ops when it is not (the
// user left Capture). Mirrors app.js's 30s approvals-badge refresh; not a per-render
// timer, so nothing leaks.
let livenessEl = null;
async function refreshLiveness() {
  if (!livenessEl || !livenessEl.isConnected) return;
  try { renderLiveness(livenessEl, await api('/api/govern/health')); } catch { /* transient */ }
}
setInterval(refreshLiveness, 30000);
// Refresh immediately when a keep-alive governance pane is restored (nav-away/return),
// so the strip never shows a frozen age. No-ops if the strip isn't on screen.
document.addEventListener('cg:refresh-liveness', refreshLiveness);

// renderLiveness draws the capture-health strip (D13). It escalates: a governor that
// never opened, or observations that could not be persisted, is a RED failure — the
// silent-loss class D2 hid for four days. A merely quiet log is AMBER and says
// "could be idle", because idleness is a legitimate cause and a false alarm here
// trains the reader to ignore the real one.
function renderLiveness(box, h) {
  box.replaceChildren();
  const strip = el('div'); strip.style.cssText =
    'border-radius:8px;padding:8px 12px;margin-bottom:10px;font-size:12px;border:1px solid var(--border)';
  const now = Math.floor(Date.now() / 1000);
  const stale = h.last_event_ts && (now - h.last_event_ts) > STALE_SECONDS;

  let tone, msg;
  if (!h.configured) {
    tone = 'st-disputed';
    msg = '⚠ The governor is not configured — nothing is being captured. Every tool '
      + 'call is running ungoverned; the console below shows only what was captured before.';
  } else if (h.observe_failures > 0) {
    tone = 'st-disputed';
    msg = '⚠ ' + h.observe_failures + ' observation' + (h.observe_failures === 1 ? '' : 's')
      + ' could not be persisted since the daemon started (a full disk does this). '
      + 'Capture is lossy right now — rows are being dropped.';
  } else if (!h.total_events) {
    tone = 'st-stale';
    msg = 'No events have ever been captured. If agents are running, capture is not '
      + 'reaching the daemon.';
  } else if (stale) {
    tone = 'st-stale';
    msg = 'Last capture ' + ago(h.last_event_ts) + ' — no recent activity. This is '
      + 'normal if no agent is working; it is a problem only if one is.';
  } else {
    tone = 'cl-observed';
    msg = 'Capturing — last event ' + ago(h.last_event_ts) + ' · '
      + h.total_events.toLocaleString() + ' events on record.';
  }
  const badge = el('span', 'chip ' + tone, !h.configured || h.observe_failures ? 'DEGRADED'
    : (!h.total_events || stale ? 'QUIET' : 'LIVE'));
  badge.style.marginRight = '8px';
  strip.append(badge, document.createTextNode(msg));
  box.appendChild(strip);
  renderCoverage(box);
}

// renderCoverage reconciles the two session populations the console shows without
// ever explaining: Sessions lists everything harvested from the vendors' own files,
// Capture lists only what the governor OBSERVED. The second is always smaller,
// because governance starts the day it was installed and no earlier — and a reader
// who is not told that reads the shortfall as missing data.
//
// Deliberately non-blocking and best-effort: the harvest count needs a full scan,
// so the rail paints first and this line appears when the number arrives. If it
// never arrives, no sentence is better than a guessed one.
function renderCoverage(box) {
  const line = el('div', 'sub');
  line.style.marginTop = '6px';
  box.appendChild(line);
  Promise.all([api('/api/govern/sessions'), api('/api/sessions')])
    .then(([captured, harvested]) => {
      if (!line.isConnected) return; // generation guard: the tab may have been left
      const c = captured.length, t = harvested.length;
      if (!t) return;
      const before = Math.max(0, t - c);
      line.textContent = c.toLocaleString() + ' of ' + t.toLocaleString()
        + ' sessions are governed. The other ' + before.toLocaleString()
        + ' predate capture on this machine and can never be captured — '
        + 'governance records what happens after it is installed, not what happened before.';
    })
    .catch(() => { if (line.isConnected) line.remove(); });
}

/* ---------- right panel: what we captured, and what we did not ---------- */
provider({
  id: 'governance.capture', order: 10, title: 'Capture',
  match: ctx => ctx.surface === 'governance' && !!ctx.selection,
  render: (ctx, box) => {
    const r = ctx.selection;
    if (!r.found) {
      box.appendChild(el('div', 'sub', r.note || 'No governance rows for this session.'));
      return;
    }
    const line = (label, value) => {
      const d = el('div', 'row'); d.style.marginBottom = '4px';
      d.append(el('span', 'sub', label), el('span', '', String(value)));
      box.appendChild(d);
    };
    line('events', r.event_count + (r.truncated ? ' (capped — PARTIAL)' : ''));
    line('state facts', r.state.length);

    // Counts by verb: the cheap honest summary of what this session did.
    const byVerb = {};
    for (const e of r.events) byVerb[e.verb || '?'] = (byVerb[e.verb || '?'] || 0) + 1;
    const verbs = el('div'); verbs.style.margin = '6px 0';
    for (const [v, n] of Object.entries(byVerb).sort((a, b) => b[1] - a[1])) {
      verbs.appendChild(el('span', 'chip st-draft', v + ' · ' + n));
    }
    box.appendChild(verbs);

    if (!r.decisions_recorded) {
      box.appendChild(el('div', 'sub', 'No decision is recorded on any event here. That means '
        + 'UNKNOWN, not "nothing was blocked" — this session predates decision recording.'));
    }
    if (r.truncated) {
      box.appendChild(el('div', 'sub', 'The event list hit its cap. What you see is a PREFIX '
        + 'of this session, not all of it.'));
    }
    // State the boundary. A capture view that only shows what it caught invites the
    // reader to treat it as complete coverage.
    box.appendChild(el('div', 'sub', 'Not captured: anything outside a hooked tool call — '
      + 'the agent\'s reasoning, tool calls made while the daemon was unreachable, and '
      + 'every session that ran before the governor was installed.'));
  },
});

/* ---------- center: one session's events ---------- */
function renderReport(center, r) {
  center.replaceChildren();
  center.appendChild(crumb('Capture', r.title || r.id));
  if (!r.found) {
    center.appendChild(el('div', 'empty', r.note
      || 'No governance rows captured for this session.'));
    return;
  }
  const head = headRow();
  if (r.runtime) head.appendChild(el('span', 'chip ' + r.runtime, r.runtime));
  const t = el('span', '', r.title || r.id);
  t.style.cssText = 'font-weight:600;font-size:14px';
  if (r.title_source === 'prompt') { // INV-22, same label as the session list
    t.style.fontStyle = 'italic'; t.style.opacity = '.72';
    t.title = 'No title authored by the runtime — showing the first prompt, truncated.';
  }
  head.appendChild(t);
  head.appendChild(el('span', 'chip st-draft', r.event_count + ' events'));
  if (r.truncated) head.appendChild(el('span', 'chip st-disputed', 'PARTIAL — capped'));
  // The way back to reading it. Capture's centre IS a session view, so it owes the
  // same exit Audit already offers — otherwise governance is a one-way door.
  if (r.id && r.runtime) {
    const open = el('button', 'btn', 'Open the session →');
    open.title = 'Read this session\'s transcript on the Sessions surface';
    open.onclick = () => {
      document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'sessions' }));
      // The Sessions surface needs a paint before it can select into; same short
      // hand-off delay Audit and Usage use.
      setTimeout(() => openSession({ id: r.id, runtime: r.runtime, title: r.title }, null), 250);
    };
    head.appendChild(open);
  }
  center.appendChild(head);

  // Folded state: what we now believe about this session, and which detector said so.
  if (r.state.length) {
    const st = el('div'); st.style.marginBottom = '10px';
    st.appendChild(el('div', 'sub', 'Folded state — what we believe about this session:'));
    for (const s of r.state) {
      const c = el('span', 'chip cl-observed', s.key + '=' + s.value);
      c.title = s.detector + (s.evidence ? ' · ' + s.evidence : '');
      c.style.marginRight = '4px';
      st.appendChild(c);
    }
    center.appendChild(st);
  } else {
    center.appendChild(el('div', 'sub', 'Events were captured but no state was folded — '
      + 'no detector matched anything this session did.'));
  }

  // The events themselves, newest last (the order they happened).
  const tbl = el('div');
  tbl.appendChild(el('div', 'sub', 'Events, in the order they were observed:'));
  for (const e of r.events) {
    const row = el('div'); row.style.cssText =
      'display:flex;gap:8px;align-items:baseline;padding:3px 0;border-bottom:1px solid var(--border);font-size:12px';
    const when = el('span', '', shortWhen(new Date(e.ts * 1000).toISOString()));
    when.style.cssText = 'color:var(--dim);flex:0 0 84px';
    row.append(when, decisionChip(e.decision));
    row.appendChild(el('span', 'chip st-draft', e.verb || '?'));
    const tool = el('span', '', e.tool || ''); tool.style.fontWeight = '600';
    row.appendChild(tool);
    if (e.target_entity_id) {
      // The target is clickable: it opens that entity's governance record in the
      // right panel (what it is, its folded facts, which sessions touched it). Only
      // things we can look up by id are linked — a bare command has no entity. It is
      // a button, not an <a>: there is no href to navigate to, and a hrefless anchor
      // is neither keyboard-focusable nor Enter-activatable — worse than a button.
      const linkable = /^(file|url|mcp|db):/.test(e.target_entity_id);
      const tg = el('span', linkable ? 'entlink' : '', shortTarget(e.target_entity_id));
      tg.style.cssText = 'overflow:hidden;text-overflow:ellipsis;white-space:nowrap'
        + (linkable ? '' : ';color:var(--dim)');
      tg.title = linkable ? 'Show what the governor knows about ' + e.target_entity_id : e.target_entity_id;
      if (linkable) {
        tg.setAttribute('role', 'button');
        tg.tabIndex = 0;
        const open = () => openEntity(e.target_entity_id);
        tg.onclick = open;
        tg.onkeydown = ev => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); open(); } };
      }
      row.appendChild(tg);
    }
    if (e.reason) { const rs = el('span', 'sub', '— ' + e.reason); row.appendChild(rs); }
    tbl.appendChild(row);
  }
  center.appendChild(tbl);
}

/* ---------- rail: sessions we actually hold rows for ---------- */
// captureRow builds one rail entry through the SHARED railRow, so Capture cannot
// drift from the shape Policy/Audit/Approvals use (the whole reason governance-ui
// exists). The initial .sel class is left to select()→markSelected, not set here.
function captureRow(g, center) {
  const meta = [];
  if (g.runtime) meta.push(el('span', 'chip ' + g.runtime, g.runtime));
  meta.push(g.events + ' events');
  meta.push(shortWhen(new Date(g.last_seen * 1000).toISOString()));
  return railRow({
    title: g.title || g.session_id,
    dim: g.title_source === 'prompt', // one honesty label, one look
    titleAttr: g.title_source === 'prompt'
      ? 'No title authored by the runtime — showing the first prompt, truncated.' : undefined,
    meta,
    onClick: d => select(g.session_id, center, d),
  });
}

let selGen = 0; // guards against out-of-order responses painting the wrong session
async function select(id, center, row) {
  sel = id;
  const my = ++selGen;
  markSelected(row);
  await withState(center, mkLoader('Reading the event log…'),
    () => api('/api/govern/session?section=capture&id=' + encodeURIComponent(id)),
    r => {
      // A newer selection already started — a slow earlier fetch must NOT overwrite the
      // center + right panel with a session the rail no longer shows (INV-22). Same
      // generation-guard pattern as infopanel.js.
      if (my !== selGen) return;
      const capture=r.capture || {};
      renderReport(center, capture);
      S.selGovern = capture;
      // Promote this session to the SHARED surface scope, carrying its folded tags
      // so Policy can highlight the rules those tags could trip. Silent: Capture just
      // painted this session, so it must not ask the surface to repaint Capture over
      // itself — other tabs read the new scope when next shown.
      setGovernSession({ id, runtime: capture.runtime, title: capture.title,
        title_source: capture.title_source, tags: capture.state || [] }, { silent: true });
      document.dispatchEvent(new CustomEvent('cg:info'));
    });
}

async function renderCapture(container) {
  const center = container || $('#main');
  const side = $('#sidebody');
  center.replaceChildren();
  side.replaceChildren();

  // Capture-liveness strip first (D13): whether capture is happening at all comes
  // before what it captured. A green console over a dead pipeline was a real bug.
  const live = el('div'); center.appendChild(live);
  livenessEl = live; // the poll re-renders THIS element while it stays in the DOM
  api('/api/govern/health')
    // Bail if a fast tab-switch already detached the strip, matching select()'s
    // generation-guard discipline — no painting into a node that has left the DOM.
    .then(h => { if (live.isConnected) renderLiveness(live, h); })
    .catch(e => { if (live.isConnected) live.replaceChildren(el('div', 'sub', '✖ could not read capture health: ' + (e.message || e))); });

  // The "what this is" line now lives in the tab strip above; keep only the one
  // fact worth repeating here — that this is our own log, not a re-scan — as a
  // quiet aside rather than a second headline.
  const body = el('div'); body.style.marginTop = '10px'; center.appendChild(body);

  await withState(side, mkLoader('Loading captured sessions…'),
    () => api('/api/govern/sessions'),
    list => {
      side.replaceChildren();
      side.appendChild(railHead('Captured sessions', list.length));
      if (!list.length) {
        side.appendChild(railEmpty('No sessions have been observed yet. The governor '
          + 'records a row on each hooked tool call; if this stays empty while agents are '
          + 'running, capture is not reaching the daemon.'));
        return;
      }
      const rows = list.map(g => captureRow(g, body));
      rows.forEach(r => side.appendChild(r));
      // The shared surface scope wins over Capture's own last pick: if another tab
      // (or a prior visit) scoped to a session, select THAT one here so the surface
      // agrees with itself.
      if (S.governSession) sel = S.governSession.id;
      // Restore the selection across tab switches and navigation.
      const keep = list.findIndex(g => g.session_id === sel);
      if (keep >= 0) select(list[keep].session_id, body, rows[keep]);
      else if (S.governSession) {
        // A session is scoped, but it holds no captured rows here (e.g. it was
        // scoped from a flagged Audit session Codex never hooked). Say that rather
        // than silently showing the blank picker.
        body.appendChild(crumb('Capture', S.governSession.title || S.governSession.id));
        body.appendChild(el('div', 'empty', 'The scoped session “'
          + (S.governSession.title || S.governSession.id) + '” has no captured rows — '
          + 'Capture only holds sessions the governor observed live. It may still appear '
          + 'in Audit (harvested) or Approvals.'));
      } else {
        body.appendChild(crumb('Capture'));
        body.appendChild(el('div', 'empty', 'Select a session on the left to see what was captured for it.'));
      }
    });
}

// Clear the surface's selection when the Capture tab is left, so the right panel
// does not keep describing a session the user can no longer see.
function clearCaptureSelection() { S.selGovern = null; }

export { renderCapture, clearCaptureSelection };
