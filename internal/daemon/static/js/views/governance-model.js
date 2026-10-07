// Governance › the data model, made visible (console-governance-ia-plan §6, Phase D).
//
// Why this ships WITH the rail work and not after it (red-team R9): two of the six
// reported confusions — "when I click a row, what is it telling me?" and "how do we
// explain the hierarchy?" — are CONCEPTUAL, not structural. Giving every tab a rail
// fixes where things are; it does not tell a reader what a Detector is, or why a
// Session is the heart, or that Project is a display grouping rather than a stored
// thing. That is what this module says, in the panel, next to live counts.
//
// The model has TWO axes and confusing them is the whole problem:
//   DATA    — what happened:  Event → Session → Project (a cwd grouping) → Entity
//   CONTROL — what you enforce: Detector → Tag → Rule → Decision
// Capture is the data axis; Policy/Audit/Approvals are the control axis.
//
// Honesty: every number here is READ from an endpoint, and anything we cannot count
// says so rather than showing a plausible zero (INV-21/INV-22). In particular there
// is no global entity count — entities are counted per selected session — and
// "Project" has no count at all, because it is not stored (governance-model.md).
import { el, api } from "../core.js";
import { provider } from "../infopanel.js";

// A node in one axis: the noun, one plain sentence, and a count when we honestly
// have one. `count === null` renders "—" with a reason on hover, never "0".
function node(box, { name, count, countNote, what }) {
  const row = el('div'); row.style.cssText =
    'display:flex;gap:8px;align-items:baseline;padding:5px 0;border-bottom:1px solid var(--border)';
  const n = el('span', '', name);
  n.style.cssText = 'font-weight:600;font-size:var(--fs-8);flex:0 0 78px';
  row.appendChild(n);
  const c = el('span', '', count === null || count === undefined ? '—' : count.toLocaleString());
  c.style.cssText = 'font-family:var(--mono);font-size:var(--fs-7);color:var(--dim);flex:0 0 62px;text-align:right';
  if (countNote) c.title = countNote;
  row.appendChild(c);
  const w = el('span', 'sub', what);
  w.style.flex = '1';
  row.appendChild(w);
  box.appendChild(row);
}

function axis(box, title, sub) {
  const h = el('div'); h.style.cssText = 'margin:10px 0 2px';
  const t = el('span', '', title);
  t.style.cssText = 'font-size:var(--fs-6);font-weight:600;letter-spacing:.04em;text-transform:uppercase';
  h.appendChild(t);
  box.appendChild(h);
  box.appendChild(el('div', 'sub', sub));
}

provider({
  // order 20 puts this AFTER governance.capture (order 10), so selecting a session
  // still opens on that session's capture summary — the model is one click away in
  // the module row, not a surprise replacement for what the panel used to show.
  id: 'governance.model', order: 20, title: 'Data model',
  // Matches the whole surface, selection or not: a reader who has not clicked
  // anything yet is exactly the reader who needs this.
  match: ctx => ctx.surface === 'governance',
  render: async (ctx, box) => {
    box.appendChild(el('div', 'sub', 'Two axes. What happened, and what you enforce over '
      + 'it. Every governance tab sits on one of them.'));

    // Fetch what we can; each answer is independent, so one failure must not blank
    // the others — a missing count is stated, not silently rendered as zero.
    const [health, sessions, rules, detectors] = await Promise.all([
      api('/api/govern/health').catch(() => null),
      api('/api/govern/sessions').catch(() => null),
      api('/api/policy/rules').catch(() => null),
      api('/api/policy/detectors').catch(() => null),
    ]);
    // The panel may have moved on while those were in flight (R4). The host detaches
    // the old box, so painting is harmless — but bail rather than do the work.
    if (!box.isConnected) return;

    const sel = ctx.selection; // a Capture session report, when one is selected
    const entityCount = sel && sel.found
      ? new Set((sel.events || []).map(e => e.target_entity_id).filter(Boolean)).size
      : null;
    const ruleCount = rules && rules.doc && Array.isArray(rules.doc.rules)
      ? rules.doc.rules.length : null;
    const detectorCount = detectors && detectors.runtime_governor
      ? detectors.runtime_governor.detector_count : null;

    axis(box, 'Data — what happened',
      'Captured by the governor on every hooked tool call. This is the record, not a re-scan.');
    node(box, { name: 'Event', count: health ? health.total_events : null,
      countNote: health ? 'every governed tool call ever captured on this machine'
        : 'capture health could not be read',
      what: 'one governed tool call — the primary truth' });
    node(box, { name: 'Session', count: sessions ? sessions.length : null,
      countNote: sessions
        ? 'sessions we hold captured rows for — not every harvested session, and the '
          + 'endpoint caps the list at 500, so a 500 here would mean "at least 500"'
        : 'the captured-session list could not be read',
      what: 'one conversation — events fold onto it. The heart of the model' });
    node(box, { name: 'Project', count: null,
      countNote: 'Not counted because it is not stored: a project is a cwd prefix we group by '
        + 'for display. Filtering by it is Phase B.',
      what: 'a cwd grouping for display — NOT a stored entity' });
    node(box, { name: 'Entity', count: entityCount,
      countNote: entityCount === null
        ? 'Counted per session, not globally. Select a session in Capture to see its distinct targets.'
        : 'distinct targets touched by the selected session',
      what: 'the file / url / mcp / db a session touched' });

    axis(box, 'Control — what you enforce',
      'Config evaluated by one engine. Hook attachment and firing are separate observed facts, not proved here.');
    node(box, { name: 'Detector', count: detectorCount,
      countNote: detectorCount === null ? 'the active detector assembly could not be read'
        : 'running governor/hook assembly · ' + detectors.runtime_governor.digest
          + (detectors.restart_required ? ' · configured bytes differ; restart required' : ''),
      what: 'config that turns an event into a tag' });
    node(box, { name: 'Tag', count: sel && sel.found ? (sel.state || []).length : null,
      countNote: sel && sel.found ? 'facts folded onto the selected session'
        : 'Counted per session. Select a session in Capture.',
      what: 'a fact a detector asserted — fs=edit, phase=red-team' });
    node(box, { name: 'Rule', count: ruleCount,
      countNote: ruleCount === null ? 'the policy file could not be read'
        : 'rules active in the engine policy — vendor-hook reach is not demonstrated here',
      what: 'a predicate over tags → allow / ask / deny' });
    node(box, { name: 'Decision', count: null,
      countNote: 'Recorded on events; there is no aggregate endpoint for it yet. '
        + 'Per-event decisions are on the Capture tab, and held ones on Approvals.',
      what: 'what the engine did — plus a hold, when the action is ask' });

    // Where you are standing, said in the model's own words. This is the sentence
    // that answers "why do tabs on the right change what's on the left?".
    const tab = ctx.tab || 'capture';
    const WHERE = {
      capture: 'You are on the DATA axis: the rail lists sessions we captured, the centre lists their events.',
      policy: 'You are on the CONTROL axis: the rail lists rules, the centre is where you author them.',
      audit: 'You are on the CONTROL axis, run backwards over the data axis: rules × every harvested session, report only.',
      approvals: 'You are on the CONTROL axis: the record of decisions a rule held for a human.',
      ledger: 'Developer sandbox — not part of either axis.',
    };
    const here = el('div', 'sub');
    here.style.cssText = 'margin-top:10px;border-left:2px solid var(--accent);padding-left:8px';
    here.textContent = WHERE[tab] || WHERE.capture;
    box.appendChild(here);

    box.appendChild(el('div', 'sub', 'The active rulebook is machine-wide for events that reach the engine — '
      + 'there is no per-project on/off switch yet. This panel does not prove which vendor '
      + 'surfaces currently reach it.'));
  },
});
