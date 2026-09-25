import { $, el, cpHeaders, api, fmtTime, normMemId, escapeHtml, mdInline, mdToHtml, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from "../ui.js";
import { railHead, railRow, railEmpty, crumb, markSelected, scopeBanner } from "./governance-ui.js";
import { S } from "../state.js";

// ONE rule format (ADR 0025): a rule is {id, action, if:<predicate>}. The console
// authors the common case — command-regex guards — as a predicate over the eval-only
// `command` tag, so it writes the same format the hook evaluates. A rule's several
// patterns become an OR; a single pattern is a bare term. Detector-tag predicate trees
// (session:*/target:*) are a Phase-3 authoring surface, not V1's.
const COMMAND_TAG = 'command';

// One action → one chip class, shared by the rail list and the Check result below.
const ACTION_CHIP = { allow: 'st-verified', ask: 'st-stale', deny: 'st-disputed', observe: 'cl-observed' };

// predTagKeys walks a predicate and collects every tag KEY it references. Key-level,
// not a full evaluation: Policy's session highlight is a BEST-EFFORT "this rule reads
// a tag your session has", not a claim the rule would fire (a `not` inverts, a
// `matches` may miss). Labelled as such in the UI.
function predTagKeys(p, out = new Set()) {
  if (!p || typeof p !== 'object') return out;
  if (p.tag) out.add(p.tag);
  for (const k of ['all', 'any']) if (Array.isArray(p[k])) p[k].forEach(t => predTagKeys(t, out));
  if (p.not) predTagKeys(p.not, out);
  return out;
}

// sessionTagKeys is the set of tag keys the scoped session has. Detector tags fold as
// `session:phase`, `fs`, `vcs`…, and rules may name either the bare key or the
// `session:`-prefixed form, so both are added.
function sessionTagKeys() {
  const s = S.governSession;
  const keys = new Set();
  for (const t of (s && s.tags) || []) {
    if (!t || !t.key) continue;
    keys.add(t.key);
    keys.add('session:' + t.key.replace(/^session:/, ''));
    keys.add(t.key.replace(/^session:/, ''));
  }
  return keys;
}

// ruleTouchesSession: does this rule read any tag key the scoped session has? Command
// guards (the `command` tag) are excluded — they read the shell string, not session
// state, so "applies to this session" is not a meaningful question for them.
function ruleTouchesSession(rule, sessKeys) {
  for (const k of predTagKeys(rule.if)) if (k !== COMMAND_TAG && sessKeys.has(k)) return true;
  return false;
}

// patternsOf reads the editable command-regex list from a rule in EITHER format: the
// new `if` predicate, or an old format-2 `guards` array (a file not yet re-saved).
function patternsOf(rule) {
  if (Array.isArray(rule.guards)) return rule.guards.map(g => g.pattern || ''); // legacy, still editable
  const p = rule.if;
  if (!p) return [];
  if (p.any) return p.any.filter(t => t && t.tag === COMMAND_TAG).map(t => t.matches || '');
  if (p.tag === COMMAND_TAG) return [p.matches || ''];
  return []; // a non-command predicate (Phase 3) — not editable in this list view
}

// setPatterns writes the command-regex list back onto the rule as the one format,
// dropping any legacy `guards` field so a save never emits both shapes.
function setPatterns(rule, patterns) {
  delete rule.guards;
  const terms = patterns.map(pat => ({ tag: COMMAND_TAG, matches: pat }));
  rule.if = terms.length === 1 ? terms[0] : { any: terms };
}

// isCommandPredicate POSITIVELY identifies the only shapes the pattern-list editor can
// safely round-trip: absent, a single {tag:command, matches:…} term, or an `any` of
// such terms. Everything else — an `all`/`not` tree, a `value`-based (exact-match) term,
// or any detector-tag predicate — returns false and is shown READ-ONLY, so editing can
// never silently flatten it. This is a positive allow-list on purpose: the old deny-list
// (`nonCommandPredicate`) failed to recognize `all`/`not`, rendered them as an empty
// pattern box, and setPatterns then destroyed the tree on the first edit.
function isCommandPredicate(p) {
  if (!p) return true; // no predicate yet — safe to author as a fresh pattern list
  if (Array.isArray(p.all) || p.not) return false; // a tree the list can't represent
  if (p.any) return p.any.every(t => t && t.tag === COMMAND_TAG && t.value === undefined);
  return p.tag === COMMAND_TAG && p.value === undefined;
}

// nonCommandPredicate reports a rule whose predicate the list editor must NOT touch (a
// Phase-3 detector-tag rule, an all/not tree, or an exact-value term). Shown read-only
// rather than silently flattened on save.
function nonCommandPredicate(rule) {
  if (Array.isArray(rule.guards)) return false; // legacy format-2, editable via patternsOf
  return !isCommandPredicate(rule.if);
}

/* ---------- policy ----------
   RAIL SHAPE — the R8 pressure test, decided (console-governance-ia-plan §3, §10.5).
   The contract's rail=list / center=one-detail split was designed for LONG lists
   (sessions, hundreds). Policy has four rules, and each one is a small card, so
   splitting them into rail-plus-one-editor would have added a click to reach every
   rule and shown LESS than the single pane already shows. Applied dogmatically it
   would make the tab worse.

   So Policy keeps its single-pane center and the rail is a NAVIGATOR over it: the
   rules, labeled and selectable, jumping to the card in place. That satisfies what
   the contract actually asks for — no blank left, labeled, selectable — without
   paying for a split the list is too short to earn. The center also holds three
   more sections (Test a command and Recent decisions), so a jump list has a real
   job the moment the page is longer than a screen. */
async function renderPolicy(container, mutationNotice) {
  const main = container || $('#main');
  const side = $('#sidebody');
  main.innerHTML = '<h2>Policy — the rules you set</h2><div class="sub">'
    + 'Each rule has an <b>action</b> — allow, <b>hold</b> (ask a human), <b>deny</b>, or <b>observe</b> (report only) — and a <b>condition</b>. '
    + 'Two kinds of condition: a <b>command guard</b> matches a shell command (e.g. block <code>rm -rf</code>); '
    + 'a <b>workflow rule</b> gates on what the session has already done (e.g. hold a commit with no red-team). '
    + 'Saved rules become active in the same engine the hooks call. This screen does not prove that a vendor hook is attached or firing.</div>';

  if (mutationNotice) {
    const notice = el('div', 'banner');
    notice.appendChild(el('div', '', mutationNotice.title || 'Policy state changed'));
    for (const fact of mutationNotice.facts || []) notice.appendChild(el('div', 'sub', fact));
    main.appendChild(notice);
  }

  // The breadcrumb states the scope in the center, where you act — not just in the
  // tab strip you clicked a moment ago. It names the selected rule once one is picked.
  let crumbEl = crumb('Policy');
  main.prepend(crumbEl);
  const setCrumb = ruleId => {
    const next = crumb('Policy', ruleId);
    crumbEl.replaceWith(next);
    crumbEl = next;
  };

  // When the surface is scoped to a session, Policy stays a full authoring surface —
  // it never HIDES a rule (that would be silent filtering on the one screen where you
  // must see every rule) — but it says which session is in scope and HIGHLIGHTS the
  // rules that session's tags could trip. Best-effort, and labelled so.
  if (S.governSession) {
    const bar = scopeBanner({ note: 'highlighting rules its tags could trip (best-effort)' });
    if (bar) crumbEl.after(bar);
  }

  // --- DETECTOR ASSEMBLY FACTS ---
  // Rules consume tags; this block shows the exact assemblies that produce those tags
  // before the rule editor. It deliberately renders loader facts and ID diffs only — no
  // inferred risk, recommendation, or confidence score.
  main.appendChild(el('h2', '', 'Detector assembly — what derives facts'));
  const detectorWrap = el('div');
  main.appendChild(detectorWrap);
  await withState(detectorWrap, mkLoader('Loading detector assembly facts…'),
    () => api('/api/policy/detectors'), status => {
      if (!main.isConnected) return;
      const appendInvalidRecovery = parent => {
        if (!status.invalid_selection || !status.recovery_state_token) return;
        parent.appendChild(el('div', 'sub', 'selected digest: ' + status.invalid_selection.digest
          + ' · source: ' + status.invalid_selection.source));
        const recover = el('button', 'btn', 'Archive invalid selection and return to cohort baseline');
        recover.onclick = async () => {
          recover.disabled = true;
          try {
            const out = await api('/api/policy/detectors/selection/unselect', { method: 'POST', body: JSON.stringify({
              expected_state_token: status.recovery_state_token,
            }) });
            document.dispatchEvent(new CustomEvent('cg:govern-refresh', { detail: {
              title: 'Invalid detector selection archived', facts: ['prior digest: ' + status.invalid_selection.digest,
                out.displaced_by_invocation
                  ? 'activation: restart without ' + out.invocation_path
                  : 'runtime activation: daemon restart required'],
            } }));
          } catch (e) {
            parent.appendChild(el('div', 'sub', '✖ ' + e.message));
            const reload = el('button', 'btn', 'Reload Policy and review again');
            reload.onclick = () => document.dispatchEvent(new CustomEvent('cg:govern-refresh'));
            parent.appendChild(reload);
          }
        };
        parent.appendChild(recover);
      };
      if (!status.available) {
        const unavailable = el('div', 'banner');
        unavailable.appendChild(el('div', 'chip st-disputed', 'DETECTOR CONFIGURATION UNAVAILABLE'));
        unavailable.appendChild(el('div', 'sub', 'ledger/audit: ' + (status.ledger_error || 'unknown')));
        unavailable.appendChild(el('div', 'sub', 'governor/hook: ' + (status.governor_error || 'unknown')));
        const runtimeFacts = el('table');
        runtimeFacts.appendChild(el('caption', 'sub', 'Startup-cached assemblies still running'));
        const runtimeHead = el('tr');
        for (const label of ['Surface', 'Present', 'Origin / selection', 'Count', 'Digest', 'Source']) {
          const heading = el('th', '', label); heading.scope = 'col'; runtimeHead.appendChild(heading);
        }
        runtimeFacts.appendChild(runtimeHead);
        for (const [surface, loaded] of [['ledger + audit', status.runtime_ledger],
          ['governor + hook', status.runtime_governor]]) {
          const row = el('tr');
          row.append(el('td', '', surface), el('td', '', String(Boolean(loaded))),
            el('td', '', loaded ? (loaded.origin + ' / ' + loaded.selection) : 'unavailable'),
            el('td', '', loaded ? String(loaded.detector_count) : 'unknown'),
            el('td', '', loaded?.digest || 'unknown'),
            el('td', '', loaded?.path || (loaded ? 'embedded bytes' : 'unavailable')));
          runtimeFacts.appendChild(row);
        }
        unavailable.appendChild(runtimeFacts);
        appendInvalidRecovery(unavailable);
        detectorWrap.appendChild(unavailable);
        return;
      }
      const facts = el('table');
      facts.appendChild(el('caption', 'sub', 'Configured and startup-cached detector bytes'));
      facts.innerHTML += '<tr><th>State</th><th>Surface</th><th>Origin / selection</th><th>Count</th><th>Digest</th><th>Source</th></tr>';
      for (const heading of facts.querySelectorAll('tr:first-child th')) heading.scope = 'col';
      const unavailableFact = { origin: 'unavailable', selection: 'unavailable',
        detector_count: 'unknown', digest: 'unknown', path: '' };
      const configured = [
        ['configured', 'ledger + audit', status.durable_ledger],
        ['configured', 'governor + hook', status.durable_governor],
      ];
      const invocationEffective = status.invocation_override ? [
        ['invocation-effective', 'ledger + audit', status.ledger],
        ['invocation-effective', 'governor + hook', status.governor],
      ] : [];
      const running = [
        ['running since daemon start', 'ledger + audit', status.runtime_ledger],
        ['running since daemon start', 'governor + hook', status.runtime_governor],
      ];
      for (const [state, surface, loaded] of [...configured, ...invocationEffective, ...running]) {
        const fact = loaded || unavailableFact;
        const row = el('tr');
        row.append(el('td', '', state), el('td', '', surface),
          el('td', '', (fact.origin || 'unknown') + ' / ' + (fact.selection || 'unknown')),
          el('td', '', String(fact.detector_count)),
          el('td', '', fact.digest || 'unknown'),
          el('td', '', fact.path || (loaded ? 'embedded bytes' : 'unavailable')));
        facts.appendChild(row);
      }
      detectorWrap.appendChild(facts);
      const durable = status.durable_governor;
      if (durable) {
        const basis = el('div', 'sub', 'cohort=' + durable.install_cohort
          + ' · legacy compatibility digest=' + durable.available_starter_digest
          + ' · structural present=' + (durable.structural_present || []).length
          + ' · structural missing=' + ((durable.structural_missing || []).join(', ') || 'none'));
        detectorWrap.appendChild(basis);
      } else {
        const invalid = el('div', 'banner', 'DURABLE CONFIGURATION UNAVAILABLE — '
          + (status.durable_error || status.durable_ledger_error || 'unknown'));
        appendInvalidRecovery(invalid);
        detectorWrap.appendChild(invalid);
      }
      if (status.invocation_override) {
        const override = el('div', 'banner');
        override.appendChild(el('div', 'chip st-stale', 'INVOCATION OVERRIDE'));
        override.appendChild(el('div', 'sub', 'path=' + status.invocation_path));
        override.appendChild(el('div', 'sub', 'Durable selections can be recorded, but remain displaced until the daemon starts without this override.'));
        if (status.governor.displaced_selection) {
          override.appendChild(el('div', 'sub', 'displaced durable digest=' + status.governor.displaced_selection.digest));
        }
        if (status.governor.displaced_selection_error) {
          override.appendChild(el('div', 'sub', 'displaced durable selection error=' + status.governor.displaced_selection_error));
        }
        detectorWrap.appendChild(override);
      }
      if (status.configured_divergence) {
        detectorWrap.appendChild(el('div', 'banner', 'DIVERGENT CONFIGURATION — the two historical surfaces resolve different digests. Selection is the only convergence action.'));
      }
      if (status.restart_required) {
        detectorWrap.appendChild(el('div', 'banner', 'RESTART REQUIRED — durable selection differs from the assemblies cached by this daemon process.'));
      }

      detectorWrap.appendChild(el('div', 'sub', 'Detectors produce facts. Select rules separately to configure consequences. Security observation needs both security-observe documents; selecting detectors alone blocks no egress. Detector changes require a daemon restart.'));
      const controls = el('div', 'row'); controls.style.marginTop = '10px';
      const review = el('div');
      if (durable) {
        for (const [source, label] of [['current', 'current'], ['portable-floor', 'portable floor'],
          ['security-observe', 'security observation'], ['legacy', 'legacy compatibility']]) {
          const button = el('button', 'btn', 'Review detector ' + label);
          button.onclick = () => reviewDetectorSelection(source);
          controls.appendChild(button);
        }
      }
      if (durable && durable.selected && durable.selection === 'explicit-user') {
        const unselect = el('button', 'btn', 'Review unselect');
        unselect.onclick = () => reviewDetectorUnselect(durable);
        controls.appendChild(unselect);
      }
      review.setAttribute('aria-live', 'polite');
      detectorWrap.append(controls, review);

      async function reviewDetectorSelection(source) {
        review.replaceChildren(mkLoader('Computing exact detector ID changes…'));
        try {
          const preview = await api('/api/policy/detectors/selection/preview', {
            method: 'POST', body: JSON.stringify({ source }),
          });
          if (!main.isConnected) return;
          const box = el('div', 'banner');
          const table = el('table');
          table.appendChild(el('caption', 'sub', 'Detector selection review'));
          const rows = [
            ['source', preview.source + (preview.source_path ? ' · ' + preview.source_path : '')],
            ['reviewed durable base digest', preview.expected_active_digest],
            ['proposed digest', preview.proposed_digest],
            ['detector count', String(preview.detector_count)],
            ['added IDs', (preview.added || []).join(', ') || 'none'],
            ['removed IDs', (preview.removed || []).join(', ') || 'none'],
            ['replaced IDs', (preview.replaced || []).join(', ') || 'none'],
            ['semantic reach', preview.semantic_reach],
          ];
          for (const [key, value] of rows) {
            const row = el('tr'); const heading = el('th', '', key); heading.scope = 'row';
            row.append(heading, el('td', '', value)); table.appendChild(row);
          }
          const confirm = el('button', 'btn primary', 'Acknowledge digest and select');
          confirm.style.marginTop = '10px';
          confirm.onclick = async () => {
            confirm.disabled = true;
            try {
              const out = await api('/api/policy/detectors/selection', { method: 'POST', body: JSON.stringify({
                source, expected_active_digest: preview.expected_active_digest,
                expected_state_token: preview.expected_state_token,
                acknowledged_digest: preview.proposed_digest,
              }) });
              document.dispatchEvent(new CustomEvent('cg:govern-refresh', { detail: {
                title: 'Detector selection recorded',
                facts: ['selected digest: ' + preview.proposed_digest,
                  out.displaced_by_invocation
                    ? 'activation: displaced by ' + out.invocation_path + '; restart without the override'
                    : 'runtime activation: daemon restart required',
                  ...(out.selection?.recovered_document_archive
                    ? ['archived invalid document: ' + out.selection.recovered_document_archive] : [])],
              } }));
            } catch (e) {
              box.appendChild(el('div', 'sub', '✖ ' + e.message));
              const reload = el('button', 'btn', 'Reload Policy and review again');
              reload.onclick = () => document.dispatchEvent(new CustomEvent('cg:govern-refresh'));
              box.appendChild(reload);
            }
          };
          box.append(table, confirm); review.replaceChildren(box);
        } catch (e) {
          if (main.isConnected) review.replaceChildren(el('div', 'banner', '✖ ' + e.message));
        }
      }

      function reviewDetectorUnselect(active) {
        const box = el('div', 'banner');
        const table = el('table');
        table.appendChild(el('caption', 'sub', 'Detector unselect review'));
        for (const [key, value] of [['selected digest', active.digest],
          ['cohort baseline', active.install_cohort === 'c5c-mechanism-first' ? 'structural floor' : 'legacy compatibility assembly'],
          ['runtime activation', status.invocation_override
            ? 'restart without ' + status.invocation_path
            : 'daemon restart required']]) {
          const row = el('tr'); const heading = el('th', '', key); heading.scope = 'row';
          row.append(heading, el('td', '', value)); table.appendChild(row);
        }
        const confirm = el('button', 'btn primary', 'Archive selection and unselect');
        confirm.style.marginTop = '10px';
        confirm.onclick = async () => {
          confirm.disabled = true;
          try {
            const out = await api('/api/policy/detectors/selection/unselect', { method: 'POST',
              body: JSON.stringify({ expected_state_token: active.state_token }) });
            document.dispatchEvent(new CustomEvent('cg:govern-refresh', { detail: {
              title: 'Detector selection archived', facts: ['baseline: ' + active.install_cohort,
                out.displaced_by_invocation
                  ? 'activation: restart without ' + out.invocation_path
                  : 'runtime activation: daemon restart required'],
            } }));
          } catch (e) {
            box.appendChild(el('div', 'sub', '✖ ' + e.message));
            const reload = el('button', 'btn', 'Reload Policy and review again');
            reload.onclick = () => document.dispatchEvent(new CustomEvent('cg:govern-refresh'));
            box.appendChild(reload);
          }
        };
        box.append(table, confirm); review.replaceChildren(box);
      }
    });

  // --- 1. LIVE RULES (the human-editable policy) ---
  // The rail says what it is BEFORE the rules land, so the left is never blank and
  // never a bare gap while the fetch is in flight (no dead air, contract §2.4).
  side.replaceChildren(railHead('Rules'), railEmpty('Loading rules from the engine…'));
  const rulesWrap = el('div');
  main.appendChild(rulesWrap);
  // On failure the rail must stop saying "Loading…" — a frozen loading state is a
  // claim that the fetch is still running. The centre owns the Retry (withState).
  const loadRules = () => api('/api/policy/rules').catch(e => {
    if (main.isConnected) {
      side.replaceChildren(railHead('Rules'),
        railEmpty('✖ could not load rules: ' + (e.message || e) + ' — retry in the centre pane.'));
    }
    throw e;
  });
  await withState(rulesWrap, mkLoader('Loading rules from the engine…'), loadRules, res => {
    if (!main.isConnected) return;
    const provenance = el('div', 'sub');
    rulesWrap.appendChild(provenance);
    let activeStatus = res;
    const paintProvenance = status => {
      activeStatus = status;
      provenance.replaceChildren(
        el('span', '', 'active=' + String(status.active)),
        el('span', '', ' · origin=' + (status.origin || 'unknown')),
        el('span', '', ' · selection=' + (status.selection || 'unknown')),
        el('span', '', ' · path=' + status.path),
        el('span', '', ' · digest=' + (status.digest || 'unknown') + ' · '),
        el('span', 'chip st-stale', status.label),
      );
    };
    paintProvenance(res);

    const selectionWrap = el('div', 'banner');
    const selectionReview = el('div');
    rulesWrap.append(selectionWrap, selectionReview);
    const renderSelectionStatus = status => {
      selectionWrap.replaceChildren();
      const facts = el('table');
      const factRows = [
        ['safety starter digest', status.available_starter_digest || 'unknown'],
        ['selected', String(Boolean(status.selected))],
        ['active', String(Boolean(status.active))],
        ['selection', status.selection || 'unknown'],
        ['selector', status.selector || 'none'],
        ['selected source', status.selected_source || 'none'],
        ['source reference', status.selected_source_ref || 'none'],
        ['compatibility', status.compatibility || 'none'],
        ['install cohort', status.install_cohort || 'unknown'],
      ];
      if (status.displaced_selection_error) {
        factRows.push(['displaced selection error', status.displaced_selection_error]);
      }
      for (const [key, value] of factRows) {
        const row = el('tr'); row.append(el('th', '', key), el('td', '', value)); facts.appendChild(row);
      }
      selectionWrap.appendChild(facts);
      selectionWrap.appendChild(el('div', 'sub', 'Rules configure consequences. Safety starter adds command guards; security observation includes those guards plus a report-only credential rule. That observation also needs the separately selected security-observe detectors. Each selection replaces one complete document.'));
      const actions = el('div', 'row'); actions.style.marginTop = '10px';
      if (status.selection === 'invocation-path') {
        actions.appendChild(el('span', 'chip st-stale', 'invocation override — Policy mutations disabled'));
        const displaced = status.displaced_selection;
        if (displaced) actions.appendChild(el('span', 'sub', 'durable selection displaced: ' + displaced.digest));
      } else {
        for (const [source, label] of [['current', 'current'], ['safety-starter', 'safety starter'],
          ['security-observe', 'security observation'], ['none', 'no rules'], ['starter', 'legacy compatibility']]) {
          const review = el('button', 'btn', 'Review rules ' + label);
          review.onclick = () => reviewSelection(source, false);
          actions.appendChild(review);
        }
        if (status.selection === 'explicit-user') {
          const rollback = el('button', 'btn', 'Review unselect to cohort baseline');
          rollback.onclick = () => reviewSelection('legacy', true);
          actions.appendChild(rollback);
        }
      }
      selectionWrap.appendChild(actions);
    };
    const reviewSelection = async (source, rollback) => {
      selectionReview.replaceChildren(mkLoader('Computing exact selection facts…'));
      try {
        const preview = await api('/api/policy/rules/selection/preview', {
          method: 'POST', body: JSON.stringify({ source }),
        });
        selectionReview.replaceChildren();
        const box = el('div', 'banner');
        box.appendChild(el('div', '', (rollback ? 'ROLLBACK REVIEW' : 'SELECTION REVIEW')));
        const table = el('table');
        const rows = [
          ['source', preview.source + (preview.source_path ? ' · ' + preview.source_path : '')],
          ['current digest', preview.expected_active_digest],
          ['proposed digest', preview.proposed_digest],
          ['rules', String(preview.rule_count) + ' · ' + (preview.action_summary || 'none')],
          ['added IDs', (preview.added || []).join(', ') || 'none'],
          ['removed IDs', (preview.removed || []).join(', ') || 'none'],
          ['semantic reach', preview.semantic_reach],
          ['recovery', rollback
            ? 'crossing-guard rules select PATH'
            : 'crossing-guard rules unselect'],
        ];
        if (rollback) rows.push(['reselection PATH', preview.active_path]);
        for (const [key, value] of rows) {
          const row = el('tr'); row.append(el('th', '', key), el('td', '', value)); table.appendChild(row);
        }
        box.appendChild(table);
        for (const change of (preview.changed || [])) {
          box.appendChild(el('div', 'sub', 'changed ' + change.id + ': action ' +
            (change.before_action || 'none') + ' → ' + (change.after_action || 'none') +
            ' · predicate ' + change.before_predicate + ' → ' + change.after_predicate));
        }
        const acknowledge = el('button', 'btn primary', rollback
          ? 'Acknowledge digest and rollback' : 'Acknowledge digest and select');
        acknowledge.style.marginTop = '10px';
        acknowledge.onclick = async () => {
          acknowledge.disabled = true;
          try {
            if (rollback) {
              const out = await api('/api/policy/rules/selection/unselect', {
                method: 'POST', body: JSON.stringify({ expected_state_token: preview.expected_state_token }),
              });
              document.dispatchEvent(new CustomEvent('cg:govern-refresh', { detail: {
                title: 'Rulebook selection archived',
                facts: [
                  'active digest: ' + (out.active?.digest || 'unknown'),
                  'archived selection: ' + (out.archive || 'unknown'),
                  'prior selected digest: ' + preview.expected_active_digest,
                  'reselect command: crossing-guard rules select PATH',
                  'reselection PATH: ' + preview.active_path,
                ],
              } }));
            } else {
              await api('/api/policy/rules/selection', {
                method: 'POST', body: JSON.stringify({ source,
                  expected_active_digest: preview.expected_active_digest,
                  expected_state_token: preview.expected_state_token,
                  acknowledged_digest: preview.proposed_digest }),
              });
              document.dispatchEvent(new CustomEvent('cg:govern-refresh', { detail: {
                title: 'Selection completed',
                facts: ['selected digest: ' + preview.proposed_digest, 'source: ' + preview.source],
              } }));
            }
          } catch (e) {
            box.appendChild(el('div', 'sub', '✖ ' + e.message));
            const reload = el('button', 'btn', 'Reload Policy and review again');
            reload.onclick = () => document.dispatchEvent(new CustomEvent('cg:govern-refresh'));
            box.appendChild(reload);
          }
        };
        box.appendChild(acknowledge);
        selectionReview.appendChild(box);
      } catch (e) {
        selectionReview.replaceChildren(el('div', 'banner', '✖ ' + e.message));
      }
    };
    renderSelectionStatus(res);
    const doc = res.doc || { rules: [] };

    const cards = el('div');
    const cardOf = new Map(); // rule -> its card, so the rail can jump to it
    const renderCards = () => {
      cards.innerHTML = '';
      cardOf.clear();
      const sessKeys = S.governSession ? sessionTagKeys() : null;
      for (const rule of (doc.rules || [])) {
        const card = el('div', 'banner');
        cardOf.set(rule, card);
        const inScope = sessKeys && ruleTouchesSession(rule, sessKeys);
        // A scoped session dims the rules it can't trip, so the ones it can stand out
        // — highlight without hiding (the whole rule set stays on screen and editable).
        card.style.borderLeftColor = 'var(--accent)';
        if (sessKeys) card.style.opacity = inScope ? '1' : '.5';
        const head = el('div', 'row');
        const idInput = el('input'); idInput.value = rule.id || ''; idInput.style.width = '220px'; idInput.title = 'rule id';
        idInput.oninput = () => { rule.id = idInput.value; paintRail(); };
        const act = mkSelect(['ask', 'deny', 'allow', 'observe'], rule.action || 'ask');
        act.onchange = () => { rule.action = act.value; paintRail(); };
        act.className = 'act-' + (rule.action || 'ask');
        const del = el('button', 'btn', 'Remove rule');
        del.onclick = () => { doc.rules = doc.rules.filter(x => x !== rule); renderCards(); };
        // Name the KIND on the card, so the two very different conditions are not
        // mistaken for one — a command guard reads a shell string, a workflow rule
        // reads the session's history. Without this they look identical.
        const isWorkflow = nonCommandPredicate(rule);
        const kind = el('span', 'chip ' + (isWorkflow ? 'cl-observed' : 'st-draft'),
          isWorkflow ? 'workflow rule' : 'command guard');
        kind.title = isWorkflow
          ? 'Gates on what this session has already done (folded state).'
          : 'Matches the shell command an agent is about to run.';
        head.append(lblWrap('id', idInput), lblWrap('action', act), kind);
        if (inScope) {
          const sc = el('span', 'chip cl-observed', 'in scope');
          sc.title = 'Reads a tag the scoped session has — could apply here (best-effort).';
          head.append(sc);
        }
        head.append(del);
        card.appendChild(head);
        const intent = el('input'); intent.value = rule.intent || ''; intent.placeholder = 'intent — the human sentence this rule enforces';
        intent.style.width = '100%'; intent.oninput = () => rule.intent = intent.value;
        card.appendChild(lblWrap('intent (plain English)', intent));

        if (nonCommandPredicate(rule)) {
          // A Phase-3 detector-tag rule: not representable in the pattern list, so
          // show it read-only rather than flatten it into a command regex on save.
          const ro = el('div', 'sub');
          ro.innerHTML = 'Predicate over folded state (not a command regex) — edit as JSON, '
            + 'not yet supported in this view: <code>' + escapeHtml(JSON.stringify(rule.if)) + '</code>';
          card.appendChild(ro);
          cards.appendChild(card);
          continue;
        }

        const patterns = patternsOf(rule);
        const gt = el('table');
        gt.innerHTML = '<tr><th>pattern (regex on the command)</th><th></th></tr>';
        patterns.forEach((pat, pi) => {
          const tr = el('tr');
          const gp = el('input'); gp.value = pat; gp.style.width = '100%'; gp.style.fontFamily = 'var(--mono)'; gp.style.fontSize = '12px';
          gp.oninput = () => { patterns[pi] = gp.value; setPatterns(rule, patterns); };
          const gd = el('button', 'btn', '✕'); gd.title = 'remove pattern';
          gd.onclick = () => { patterns.splice(pi, 1); setPatterns(rule, patterns); renderCards(); };
          const td1 = el('td'); td1.appendChild(gp); const td2 = el('td'); td2.appendChild(gd);
          tr.append(td1, td2); gt.appendChild(tr);
        });
        card.appendChild(gt);
        const addG = el('button', 'btn', '+ pattern');
        addG.style.marginTop = '6px';
        addG.onclick = () => { patterns.push(''); setPatterns(rule, patterns); renderCards(); };
        card.appendChild(addG);
        cards.appendChild(card);
      }
      paintRail();
    };

    // paintRail writes the tab's list into the left rail: id · action · kind, the
    // same three facts the card leads with. It repaints from `doc.rules` on every
    // renderCards, so adding, removing, or renaming a rule is reflected immediately
    // — a rail that drifted from the editor beside it would be worse than no rail.
    const paintRail = () => {
      side.replaceChildren();
      const rules = doc.rules || [];
      side.appendChild(railHead('Rules', rules.length));
      if (!rules.length) {
        side.appendChild(railEmpty('No active engine rules. Add and save one on the right; '
          + 'vendor-hook reach is a separate observed fact.'));
        return;
      }
      const sessKeys = S.governSession ? sessionTagKeys() : null;
      for (const rule of rules) {
        const workflow = nonCommandPredicate(rule);
        // Same chip vocabulary the Check result uses, so an action means the same
        // colour everywhere in the console (console-design §3, honesty palette).
        const act = el('span', 'chip ' + (ACTION_CHIP[rule.action] || 'st-stale'),
          rule.action || 'ask');
        const kind = el('span', 'chip ' + (workflow ? 'cl-observed' : 'st-draft'),
          workflow ? 'workflow' : 'command');
        const meta = [act, kind];
        const applies = sessKeys && ruleTouchesSession(rule, sessKeys);
        if (applies) {
          const a = el('span', 'chip cl-observed', 'in scope');
          a.title = 'This rule reads a tag the scoped session has — it could apply here (best-effort).';
          meta.push(a);
        }
        side.appendChild(railRow({
          title: rule.id || '(unnamed rule)',
          dim: !rule.id,
          meta,
          onClick: row => {
            markSelected(row);
            setCrumb(rule.id || '(unnamed rule)');
            const card = cardOf.get(rule);
            if (!card) return;
            // Instant, not smooth: a smooth scroll over a 7000px pane visibly failed
            // to arrive here, and motion is the wrong thing to spend on a jump list
            // (console-design §3 — subtle motion, reduced-motion respected).
            card.scrollIntoView({ block: 'center' });
            // A brief outline, not a persistent selected state: the center is one
            // pane showing every rule, so "selected" here means "the one you just
            // jumped to", and a permanent highlight would claim a filter we do not apply.
            card.style.outline = '2px solid var(--accent)';
            setTimeout(() => { card.style.outline = ''; }, 1200);
          },
        }));
      }
    };

    renderCards();
    rulesWrap.appendChild(cards);

    const actions = el('div', 'row');
    const addRule = el('button', 'btn', '+ New rule');
    addRule.onclick = () => {
      const rule = { id: 'new-rule', intent: '', action: 'ask' };
      setPatterns(rule, ['']); // one empty command-regex term, in the one format
      (doc.rules = doc.rules || []).push(rule); renderCards();
    };

    const save = el('button', 'btn primary', 'Save rules (engine-validated)');
    const saveMsg = el('span', 'sub', '');
    save.onclick = async () => {
      save.disabled = true; saveMsg.textContent = 'validating with cp…';
      // Normalize every command-guard rule to the one format on the way out, so the
      // saved file is never MIXED (some `if`, some legacy `guards`). Rules the editor
      // showed read-only (Phase-3 detector predicates) already carry `if` and are
      // left untouched.
      for (const rule of (doc.rules || [])) {
        if (!nonCommandPredicate(rule) && Array.isArray(rule.guards)) {
          setPatterns(rule, patternsOf(rule));
        }
      }
      try {
        const out = await api('/api/policy/rules', { method: 'PUT',
          headers: { 'If-Match': activeStatus.state_token }, body: JSON.stringify(doc) });
        if (!main.isConnected) return;
        paintRail();
        let refreshNote = '';
        try {
          const refreshed = await api('/api/policy/rules');
          paintProvenance(refreshed); renderSelectionStatus(refreshed);
          selectionReview.replaceChildren();
          document.dispatchEvent(new CustomEvent('cg:info'));
        } catch (e) {
          refreshNote = ' · active provenance refresh failed: ' + e.message;
        }
        saveMsg.textContent = '✓ saved' + (out.backup ? ' (backup: ' + out.backup + ')' : '') + refreshNote;
      } catch (e) {
        saveMsg.replaceChildren(el('span', '', '✖ ' + e.message));
        const reload = el('button', 'btn', 'Reload Policy and review again');
        reload.onclick = () => document.dispatchEvent(new CustomEvent('cg:govern-refresh'));
        saveMsg.append(' ', reload);
      }
      save.disabled = false;
    };
    if (res.selection === 'invocation-path') {
      save.disabled = true;
      saveMsg.textContent = 'invocation override — remove CG_RULES before editing in Policy';
    }
    actions.append(addRule, save, saveMsg);
    rulesWrap.appendChild(actions);
    rulesWrap.appendChild(el('div', 'sub',
      'Honesty: regex guards on shell commands are best-effort (an agent can reach the same effect another way); labels stay amber until canary-probed. Loosen-confirm dialog is v1b — this editor trusts the human at the console.'));
  });

  // --- 2. TEST A COMMAND (cp check — the keystone) ---
  main.appendChild(el('h2', '', 'Test a command'));
  main.appendChild(el('div', 'sub', 'Dry-runs against the SAME binary the hooks run — no agent, no model call. What would happen if an agent ran this?'));
  const testRow = el('div', 'row');
  const cmdIn = el('input'); cmdIn.placeholder = 'e.g. git push origin main'; cmdIn.style.flex = '1'; cmdIn.style.fontFamily = 'var(--mono)';
  const testBtn = el('button', 'btn primary', 'Check');
  testRow.append(cmdIn, testBtn);
  main.appendChild(testRow);
  const testOut = el('div'); main.appendChild(testOut);
  const doCheck = async () => {
    const command = cmdIn.value.trim();
    if (!command) return;
    testOut.innerHTML = '';
    try {
      const r = await api('/api/policy/check', { method: 'POST', body: JSON.stringify({ command }) });
      const card = el('div', 'banner');
      card.style.borderLeftColor = r.decision === 'allow' ? 'var(--ok)' : r.decision === 'ask' ? 'var(--warn)' : 'var(--bad)';
      const row = el('div', 'row');
      row.appendChild(el('span', 'chip ' + (r.decision === 'allow' ? 'st-verified' : r.decision === 'ask' ? 'st-stale' : 'st-disputed'), r.decision.toUpperCase()));
      if (r.rule) row.appendChild(el('span', '', r.rule));
      else row.appendChild(el('span', 'sub', 'no rule matched'));
      card.appendChild(row);
      card.appendChild(el('div', 'sub', 'evaluator: ' + r.evaluator));
      testOut.appendChild(card);
    } catch (e) { testOut.appendChild(el('div', 'sysline err', '✖ ' + e.message)); }
  };
  testBtn.onclick = doCheck;
  cmdIn.onkeydown = e => { if (e.key === 'Enter') doCheck(); };

  // --- 3. RECENT DECISIONS (the audit trail) ---
  main.appendChild(el('h2', '', 'Recent decisions'));
  const decWrap = el('div');
  main.appendChild(decWrap);
  await withState(decWrap, mkLoader('Reading decision log…'), () => api('/api/policy/decisions'), res => {
    decWrap.appendChild(el('div', 'sub', (res.source || 'no log found') + ' · ' + res.decisions.length + ' entries · ' + res.note));
    if (!res.decisions.length) {
      decWrap.appendChild(el('div', 'empty', 'No decisions logged yet — they appear when hooks fire in real agent sessions.'));
      return;
    }
    const t = el('table');
    t.innerHTML = '<tr><th>When</th><th>Decision</th><th>Rule / guard</th><th>Command</th><th>Prompt</th></tr>';
    for (const d of res.decisions) {
      const tr = el('tr');
      tr.appendChild(el('td', '', fmtTime(d.ts)));
      const c = el('td'); c.appendChild(el('span', 'chip ' + (d.decision === 'allow' ? 'st-verified' : 'st-disputed'), d.decision || d.action)); tr.appendChild(c);
      tr.appendChild(el('td', '', (d.rule || '') + (d.guard ? ' / ' + d.guard : '')));
      const cmd = el('td', '', d.command || ''); cmd.style.fontFamily = 'var(--mono)'; cmd.style.fontSize = '11.5px'; tr.appendChild(cmd);
      tr.appendChild(el('td', '', d.prompt || ''));
      t.appendChild(tr);
    }
    decWrap.appendChild(t);
  });

}

export { renderPolicy };
