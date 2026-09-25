// Pure Code state subview for the existing Change map provider. This module
// registers no provider and owns no cache, endpoint, or global state.
import { el, refAnchor } from '../core.js';
import { addPager } from './panel-pager.js';
import { cell, factTable, observedMillis, sessionEvidence } from './session-evidence.js';

const pageValue = (page, key, fallback=0) => page?.[key] ?? fallback;
const measured = value => value === null || value === undefined ? '—' : String(value);
const shortDigest = value => value ? value.slice(0, 18) + (value.length > 18 ? '…' : '') : '—';
const boundaryTime = value => value ? new Date(observedMillis(value)).toLocaleString() : '—';
const checkoutKey = item => `${item.repository_id}\u0000${item.checkout_id}`;

function appendModeState(box, changes) {
  const identity = el('div', 'sub evidence-code-identity',
    `${changes.repository_id || '? repository'} · checkout ${changes.checkout_id || '?'}`);
  identity.title = `${changes.repository_id || ''} · ${changes.checkout_id || ''}`;
  box.appendChild(identity);
  box.appendChild(el('div', 'evidence-state', changes.state || 'unavailable'));
  if (changes.reason) box.appendChild(el('div', 'sub evidence-boundary', changes.reason));

  const factsAvailable = changes.current && ['exact', 'baseline_unavailable'].includes(changes.state);
  const baselineExact = changes.state === 'exact' && !!changes.baseline?.generation_id;
  if (factsAvailable) {
    const facts = el('div', 'evidence-populations evidence-code-counts');
    const add = (value, label, title='') => {
      const fact = el('div', 'evidence-population');
      if (title) fact.title = title;
      fact.append(el('b', '', measured(value)), el('small', '', label));
      facts.appendChild(fact);
    };
    add(changes.current_analyzed_units, 'current analyzed files');
    add(changes.current_checkpoint_path_total, 'Git checkpoint paths');
    if (baselineExact) add(changes.baseline_analyzed_units, 'baseline analyzed files');
    add(pageValue(changes.file_page, 'total'),
      changes.population === 'current_analyzed_units' ? 'current file rows' : 'changed file rows');
    box.appendChild(facts);
  }

  const boundary = el('div', 'evidence-code-boundaries');
  if (changes.current) {
    boundary.appendChild(el('div', 'sub',
      `current checkpoint #${changes.current.checkpoint_id} · generation ${changes.current.generation_id || '—'} · ${boundaryTime(changes.current.captured_at)}`));
  }
  if (baselineExact) {
    boundary.appendChild(el('div', 'sub',
      `baseline checkpoint #${changes.baseline.checkpoint_id} · generation ${changes.baseline.generation_id || '—'} · ${boundaryTime(changes.baseline.captured_at)}`));
  } else if (changes.baseline) {
    boundary.appendChild(el('div', 'sub',
      `observed baseline checkpoint #${changes.baseline.checkpoint_id} · exact baseline analysis unavailable`));
  } else {
    boundary.appendChild(el('div', 'sub', '? exact baseline unavailable'));
  }
  boundary.appendChild(el('div', 'sub', `function calls · ${changes.function_call_coverage || 'unavailable'}`));
  box.appendChild(boundary);
}

function appendAggregateFacts(box, changes) {
  const aggregate = changes.aggregates || {};
  if (!['exact', 'partial'].includes(aggregate.state)) return;
  const values = el('div', 'evidence-value-strip evidence-code-aggregate-values');
  const add = (value, label, available=true) => {
    if (!available) return;
    const fact = el('div', 'evidence-value', String(value));
    fact.appendChild(el('small', '', label));
    values.appendChild(fact);
  };
  add(aggregate.file_total, 'files changed');
  add(aggregate.declaration_changes, 'symbols changed');
  add(aggregate.declarations_added, 'symbols added');
  add(aggregate.declarations_removed, 'symbols removed');
  add(aggregate.declarations_modified, 'symbols modified');
  add(aggregate.source_span_lines?.delta, 'source-span Δ', (aggregate.source_span_lines?.measured || 0) > 0);
  add(aggregate.cyclomatic?.delta, 'cyclomatic Δ', (aggregate.cyclomatic?.measured || 0) > 0);
  const structural = aggregate.structural_matches || {};
  add(pageValue(structural.page, 'total'), 'structural matches', ['exact', 'partial'].includes(structural.state));
  box.appendChild(values);
  box.appendChild(el('div', 'sub evidence-code-family-boundary',
    `${aggregate.file_returned || 0} of ${aggregate.file_total || 0} changed files measured · ` +
    `${aggregate.source_span_lines?.measured || 0} source-span deltas · ${aggregate.cyclomatic?.measured || 0} cyclomatic deltas · ${aggregate.state}`));

  if (['exact', 'partial'].includes(structural.state) && (structural.rows || []).length) {
    const disclosure = document.createElement('details');
    disclosure.className = 'evidence-detail-group evidence-code-structural-population';
    const summary = document.createElement('summary');
    summary.textContent = `Structural match changes · ${pageValue(structural.page, 'total')}`;
    disclosure.appendChild(summary);
    const table = factTable('Measured structural match relationship changes', ['file', 'other file', 'state', 'nodes', 'digest']);
    for (const match of structural.rows) {
      const row = document.createElement('tr');
      const center = document.createElement('th'); center.scope = 'row'; center.appendChild(refAnchor(match.center_path, match.center_path)); row.appendChild(center);
      const other = document.createElement('td'); other.appendChild(refAnchor(match.other_path, match.other_path)); row.appendChild(other);
      cell(row, match.state || '—'); cell(row, measured(match.node_count)); cell(row, shortDigest(match.digest), false, match.digest || '');
      table.tBodies[0].appendChild(row);
    }
    disclosure.appendChild(table);
    if (structural.reason) disclosure.appendChild(el('div', 'sub evidence-boundary', structural.reason));
    box.appendChild(disclosure);
  }
}

function appendCheckoutSelector(box, statuses, state, reload) {
  if (statuses.length < 2) return;
  const label = el('label', 'evidence-scope-filter evidence-code-checkout');
  label.appendChild(el('span', '', 'Repository checkout'));
  const select = document.createElement('select');
  select.setAttribute('aria-label', 'Repository checkout for code state');
  const prompt = document.createElement('option');
  prompt.value = '';
  prompt.textContent = 'Select one checkout';
  prompt.selected = !state.codeCheckout;
  select.appendChild(prompt);
  for (const status of statuses) {
    const option = document.createElement('option');
    option.value = checkoutKey(status);
    option.textContent = `${status.repository_id} · ${status.checkout_id}`;
    option.title = option.textContent;
    option.selected = state.codeCheckout === option.value;
    select.appendChild(option);
  }
  select.addEventListener('change', () => {
    state.codeCheckout = select.value;
    state.codeOffset = 0;
    void reload();
  });
  label.appendChild(select);
  box.appendChild(label);
}

function metricChange(value) {
  return `${measured(value?.before)} → ${measured(value?.after)} · Δ ${measured(value?.delta)}`;
}

function appendCurrentDeclarations(box, facts) {
  const rows = facts?.declarations || [];
  box.appendChild(el('div', 'sub evidence-code-family-boundary',
    `current declarations · ${facts?.returned || 0} of ${facts?.total || 0} shown${facts?.truncated ? ' · truncated' : ' · exact'}`));
  const list = el('div', 'evidence-code-declarations');
  for (const declaration of rows) {
    const item = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = `${declaration.kind || '?'} · ${declaration.name || declaration.identity || '?'}`;
    item.append(summary,
      el('div', 'sub', `identity · ${declaration.identity || '—'}`),
      el('div', 'sub', `lines · ${declaration.line || '—'}–${declaration.end_line || declaration.line || '—'}`),
      el('div', 'sub', `source span · ${measured(declaration.source_span_lines)}`),
      el('div', 'sub', `cyclomatic · ${measured(declaration.cyclomatic)}`));
    list.appendChild(item);
  }
  box.appendChild(list);
}

function appendDeclarationChanges(box, facts) {
  const rows = facts?.changes || [];
  box.appendChild(el('div', 'sub evidence-code-family-boundary',
    `declaration changes · ${facts?.returned || 0} of ${facts?.total || 0} shown${facts?.truncated ? ' · truncated' : ' · exact'}`));
  const list = el('div', 'evidence-code-declarations');
  for (const declaration of rows) {
    const item = document.createElement('details');
    const summary = document.createElement('summary');
    const name = declaration.after_name || declaration.before_name || declaration.identity || '?';
    summary.textContent = `${declaration.state || '?'} · ${declaration.kind || '?'} · ${name}`;
    item.append(summary,
      el('div', 'sub', `identity · ${declaration.identity || '—'}`),
      el('div', 'sub', `lines · ${declaration.before_line || '—'}–${declaration.before_end_line || declaration.before_line || '—'} → ${declaration.after_line || '—'}–${declaration.after_end_line || declaration.after_line || '—'}`),
      el('div', 'sub', `source span · ${metricChange(declaration.source_span_lines)}`),
      el('div', 'sub', `cyclomatic · ${metricChange(declaration.cyclomatic)}`));
    list.appendChild(item);
  }
  box.appendChild(list);
}

function appendStructuralMatches(box, structural, detailState, reload) {
  const rows = structural?.rows || [];
  const page = structural?.page || {};
  const total = pageValue(page, 'total');
  const returned = rows.length;
  box.appendChild(el('div', 'evidence-sectionhead', 'Structural matches'));
  const available = ['exact', 'partial'].includes(structural?.state);
  box.appendChild(el('div', 'sub evidence-code-family-boundary', available
    ? `structural matches · ${returned} of ${total} shown · ${structural.state} · mechanical match ≠ duplicate`
    : `${structural?.state || 'unavailable'} · mechanical match ≠ duplicate`));
  if (structural?.reason) box.appendChild(el('div', 'sub evidence-boundary', structural.reason));
  if (available && rows.length) {
    const table = factTable('Structural match relationship changes', ['other file', 'state', 'nodes', 'digest']);
    for (const match of rows) {
      const row = document.createElement('tr');
      const path = document.createElement('th');
      path.scope = 'row';
      path.appendChild(refAnchor(match.other_path, match.other_path));
      row.appendChild(path);
      cell(row, match.state || '—');
      cell(row, measured(match.node_count));
      cell(row, shortDigest(match.digest), false, match.digest || '');
      table.tBodies[0].appendChild(row);
    }
    box.appendChild(table);
  }
  if (!available) return;
  const paging = {
    offset: pageValue(page, 'offset'),
    limit: pageValue(page, 'limit', 25),
    total,
    returned,
  };
  if (returned > 0 && paging.offset + returned < total) paging.next_offset = paging.offset + returned;
  addPager(box, 'structural matches', paging, detailState, 'structuralOffset', reload);
}

function appendStringFacts(box, label, facts, paths=false) {
  const values = facts?.values || [];
  box.appendChild(el('div', 'sub evidence-code-family-boundary',
    `${label} · ${facts?.returned || 0} of ${facts?.total || 0} shown${facts?.exact ? ' · exact' : ' · partial'}`));
  const list = el('div', 'evidence-code-values');
  for (const value of values) list.appendChild(paths ? refAnchor(value, value) : el('code', '', value));
  if (values.length) box.appendChild(list);
}

function appendDependencies(box, dependencies) {
  box.appendChild(el('div', 'evidence-sectionhead', 'Dependency facts'));
  box.appendChild(el('div', 'sub', `${dependencies?.state || 'unavailable'}${dependencies?.reason ? ` · ${dependencies.reason}` : ''}`));
  if (['exact', 'partial'].includes(dependencies?.state)) {
    appendStringFacts(box, 'Dependent packages', dependencies?.dependent_packages);
    appendStringFacts(box, 'Referencing files', dependencies?.referencing_files, true);
  }
  box.appendChild(el('div', 'sub evidence-code-family-boundary',
    `function calls · ${dependencies?.function_call_coverage || 'unavailable'}`));
  const callers = dependencies?.callers || {};
  if (['exact', 'partial'].includes(callers.state)) {
    box.appendChild(el('div', 'sub evidence-code-family-boundary',
      `${callers.returned || 0} of ${callers.total || 0} incoming call edges shown · ` +
      `${callers.declaration_returned || 0} of ${callers.declaration_total || 0} changed declarations · ${callers.state}`));
    if ((callers.edges || []).length) {
      const table = factTable('Incoming calls to changed declarations', ['caller', 'changed declaration', 'source', 'producer']);
      for (const edge of callers.edges) {
        const row = document.createElement('tr');
        cell(row, edge.from_ref, true); cell(row, edge.to_ref);
        cell(row, edge.source_path ? `${edge.source_path}${edge.source_line ? ':' + edge.source_line : ''}` : '—');
        cell(row, edge.analyzer_id || '—'); table.tBodies[0].appendChild(row);
      }
      box.appendChild(table);
    }
  } else if (callers.reason) box.appendChild(el('div', 'sub evidence-boundary', callers.reason));
}

function appendAttribution(box, coverage) {
  const exact = coverage?.exact === true;
  box.appendChild(el('div', 'evidence-sectionhead', 'Action attribution for this file'));
  box.appendChild(el('div', 'sub evidence-code-family-boundary',
    `${coverage?.returned || 0} of ${coverage?.total || 0} path reconciliations shown · ${exact ? 'exact' : 'partial'}`));
  const facts = el('div', 'evidence-populations evidence-code-attribution');
  for (const [value, label] of [
    [coverage?.exact_matched, 'exact matched'],
    [coverage?.ambiguous, 'ambiguous'],
    [coverage?.unattributed, 'unattributed'],
    [coverage?.other, 'other'],
  ]) {
    const item = el('div', 'evidence-population');
    item.append(el('b', '', measured(value)), el('small', '', label));
    facts.appendChild(item);
  }
  box.appendChild(facts);
}

function renderFileDetail(box, detail, detailState, reload) {
  box.replaceChildren();
  const file = detail.file || {};
  const identity = [file.path || '?', file.state || detail.state || 'unavailable', file.language, file.namespace]
    .filter(Boolean).join(' · ');
  box.appendChild(el('div', 'evidence-state', identity));
  if (detail.reason) box.appendChild(el('div', 'sub evidence-boundary', detail.reason));
  if (file.state === 'current_only') appendCurrentDeclarations(box, file.current_declarations);
  else appendDeclarationChanges(box, file.declaration_changes);
  appendStructuralMatches(box, detail.structural_matches, detailState, reload);
  appendDependencies(box, detail.dependencies);
  appendAttribution(box, detail.attribution_coverage);
}

function attachCodeFileDetail(ctx, host, detailRow, detail, changes, file) {
  const button = el('button', 'evidence-code-inspect', 'Inspect');
  button.type = 'button';
  button.setAttribute('aria-label', `Inspect collected code facts for ${file.path}`);
  const detailState = {request: 0, structuralOffset: 0, loaded: false};
  const load = async () => {
    const request = ++detailState.request;
    if (!detailState.loaded) detail.replaceChildren(el('div', 'sub', 'Loading code facts…'));
    try {
      const response = await sessionEvidence(ctx, 'code-change-file', {
        repository_id: changes.repository_id,
        checkout_id: changes.checkout_id,
        path: file.path,
        structural_offset: detailState.structuralOffset,
        structural_limit: 25,
      });
      if (!detail.isConnected || request !== detailState.request) return;
      detailState.loaded = true;
      renderFileDetail(detail, response.code_change_file || {}, detailState, load);
    } catch (error) {
      if (!detail.isConnected || request !== detailState.request) return;
      if (error?.status === 404) {
        detail.replaceChildren(el('div', 'evidence-state', 'Code analysis not captured for this file'),
          el('div', 'sub', file.path));
        return;
      }
      detail.replaceChildren(el('div', 'evidence-state', 'Request failed'),
        el('div', 'sub', 'Code facts for this file could not be loaded.'));
      const retry = el('button', '', 'Retry file facts');
      retry.addEventListener('click', () => { void load(); });
      detail.appendChild(retry);
    }
  };
  button.addEventListener('click', event => {
    event.preventDefault();
    event.stopPropagation();
    detailRow.hidden = !detailRow.hidden;
    button.setAttribute('aria-expanded', String(!detailRow.hidden));
    if (!detailRow.hidden && !detailState.loaded) void load();
  });
  button.setAttribute('aria-expanded', 'false');
  host.appendChild(button);
}

function renderFiles(box, ctx, changes, state, reload) {
  const page = changes.file_page || {};
  const currentOnly = changes.population === 'current_analyzed_units';
  const label = currentOnly ? 'Current analyzed files' : 'Changed analyzed files';
  box.appendChild(el('div', 'evidence-sectionhead', label));
  box.appendChild(el('div', 'sub evidence-match-count',
    `${pageValue(page, 'returned')} of ${pageValue(page, 'total')} shown${page.exact === false ? ' · paged' : ' · full population'}`));
  const table = factTable(label, ['file', 'state', 'declarations', 'inspect']);
  table.classList.add('evidence-code-files');
  for (const file of changes.files || []) {
    const row = document.createElement('tr');
    const path = document.createElement('th');
    path.scope = 'row';
    path.appendChild(refAnchor(file.path, file.path));
    row.appendChild(path);
    cell(row, file.state || '—');
    const declarations = currentOnly ? file.current_declarations : file.declaration_changes;
    cell(row, `${declarations?.returned || 0} / ${declarations?.total || 0}${declarations?.truncated ? '+?' : ''}`);
    const inspect = document.createElement('td');
    row.appendChild(inspect);
    table.tBodies[0].appendChild(row);
    const detailRow = document.createElement('tr');
    detailRow.hidden = true;
    const detailCell = document.createElement('td');
    detailCell.colSpan = 4;
    const detail = el('div', 'evidence-code-file-detail');
    detailCell.appendChild(detail);
    detailRow.appendChild(detailCell);
    attachCodeFileDetail(ctx, inspect, detailRow, detail, changes, file);
    table.tBodies[0].appendChild(detailRow);
  }
  box.appendChild(table);
  addPager(box, 'code files', page, state, 'codeOffset', reload);
}

function appendTargetFileDetail(box, ctx, changes, state) {
  const target = state.codeTarget;
  if (!target?.path || target.repository_id !== changes.repository_id || target.checkout_id !== changes.checkout_id) return;
  const key = `${target.repository_id}\u0000${target.checkout_id}\u0000${target.path}`;
  if (!state.codeTargetDetail || state.codeTargetDetail.key !== key) {
    state.codeTargetDetail = {key, request:0, structuralOffset:0};
  }
  const detailState = state.codeTargetDetail;
  const disclosure = document.createElement('details');
  disclosure.className = 'evidence-detail-group evidence-code-target';
  disclosure.open = true;
  const summary = document.createElement('summary');
  summary.textContent = `Code facts · ${target.path}`;
  const detail = el('div', 'evidence-code-file-detail');
  disclosure.append(summary, detail);
  box.appendChild(disclosure);
  const load = async () => {
    const request = ++detailState.request;
    detail.replaceChildren(el('div', 'sub', 'Loading code facts…'));
    try {
      const response = await sessionEvidence(ctx, 'code-change-file', {
        repository_id: target.repository_id,
        checkout_id: target.checkout_id,
        path: target.path,
        structural_offset: detailState.structuralOffset,
        structural_limit: 25,
      });
      if (!detail.isConnected || request !== detailState.request) return;
      renderFileDetail(detail, response.code_change_file || {}, detailState, load);
    } catch (error) {
      if (!detail.isConnected || request !== detailState.request) return;
      if (error?.status === 404) {
        detail.replaceChildren(el('div', 'evidence-state', 'Code analysis not captured for this file'),
          el('div', 'sub', target.path));
        return;
      }
      detail.replaceChildren(el('div', 'evidence-state', 'Request failed'),
        el('div', 'sub', 'Code facts for this file could not be loaded.'));
      const retry = el('button', '', 'Retry file facts');
      retry.addEventListener('click', () => { void load(); });
      detail.appendChild(retry);
    }
  };
  void load();
}

export async function renderCodeState(ctx, box, state, reload, request) {
  try {
    const summaryResponse = await sessionEvidence(ctx, 'summary');
    if (!box.isConnected || request !== state.request) return;
    const statuses = summaryResponse.summary?.code_changes || [];
    if (!statuses.length) {
      box.replaceChildren(el('div', 'evidence-state', 'Code analysis not captured'),
        el('div', 'sub', 'No repository checkout with stored code-state analysis is linked to this session.'));
      return;
    }
    if (state.codeTarget && statuses.some(status => status.repository_id === state.codeTarget.repository_id && status.checkout_id === state.codeTarget.checkout_id)) {
      state.codeCheckout = `${state.codeTarget.repository_id}\u0000${state.codeTarget.checkout_id}`;
    } else if (statuses.length === 1) state.codeCheckout = checkoutKey(statuses[0]);
    if (state.codeCheckout && !statuses.some(status => checkoutKey(status) === state.codeCheckout)) {
      state.codeCheckout = '';
    }
    box.replaceChildren();
    appendCheckoutSelector(box, statuses, state, reload);
    if (!state.codeCheckout) {
      box.appendChild(el('div', 'evidence-state', 'Select one repository checkout'));
      box.appendChild(el('div', 'sub', 'Code-state comparison requires an exact repository and checkout identity.'));
      return;
    }
    const selected = statuses.find(status => checkoutKey(status) === state.codeCheckout);
    const response = await sessionEvidence(ctx, 'code-changes', {
      repository_id: selected.repository_id,
      checkout_id: selected.checkout_id,
      code_offset: state.codeOffset,
      code_limit: 25,
    });
    if (!box.isConnected || request !== state.request) return;
    const changes = response.code_changes || {};
    box.replaceChildren();
    appendCheckoutSelector(box, statuses, state, reload);
    appendModeState(box, changes);
    if (!changes.current || !['exact', 'baseline_unavailable'].includes(changes.state)) return;
    appendAggregateFacts(box, changes);
    appendTargetFileDetail(box, ctx, changes, state);
    renderFiles(box, ctx, changes, state, reload);
  } catch {
    if (!box.isConnected || request !== state.request) return;
    box.replaceChildren(el('div', 'evidence-state', 'Request failed'),
      el('div', 'sub', 'Code-state evidence could not be loaded.'));
    const retry = el('button', '', 'Retry code state');
    retry.addEventListener('click', () => { void reload(); });
    box.appendChild(retry);
  }
}
