import { $, el, api, escapeHtml, SEV_CHIP } from "../core.js";
import { mkLoader, withState } from "../ui.js";
import { openSession } from "./sessions.js";
import { railHead, railRow, railEmpty, crumb, headRow, markSelected, setGovernSession, scopeBanner } from "./governance-ui.js";
import { S } from "../state.js";

// predText renders a predicate as text. It escapes its leaves because the result is
// interpolated into innerHTML by every caller — tag keys/values come from the rules
// file and must not be able to inject markup.
function predText(p) {
  if (!p) return '';
  if (p.all) return p.all.map(predText).join(' AND ');
  if (p.any) return p.any.map(predText).join(' OR ');
  if (p.not) return 'NOT ' + predText(p.not);
  if (Object.hasOwn(p, 'matches')) {
    return escapeHtml(p.tag || '') + ' matches /' + escapeHtml(p.matches || '') + '/';
  }
  return escapeHtml(p.tag || '')
    + (Object.hasOwn(p, 'value') ? '=' + escapeHtml(p.value || '') : '');
}

let auditLast = null; // cached run findings, survives leaving/returning to the tab
let auditSel = null;  // selected flagged session key, so a tab switch comes back to it
let auditRun = null;  // the in-flight run ({ctl: AbortController}) — module-scoped so a tab switch cannot forget it

async function renderAudit(container) {
  const main = container || $('#main');
  const side = $('#sidebody');
  main.innerHTML = '<h2>Audit <span class="chip st-draft">policy × real sessions</span></h2>'
    + '<div class="sub">Run a policy across every real harvested session and see which '
    + 'ones it flags — flagged sessions land in the list on the left; click one to read '
    + 'its findings here. <b>Post-hoc detect/report '
    + 'only</b> (harvest reach): these are findings on past sessions, never live blocks. '
    + 'A session with no finding is a <i>weak negative</i> (no declared detector matched), '
    + 'not a clean bill.</div>';
  // ONE breadcrumb per tab, at the top of the centre, rewritten as the selection
  // changes. Two crumbs (a tab one plus a detail one) said the same thing twice.
  let crumbEl = crumb('Audit');
  main.prepend(crumbEl);
  const setCrumb = flagged => {
    const next = crumb('Audit', flagged);
    crumbEl.replaceWith(next); crumbEl = next;
  };

  // the policy being tested
  const rulesBox = el('div'); rulesBox.style.cssText =
    'border:1px solid var(--border);border-radius:8px;padding:12px;margin-bottom:12px';
  main.appendChild(rulesBox);
  await withState(rulesBox, mkLoader('Loading policy…'), () => api('/api/audit/rules'), r => {
    const activeRules = r.rules || [];
    rulesBox.innerHTML = '<div class="sub" style="margin-bottom:8px">active stateful policy — '
      + activeRules.length + ' rule' + (activeRules.length === 1 ? '' : 's') + ' · ' + escapeHtml(r.source || '')
      + ' · <span style="color:var(--dim)">' + escapeHtml(r.reach || '') + '</span></div>'
      + '<div class="sub" style="margin-bottom:8px">' + escapeHtml(r.path || '') + '</div>';
    if (!activeRules.length) {
      rulesBox.appendChild(el('div', 'sub', 'No stateful rules are active, so this run can still classify sessions '
        + 'but has no session-level policy to match. Shipped starter observations apply only when the embedded '
        + 'default is active; a custom rule file replaces that default.'));
    }
    activeRules.forEach(rule => {
      const row = el('div'); row.style.cssText = 'padding:4px 0;font-size:12px';
      // Escaped like the finding cards below. Severity is report metadata; action is
      // the one behavioral vocabulary and remains visible even on post-hoc Audit.
      const label = rule.severity || rule.action || 'observe';
      row.innerHTML = '<span class="chip ' + (SEV_CHIP[rule.severity] || 'st-draft') + '">'
        + escapeHtml(label) + '</span> <b>' + escapeHtml(rule.id || '') + '</b> — '
        + '<span style="color:var(--dim)">IF ' + predText(rule.if) + ' → '
        + escapeHtml(rule.action || 'observe') + '</span>';
      rulesBox.appendChild(row);
    });
  });

  // run control
  const runRow = el('div', 'row');
  const runBtn = el('button', 'btn primary', 'Run against all sessions →');
  const summary = el('span'); summary.style.cssText = 'color:var(--dim);font-size:12px';
  runRow.append(runBtn, summary);
  main.appendChild(runRow);

  const out = el('div'); out.style.marginTop = '10px'; main.appendChild(out);

  // Before a run there is nothing to list, and the rail says exactly that rather
  // than going blank — an empty rail here would read as "no findings", which is a
  // different and much stronger claim than "this has not been run".
  const railBeforeRun = () => {
    side.replaceChildren();
    side.appendChild(railHead('Flagged sessions'));
    side.appendChild(railEmpty('Not run yet. The audit reads every harvested session '
      + 'and reports what the rules WOULD have caught — nothing is listed until you run it.'));
  };

  // A run reads EVERY harvested session off disk. Measured: ~30s over 551 sessions,
  // not the "a few seconds" this used to claim. Three things follow from that being
  // the honest number rather than the hoped-for one:
  //   · the button must disable, or a second click starts a second full scan while
  //     the first is still running (it did, silently);
  //   · the wait needs a live elapsed count, because a frozen label for half a
  //     minute is indistinguishable from a hang;
  //   · it must be cancellable, since the only alternative was reloading the page.
  const cancelBtn = el('button', 'btn', 'Cancel');
  cancelBtn.style.display = 'none';
  runRow.insertBefore(cancelBtn, summary);

  // Sync with any run already in flight. auditRun is MODULE state, not closure
  // state: #sidebody and #main are shared, permanent nodes, and a closure-local
  // guard resets on every tab return — so leaving Audit mid-run and coming back
  // showed a live Run button over a scan still running, and clicking it started a
  // SECOND concurrent full scan (the exact double-scan the guard exists to stop).
  if (auditRun) {
    runBtn.disabled = true;
    cancelBtn.style.display = '';
    cancelBtn.onclick = () => auditRun && auditRun.ctl.abort();
    out.replaceChildren(mkLoader('Evaluating every harvested session — reading each file…'));
    // When the run settles, its own view's nodes are orphaned (a tab switch
    // rebuilds the panel), so its completion paints NOTHING here — without this
    // hook the returned-to view kept a loader and a dead Run button forever.
    // Re-rendering the whole tab is the simplest honest recovery: it paints from
    // auditLast, which the settling run just wrote.
    auditRun.rerender = () => { if (out.isConnected) renderAudit(main); };
  }

  const run = async () => {
    if (auditRun) return; // belt and braces: the button is disabled, but never trust one guard
    const myRun = { ctl: new AbortController() };
    auditRun = myRun;
    const started = Date.now();
    runBtn.disabled = true;
    cancelBtn.style.display = '';
    cancelBtn.onclick = () => myRun.ctl.abort();
    staleNote.remove(); // "showing the last run" beside a LIVE ticker is two contradicting statuses

    const loader = mkLoader('Evaluating every harvested session — reading each file…');
    side.replaceChildren(railHead('Flagged sessions'), railEmpty('Evaluating every harvested session…'));
    out.replaceChildren(loader);
    const tick = setInterval(() => {
      const s = Math.round((Date.now() - started) / 1000);
      if (summary.isConnected) summary.textContent = 'running… ' + s + 's';
    }, 1000);

    const done = () => {
      clearInterval(tick);
      auditRun = null;
      runBtn.disabled = false;
      runBtn.textContent = 'Re-run against all sessions →';
      cancelBtn.style.display = 'none';
    };
    try {
      const data = await api('/api/audit/run', { method: 'POST', signal: myRun.ctl.signal });
      done();
      auditLast = data; auditSel = null;
      // Generation guard on SHARED DOM: `side` is the one #sidebody every surface
      // and tab writes into. If the user navigated away mid-run, `out` left the
      // document — painting into `side` then would clobber the rail of whatever
      // they are looking at now. The result is kept (auditLast) and paints on the
      // next visit; only the LIVE write is skipped.
      if (!out.isConnected) {
        if (myRun.rerender) myRun.rerender(); // a newer Audit view is waiting on this result
        return;
      }
      paint(out, side, summary, data, setCrumb);
    } catch (e) {
      const aborted = e && e.name === 'AbortError';
      done();
      if (!out.isConnected) { // same guard: never repaint a surface we have left
        if (myRun.rerender) myRun.rerender();
        return;
      }
      const message = aborted
        ? 'Run cancelled. Any partial scan was discarded; no partial result is shown.'
        : '✖ the rerun failed: ' + (e && e.message ? e.message : 'unknown error');
      if (auditLast) {
        paint(out, side, summary, auditLast, setCrumb);
        summary.textContent = (aborted ? 'cancelled' : 'rerun failed')
          + ' · showing the last successful result';
        out.prepend(el('div', 'empty', message));
      } else {
        railBeforeRun();
        setCrumb();
        summary.textContent = aborted ? 'cancelled — partial results discarded' : '';
        out.replaceChildren(el('div', 'empty', message));
      }
    }
  };
  runBtn.onclick = run;

  // The "showing the last run" note is created once and REMOVED when a run starts
  // (see run()); the first version left it in place, contradicting the live
  // ticker for the whole re-run and going stale-but-present after it.
  const staleNote = el('div', 'sub');
  staleNote.style.marginTop = '4px';
  staleNote.textContent = 'showing the last run — click Re-run to refresh';

  // persist the last run across tab switches — don't make the user re-scan 9s
  if (auditLast && !auditRun) {
    runBtn.textContent = 'Re-run against all sessions →';
    paint(out, side, summary, auditLast, setCrumb);
    summary.after(staleNote);
  } else if (!auditRun) {
    railBeforeRun();
    out.appendChild(el('div', 'empty', 'Run the audit to see which of your real sessions '
      + 'these rules would have flagged.'));
  }
}

// bySession folds the flat finding list onto the object a human asked about: the
// session. One session flagged by three rules is ONE row in the rail, not three.
function bySession(data) {
  const m = new Map();
  data.findings.forEach(f => {
    const k = f.session.runtime + '|' + f.session.id;
    if (!m.has(k)) m.set(k, { key: k, session: f.session, rules: [] });
    m.get(k).rules.push(f);
  });
  return [...m.values()].sort((a, b) => b.rules.length - a.rules.length);
}

// paint writes both halves of the contract for a completed run: the rail is the
// list of flagged sessions, the centre is the selected one's findings.
function paint(out, side, summary, data, setCrumb) {
  // Name the armed policy explicitly: findings suffixed "(armed)" are the rules
  // the hook actually enforces, dry-run across history — the whole point is that
  // this is the SAME rule, not a copy. Say so, or the loop is invisible.
  const armed = data.live_rules
    ? ' · incl. ' + data.live_rules + ' armed live rule' + (data.live_rules === 1 ? '' : 's') + ' dry-run over history'
    : '';
  const failed = Number.isFinite(data.failed) ? data.failed : Math.max(0, data.total - data.evaluated);
  summary.textContent = data.findings.length + ' findings across '
    + data.evaluated + ' / ' + data.total + ' sessions (' + data.with_tags + ' had any tag'
    + (failed ? ' · ' + failed + ' could not be loaded' : '') + ') · '
    + data.elapsed_ms + 'ms' + armed;

  const groups = bySession(data);
  side.replaceChildren();
  side.appendChild(railHead('Flagged sessions', groups.length));
  out.innerHTML = '';

  if (!groups.length) {
    side.appendChild(railEmpty('Nothing was flagged in ' + data.evaluated + ' sessions. '
      + 'That is a weak negative — no declared detector matched — not proof of a clean history.'));
    out.appendChild(el('div', 'empty', 'No findings. (Remember: a weak negative — '
      + 'no declared detector matched — is not proof of a clean session.)'));
    return;
  }

  const rows = new Map(); // key -> rail row, so a restored selection can be highlighted
  for (const g of groups) {
    const runtime = el('span', 'chip ' + (g.session.runtime === 'codex' ? 'codex' : 'claude'),
      g.session.runtime);
    const row = railRow({
      title: g.session.title || g.session.id,
      dim: !g.session.title,
      meta: [runtime, g.rules.length + (g.rules.length === 1 ? ' finding' : ' findings'),
        (g.session.modified || '').slice(0, 10)],
      onClick: r => { markSelected(r); auditSel = g.key; scopeFromGroup(g); setCrumb(g.session.title || g.session.id); renderFinding(out, g); document.dispatchEvent(new CustomEvent('cg:info')); },
    });
    rows.set(g.key, row);
    side.appendChild(row);
  }

  // The shared surface scope wins over Audit's own last pick. If the scoped session
  // was flagged, select it; if it was NOT flagged, the rail still lists the flagged
  // ones (never silently emptied) and the centre says the scoped session was clean
  // in this run — which is a real, different answer from "not shown".
  const scopeId = S.governSession && S.governSession.id;
  const scopeRuntime = S.governSession && S.governSession.runtime;
  const scoped = scopeId && groups.find(g => g.session.id === scopeId
    && g.session.runtime === scopeRuntime);
  if (scopeId && !scoped) {
    renderUnflagged(out, setCrumb);
    return;
  }
  const keep = scoped || groups.find(g => g.key === auditSel) || groups[0];
  markSelected(rows.get(keep.key));
  auditSel = keep.key;
  setCrumb(keep.session.title || keep.session.id);
  renderFinding(out, keep);
}

// scopeFromGroup promotes a clicked Audit session to the shared surface scope. Its
// tags are the union of what FIRED here — a real subset of the session's tags, which
// is all Policy's best-effort highlight needs.
function scopeFromGroup(g) {
  const tags = [];
  const seen = new Set();
  for (const f of g.rules) for (const t of (f.fired || [])) {
    const k = t.key + '=' + t.value;
    if (!seen.has(k)) { seen.add(k); tags.push({ key: t.key, value: t.value }); }
  }
  // Silent: Audit painted this session's findings itself; the repaint would clobber
  // that. Other tabs read the scope when next shown.
  setGovernSession({ id: g.session.id, runtime: g.session.runtime,
    title: g.session.title, tags }, { silent: true });
}

// renderUnflagged is the centre for a scoped session the last run did NOT flag. The
// rail beside it still lists every flagged session, so nothing is hidden.
function renderUnflagged(out, setCrumb) {
  const s = S.governSession;
  out.innerHTML = '';
  const bar = scopeBanner({ note: 'not flagged in the last run' });
  if (bar) out.appendChild(bar);
  setCrumb(s.title || s.id);
  out.appendChild(el('div', 'empty', 'The scoped session “' + (s.title || s.id) + '” was not '
    + 'flagged by any rule in the last run. That is a weak negative — no declared detector '
    + 'matched — not a clean bill. The flagged sessions are still listed on the left.'));
}

// renderFinding is the centre half: ONE flagged session, its findings, and the tags
// that fired — with the detector provenance on every chip (INV-22: a source-map hit
// and a pattern hit must never look equally trustworthy).
function renderFinding(out, g) {
  const { session, rules } = g;
  out.innerHTML = '';
  // When this session is the shared scope, offer the way out here too — otherwise
  // scoping from Audit would be a one-way door.
  const bar = scopeBanner();
  if (bar) out.appendChild(bar);

  const head = headRow(6);
  head.appendChild(el('span', 'chip ' + (session.runtime === 'codex' ? 'codex' : 'claude'), session.runtime));
  const title = el('span', '', session.title || session.id);
  title.style.cssText = 'font-weight:600;font-size:14px'; head.appendChild(title);
  const when = el('span'); when.style.cssText = 'color:var(--dim);font-size:11px';
  when.textContent = (session.modified || '').slice(0, 10); head.appendChild(when);
  const open = el('button', 'btn', 'Open the session →');
  open.onclick = () => {
    document.querySelector('nav button[data-view="sessions"]').click();
    setTimeout(() => openSession(session, null), 250);
  };
  head.appendChild(open);
  out.appendChild(head);

  out.appendChild(el('div', 'sub', rules.length + (rules.length === 1 ? ' rule' : ' rules')
    + ' flagged this session. Detector kind matters: source and destination come from '
    + 'direct tool channels; pattern or content is a shape matched in input and can fire '
    + 'on example data; unknown stays explicitly unknown. '
    + 'These are findings on a past session — nothing was blocked.'));

  rules.forEach(f => {
    const card = el('div'); card.style.cssText =
      'border:1px solid var(--border);border-radius:8px;padding:10px 12px;margin:8px 0';
    const line = el('div'); line.style.cssText = 'font-size:12px';
    line.innerHTML = '<span class="chip ' + (SEV_CHIP[f.severity] || 'st-draft') + '">'
      + escapeHtml(f.severity || '') + '</span> <b>' + escapeHtml(f.rule || '') + '</b>'
      + ' <span style="color:var(--dim)">— ' + escapeHtml(f.message || '') + '</span>';
    card.appendChild(line);
    const tagWrap = el('div'); tagWrap.style.cssText = 'margin:6px 0 0';
    (f.fired || []).forEach(t => {
      const direct = t.detector_kind === 'source' || t.detector_kind === 'destination';
      const c = el('span', 'chip ' + (direct ? 'cl-observed' : 'st-stale'), t.key + '=' + t.value);
      c.title = (t.detector_kind || 'unknown') + ' · ' + t.detector
        + (t.evidence ? ' · ' + t.evidence : '');
      c.style.marginRight = '4px'; tagWrap.appendChild(c);
    });
    if (!(f.fired || []).length) {
      tagWrap.appendChild(el('span', 'sub', 'No tag was recorded on this finding.'));
    }
    card.appendChild(tagWrap);
    out.appendChild(card);
  });
}

export { renderAudit };
