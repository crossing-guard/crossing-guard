// C6 session Change Map. Facts only: independent P/T/Δ/C membership and sources.
import { el, api, debounce, refAnchor } from '../core.js';
import { provider } from '../infopanel.js';
import { addPager } from './panel-pager.js';
import { cell, factTable, fileEvidencePopulations, observedMillis, qualifiedKnown, sessionEvidence, sessionQuery } from './session-evidence.js';
import { renderCodeState } from './session-code-state.js';
import { renderSessionActivity } from './session-trace.js';
import { retainedBodyButton } from './retained-body.js';

const mark = (on, text, cls, available=true) => {
  const s = el('span', `evidence-cell-mark ${on ? cls : available ? 'mark-empty' : 'mark-unknown'}`, on ? '●' : (available ? '·' : '?'));
  s.title = on ? ({P:'Claimed in declaration',T:'Observed governed touch','Δ':'Observed checkout snapshot change',C:'Claimed implementation'}[text] || text) : (available ? 'Not in this evidence set' : `${text} evidence unavailable`);
  s.setAttribute('aria-label', s.title);
  return s;
};
const pageValue = (p, key, fallback=0) => p?.[key] ?? fallback;
const shortDigest = value => value ? value.slice(0, 22) + (value.length > 22 ? '…' : '') : '?';
const when = observed => observed ? new Date(observedMillis(observed)).toLocaleString() : '?';
const age = observed => {
  if (!observed) return '?';
  const seconds = Math.max(0, Math.floor((Date.now() - observedMillis(observed)) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
};
const superseded = count => count ? ` · ${count} superseded` : '';

const statusMeaning = raw => {
  const source = String(raw || '');
  const split = source.indexOf(':');
  const layer = split < 0 ? 'source' : source.slice(0, split);
  const code = split < 0 ? source : source.slice(split + 1);
  if (layer === 'untracked') return 'untracked';
  const meanings = {A:'added',M:'modified',D:'deleted',R:'renamed',C:'copied',T:'type changed',U:'conflict'};
  const meaning = meanings[String(code || '').charAt(0)] || `unknown (${code || 'empty'})`;
  return `${({committed:'base diff',index:'staged',worktree:'working tree'}[layer] || layer)}: ${meaning}`;
};

function appendFolder(body, state, repositoryKey, dir, rows, appendRow, columnCount=6, scopeMarker=null) {
  const key = `${repositoryKey}\u0000${dir}`;
  const accessibleDir = scopeMarker ? `${scopeMarker.label} · ${dir}` : dir;
  const collapsed = state.collapsedFolders.has(key);
  const folder = document.createElement('tr');
  folder.className = 'evidence-folder';
  const heading = document.createElement('th');
  heading.scope = 'rowgroup';
  heading.colSpan = columnCount;
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'evidence-folder-toggle';
  button.setAttribute('aria-expanded', String(!collapsed));
  button.setAttribute('aria-label', `${collapsed ? 'Expand' : 'Collapse'} ${accessibleDir}, ${rows.length} file${rows.length === 1 ? '' : 's'}`);
  button.appendChild(el('span', 'evidence-folder-caret', collapsed ? '▸' : '▾'));
  if (scopeMarker) {
    const marker = el('span', 'evidence-folder-scope', scopeMarker.text);
    marker.title = scopeMarker.label;
    marker.setAttribute('aria-hidden', 'true');
    button.appendChild(marker);
  }
  button.append(el('span', '', dir), el('span', 'evidence-folder-count', String(rows.length)));
  heading.appendChild(button);
  folder.appendChild(heading);
  body.appendChild(folder);
  const rendered = rows.map(row => appendRow(row));
  for (const row of rendered) { row.hidden = collapsed; body.appendChild(row); }
  button.addEventListener('click', () => {
    const next = button.getAttribute('aria-expanded') === 'true';
    button.setAttribute('aria-expanded', String(!next));
    button.setAttribute('aria-label', `${next ? 'Expand' : 'Collapse'} ${accessibleDir}, ${rows.length} file${rows.length === 1 ? '' : 's'}`);
    button.querySelector('.evidence-folder-caret').textContent = next ? '▸' : '▾';
    for (const row of rendered) row.hidden = next;
    if (next) state.collapsedFolders.add(key); else state.collapsedFolders.delete(key);
  });
}

const observedScopeLabels = {
  'selected-checkout':'Selected captured checkout',
  'other-checkout':'Other captured checkout',
  'ambiguous-checkout':'Multiple captured checkouts',
  'working-directory':'Recorded working directory',
  temporary:'Host temporary roots',
  'outside-identified-roots':'Outside identified roots',
  unresolved:'Unresolved path',
};

// Compact context for the one Change Map owner. Counts and non-file facts only:
// the tree below remains the sole path list for this session.
function appendSessionOverview(box, selection) {
  const facts = selection.facts || {};
  const section = document.createElement('details');section.className='evidence-overview evidence-detail-group';
  const summary=document.createElement('summary');summary.textContent='Session context';section.appendChild(summary);
  const rows = el('div', 'evidence-overview-rows');
  const addRow = (label, value) => {
    if (value == null || value === '') return;
    const row = el('div', 'evidence-overview-row');
    const exact = el('code', '', String(value));
    exact.title = String(value);
    row.append(el('span', 'sub', label), exact);
    rows.appendChild(row);
  };
  addRow('working directory', selection.cwd || selection.project);
  addRow('branch', facts.branch || selection.branch);
  section.appendChild(rows);

  const counts = el('div', 'evidence-populations evidence-overview-counts');
  const addCount = (label, value, detail='') => {
    if (!Number.isFinite(value) || value <= 0) return;
    const fact = el('div', 'evidence-population');
    if (detail) fact.title = detail;
    fact.setAttribute('aria-label', `${label}: ${value}${detail ? `. ${detail}` : ''}`);
    fact.append(el('b', '', String(value)), el('small', '', label));
    counts.appendChild(fact);
  };
  addCount('commits', (facts.commit_list || []).length || selection.commits,
    (facts.commit_list || []).map(commit => `${commit.sha || ''} ${commit.msg || ''}`.trim()).join(' · '));
  addCount('pushes', (facts.pushes || []).length, (facts.pushes || []).join(' · '));
  addCount('PR references', (facts.prs || []).length, (facts.prs || []).join(' · '));
  addCount('created path mentions', (facts.files_created || []).length);
  addCount('recorded file paths', (facts.files || []).length);
  const readIDs = facts.mem_read_ids || [];
  const writeIDs = facts.mem_write_ids || [];
  const reads = readIDs.length;
  const writes = writeIDs.length || selection.mem_writes;
  if (reads || writes) {
    const fact = el('div', 'evidence-population');
    const memoryDetail = [
      readIDs.length ? `reads: ${readIDs.join(', ')}` : '',
      writeIDs.length ? `writes: ${writeIDs.join(', ')}` : '',
    ].filter(Boolean).join(' · ');
    if (memoryDetail) fact.title = memoryDetail;
    fact.setAttribute('aria-label', `memory reads: ${reads}; writes: ${writes || 0}${memoryDetail ? `. ${memoryDetail}` : ''}`);
    fact.append(el('b', '', `${reads} / ${writes || 0}`), el('small', '', 'memory reads / writes'));
    counts.appendChild(fact);
  }
  addCount('tool calls', facts.tool_calls);
  if (counts.childNodes.length) section.appendChild(counts);
  box.appendChild(section);
}

function appendChangeMode(box, state, load) {
  const controls = el('div', 'evidence-mode-switch');
  controls.setAttribute('aria-label', 'Change map view');
  for (const [value, label] of [['code', 'Code changes'], ['session', 'Collected file activity']]) {
    const button = el('button', '', label);
    button.type = 'button';
    button.setAttribute('aria-pressed', String(state.mode === value));
    button.addEventListener('click', () => {
      if (state.mode === value) return;
      state.mode = value;
      void load();
    });
    controls.appendChild(button);
  }
  box.appendChild(controls);
}

function beginChangeView(box, selection, state, load) {
  box.replaceChildren();
  appendChangeMode(box, state, load);
}

function appendGapDetails(box, gaps) {
  const details = document.createElement('details'); details.className = 'evidence-detail-group';
  const summary = document.createElement('summary'); summary.textContent = `Evidence gaps · ${gaps.length}`; details.appendChild(summary);
  for (const gap of gaps) {
    details.appendChild(el('div','sub',`? ${gap.reason}${gap.count ? ' · '+gap.count : ''}`));
  }
  box.appendChild(details);
}

function appendObservedTargets(box, c, state, load) {
  const counts = c.observed_scope_counts || {};
  const exact = c.observed_scope_counts_exact;
  const facts = el('div','evidence-populations evidence-target-scopes');
  for (const scope of Object.keys(observedScopeLabels)) {
    const count = counts[scope] || 0;
    if (!count) continue;
    const fact=el('div','evidence-population'); fact.append(el('b','',`${count}${exact ? '' : '+?'}`),el('small','',observedScopeLabels[scope])); facts.appendChild(fact);
  }
  if (facts.childNodes.length) box.appendChild(facts);
  const controls = el('div','evidence-change-controls evidence-filters');
  const label = el('label','evidence-scope-filter'); label.appendChild(el('span','','Target relation'));
  const select = document.createElement('select'); select.setAttribute('aria-label','Observed target relation');
  const total = pageValue(c.observed_page,'total');
  for (const [value,text,count] of [['all','All observed targets',c.touch_count||0],...Object.entries(observedScopeLabels).map(([value,text])=>[value,text,counts[value]||0])]) {
    const option=document.createElement('option'); option.value=value; option.textContent=`${text} · ${count}${exact ? '' : '+?'}`; option.disabled=value!=='all'&&!count; option.selected=state.observedScope===value; select.appendChild(option);
  }
  select.addEventListener('change',()=>{state.observedScope=select.value;state.observedOffset=0;load();});
  label.appendChild(select); controls.appendChild(label); box.appendChild(controls);
  box.appendChild(el('div','sub evidence-match-count',`${total} matches · ${pageValue(c.observed_page,'returned')} shown${exact ? '' : ' · population incomplete'}`));
  const touches=c.observed_touches||[];
  if (touches.length) {
    const table=el('table','evidence-table evidence-tree evidence-observed-tree');
    const cap=document.createElement('caption');cap.textContent='Observed file targets';
    const thead=document.createElement('thead');const hr=document.createElement('tr');
    for(const name of ['path','events']){const th=document.createElement('th');th.scope='col';th.textContent=name;if(name==='events'){th.title='Captured events referencing this path';th.setAttribute('aria-label','Events: captured events referencing this path');}hr.appendChild(th);}thead.appendChild(hr);table.append(cap,thead);
    const body=document.createElement('tbody');const groups=new Map();
    for(const touch of touches){
      const display=touch.display_path||touch.path||touch.identity.replace(/^file:/,'');
      const slash=display.lastIndexOf('/');
      const scope=touch.scope||'unresolved';
      const dir=slash>=0?display.slice(0,slash):'.';
      const key=`${scope}\u0000${dir}`;
      if(!groups.has(key))groups.set(key,{scope,dir,rows:[]});
      groups.get(key).rows.push({...touch,row_path:slash>=0?display.slice(slash+1):display});
    }
    for(const group of groups.values()){
      const scopeLabel=observedScopeLabels[group.scope]||group.scope;
      const scopeMarker=group.scope==='working-directory'?{text:'⌂',label:scopeLabel}:null;
      const dir=scopeMarker?group.dir:`${scopeLabel} · ${group.dir}`;
      appendFolder(body,state,`observed:${group.scope}`,dir,group.rows,touch=>{const tr=document.createElement('tr');const path=document.createElement('th');path.scope='row';path.appendChild(refAnchor(touch.path||touch.identity.replace(/^file:/,''),touch.row_path));const checkoutCount=(touch.checkout_matches||[]).length;const working=touch.working_directory?.state||'unavailable';const temporary=touch.temporary_root?.state||'unavailable';const eventCount=touch.events||0;const eventNoun=eventCount===1?'event':'events';const detail=`${eventCount} captured ${eventNoun} referencing this path. Scope: ${observedScopeLabels[touch.scope]||touch.scope}. Working directory: ${working}. Temporary root: ${temporary}. Checkout matches: ${checkoutCount}. Source: ${touch.target_source||'unavailable'}. Lineage: ${touch.lineage||'unavailable'}.`;path.title=[touch.path||touch.identity,detail,touch.working_directory?.root,touch.temporary_root?.root,touch.resolution_reason].filter(Boolean).join(' · ');path.setAttribute('aria-label',`${touch.row_path}. ${detail}`);tr.appendChild(path);const events=el('td','evidence-event-count',String(eventCount));events.title='Captured events referencing this path';events.setAttribute('aria-label',`${eventCount} captured ${eventNoun} referencing this path`);tr.appendChild(events);return tr;},2,scopeMarker);
    }
    table.appendChild(body);box.appendChild(table);
    const captureCounts=new Map();
    for(const touch of touches){const source=touch.target_source||'unavailable';const lineage=touch.lineage||'unavailable';const key=`${source}\u0000${lineage}`;captureCounts.set(key,(captureCounts.get(key)||0)+1);}
    const details=document.createElement('details');details.className='evidence-detail-group evidence-capture-details';
    const summary=document.createElement('summary');summary.textContent=`Capture details · ${touches.length} returned of ${total} matching targets`;details.appendChild(summary);
    for(const [key,count] of captureCounts){const [source,lineage]=key.split('\u0000');details.appendChild(el('div','sub',`${count} returned target${count===1?'':'s'} · source ${source} · lineage ${lineage}`));}
    details.appendChild(el('div','sub','Row details include captured root and checkout relationships.'));
    box.appendChild(details);
  }
  addPager(box,'observed targets',c.observed_page,state,'observedOffset',load);
}

function appendFileControls(box, repo, state, load) {
  const counts = repo.counts || {};
  const touchedUnknown = !repo.completeness?.touched;
  const filters = [
    ['session', 'Session evidence', `${counts.session_linked_known || 0}${touchedUnknown ? '+?' : ''}`, true],
    ['outside-plan', 'Outside plan', repo.completeness?.divergence ? counts.added_scope || 0 : '?', repo.completeness?.divergence],
    ['planned', 'Planned', repo.completeness?.planned ? counts.planned || 0 : '?', repo.completeness?.planned],
    ['touched', 'Touched', `${counts.touched_total || 0}${touchedUnknown ? '+?' : ''}`, counts.touched_total > 0],
    ['claimed', 'Claimed', repo.completeness?.claimed ? counts.claimed || 0 : '?', repo.completeness?.claimed],
    ['changed', 'Changed', repo.completeness?.changed ? counts.changed || 0 : '?', repo.completeness?.changed],
    ['all', 'All known', counts.file_union || repo.file_page?.total || 0, true],
  ];
  const controls = el('div', 'evidence-change-controls evidence-filters');
  const scope = el('label', 'evidence-scope-filter');
  scope.appendChild(el('span', '', 'Set'));
  const scopeSelect = document.createElement('select');
  scopeSelect.setAttribute('aria-label', 'Evidence set');
  for (const [value, label, count, available] of filters) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = `${label} · ${count}`;
    option.disabled = !available;
    option.selected = state.fileFilter === value;
    scopeSelect.appendChild(option);
  }
  scopeSelect.addEventListener('change', () => {
    state.fileFilter = scopeSelect.value;
    state.changeOffset = 0;
    load();
  });
  scope.appendChild(scopeSelect);
  const search = document.createElement('input');
  search.type = 'search';
  search.maxLength = 200;
  search.placeholder = 'Filter repository paths';
  search.setAttribute('aria-label', 'Filter repository paths');
  search.value = state.fileQuery;
  const applySearch = debounce(() => {
    state.fileQuery = search.value;
    state.changeOffset = 0;
    load();
  }, 180);
  search.addEventListener('input', applySearch);
  const sort = document.createElement('select');
  sort.setAttribute('aria-label', 'Sort matching paths');
  for (const [value, label] of [['overlap', 'Most evidence overlap'], ['path', 'Path']]) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = label;
    option.selected = state.fileSort === value;
    sort.appendChild(option);
  }
  sort.addEventListener('change', () => {
    state.fileSort = sort.value;
    state.changeOffset = 0;
    load();
  });
  controls.append(scope, search, sort);
  box.appendChild(controls);
}

// The file-evidence disclosure is four independent renderings of one payload:
// how much is shown, the linked actions, the code effects, and the path
// reconciliations. Each is its own function so the shape of one does not hide
// inside another's nesting, and so a failure in the action lane cannot take the
// effects table down with it.

const fileEvidenceCoverage = data =>
  `${(data.events || []).length} of ${data.event_count || 0} linked actions shown`
  + ` · ${(data.effects || []).length} of ${data.effect_count || 0} linked effects shown`
  + ` · ${(data.path_reconciliation || []).length} of ${data.path_reconciliation_count || 0} path reconciliations shown`;

// One action's call and result sizes, fetched only when the reader asks.
function appendActionDetails(body, found) {
  body.appendChild(el('div', 'sub',
    `input ${found.input?.captured_bytes ?? '?'} B of ${found.input?.raw_bytes ?? '?'} B`
    + ` · ${(found.results || []).length} of ${found.result_count || 0} exact result(s) shown`));
  for (const item of found.results || []) {
    const observed = item.observation || {};
    body.appendChild(el('div', 'sub',
      `result #${observed.id} · raw ${observed.raw_bytes || 0} B · retained ${observed.retained_bytes || 0} B`
      + ` · ${(observed.effects || []).length} of ${item.effect_count || 0} effect(s) shown`));
  }
}

function actionGroup(ctx, action) {
  const group = document.createElement('details');
  const summary = document.createElement('summary');
  summary.textContent = `action #${action.id} · ${action.tool || '?'} · ${action.verb || '?'} · ${when(action.ts)}`;
  group.appendChild(summary);
  const body = el('div', 'sub', `${action.runtime || '?'} · ${action.origin || '?'} · decision ${action.decision || '?'}`);
  group.appendChild(body);
  const inspect = el('button', '', 'Call / result details');
  inspect.addEventListener('click', async () => {
    inspect.disabled = true;
    try {
      const result = await sessionEvidence(ctx, 'action', { event_id: action.id });
      appendActionDetails(body, result.action || {});
    } catch {
      body.appendChild(el('div', 'sub', 'Action details could not be loaded.'));
    }
  });
  group.appendChild(inspect);
  return group;
}

// A digest cell that offers the retained bytes when the body was measured.
function digestCell(digest, measured, button) {
  const td = document.createElement('td');
  td.textContent = digest ? shortDigest(digest) : '?';
  if (measured) td.append(document.createElement('br'), button());
  return td;
}

function effectsTable(ctx, effects, identity) {
  const table = factTable('Linked code effects', ['action', 'operation', 'path', 'content', 'diff', 'completeness']);
  for (const effect of effects) {
    const row = document.createElement('tr');
    cell(row, `#${effect.event_id}`);
    cell(row, effect.operation || '?');
    cell(row, effect.raw_identity || '?', true);
    row.appendChild(digestCell(effect.content_digest, effect.content_measured, () =>
      retainedBodyButton(ctx, 'Show content',
        { body_kind: 'effect_content', result_id: effect.result_id, ordinal: effect.ordinal })));
    row.appendChild(digestCell(effect.diff_digest, effect.diff_measured, () =>
      retainedBodyButton(ctx, 'Show diff',
        { body_kind: 'effect_diff', result_id: effect.result_id, ordinal: effect.ordinal },
        { path: identity.path, absolutePath: identity.absolute_path, operation: effect.operation })));
    cell(row, effect.completeness || '?');
    table.tBodies[0].appendChild(row);
  }
  return table;
}

function renderFileEvidence(ctx, detail, data) {
  const identity = data.file || {};
  detail.replaceChildren();
  detail.appendChild(el('div', 'sub', fileEvidenceCoverage(data)));
  for (const action of data.events || []) detail.appendChild(actionGroup(ctx, action));
  if ((data.effects || []).length) detail.appendChild(effectsTable(ctx, data.effects, identity));
  for (const fact of data.path_reconciliation || []) {
    detail.appendChild(el('div', 'sub',
      `◆ ${fact.classification} · ${fact.path} · action #${fact.event_id || '?'} · result #${fact.result_id || '?'}`));
  }
}

function attachFileDetail(ctx, host, file) {
  if (!file.evidence_key) return;
  const button = el('button', 'evidence-file-details', 'Details');
  button.type = 'button';
  button.setAttribute('aria-label', `Show captured activity for ${file.path}`);
  const detail = el('div', 'evidence-file-detail');
  detail.hidden = true;
  let loaded = false;
  button.addEventListener('click', async event => {
    event.preventDefault();
    event.stopPropagation();
    detail.hidden = !detail.hidden;
    if (detail.hidden || loaded) return;
    loaded = true;
    detail.appendChild(el('div', 'sub', 'Loading linked actions and code-change evidence…'));
    try {
      const response = await sessionEvidence(ctx, 'file', { file_key: file.evidence_key });
      if (!detail.isConnected) return;
      renderFileEvidence(ctx, detail, response.file || {});
    } catch {
      detail.replaceChildren(
        el('div', 'evidence-state', 'Request failed'),
        el('div', 'sub', 'File evidence could not be loaded.'));
    }
  });
  host.append(button, detail);
}

function renderChange(box, c, id, state, load, selection, ctx) {
  beginChangeView(box, selection, state, load);
  if (!c.available) {
	box.appendChild(el('div', 'evidence-sectionhead', 'Observed file targets'));
	appendObservedTargets(box,c,state,load);
	if (c.partial) box.appendChild(el('div', 'sub', '? target list is partial'));
	appendGapDetails(box,c.gaps||[]);
    appendActivity(box, ctx, c.governed_events || 0);
    appendSessionOverview(box, selection);
    return;
  }
  const repos = c.repositories || [];
  if ((c.repository_count || repos.length) > 1) box.appendChild(el('div', 'sub', `${c.repository_count || repos.length} repository groups`));
  if (c.partial) box.appendChild(el('div', 'sub', `? partial envelope — ${c.reason || 'one or more evidence populations are bounded'}`));
  for (const repo of repos) {
    const counts = repo.counts || {};
	const population = fileEvidencePopulations({repository_count:1, repositories:[repo]});
	const selected = repo.selected || {};
	const sectionHead = el('div', 'evidence-sectionhead evidence-change-sectionhead');
	const sectionTitle = el('span', '', 'Declared × observed sets');
	sectionTitle.title = `${repo.checkout_display || repo.repository_id} · ${repo.repository_id} · checkout ${repo.checkout_id} · ${repo.portability}`;
	const marks = el('span', 'evidence-marks');
	for (const [text, cls, title] of [['P','mark-p','Plan membership'],['T','mark-t','Governed touch'],['Δ','mark-d','Revision change'],['C','mark-c','Implementation claim']]) {
	  const badge = el('span', `evidence-mark ${cls}`, text); badge.title = title; marks.appendChild(badge);
	}
	sectionHead.append(sectionTitle, marks); box.appendChild(sectionHead);
	const populations = el('div','evidence-populations');
	const populationFacts = [
	  ['T',qualifiedKnown(population.knownTouched,population.touchComplete),'explicit session paths'],
	  ['Δ',population.changedComplete ? String(population.checkoutChanged) : '?','checkout snapshot paths'],
	  ['T ∩ Δ',population.changedComplete ? String(population.overlap) : '?','explicit touch + snapshot change'],
	  ['Δ − T',population.checkoutWithoutExplicitTouch ?? '?','snapshot change · no explicit touch'],
	  ['T − Δ',population.explicitTouchAbsentSnapshot ?? '?','explicit touch · absent snapshot'],
	];
	for (const [mark,value,label] of populationFacts) {
	  const fact=el('div','evidence-population'); fact.append(el('b','',`${mark} ${value}`),el('small','',label)); populations.appendChild(fact);
	}
	box.appendChild(populations);
	const boundaryParts = [];
	if (!population.touchComplete) boundaryParts.push(population.nonFileActions == null
	  ? '? non-file-action count differs across repository evidence'
	  : `${population.nonFileActions} actions without file targets · additional file effects unknown`);
	else boundaryParts.push('T complete for captured file-target actions');
	boundaryParts.push(selected.revision_id
	  ? `Δ checkout snapshot #${selected.revision_id} · ${when(selected.revision_captured_at)}`
	  : '? no checkout snapshot');
	const populationBoundary=el('div','evidence-population-boundary',boundaryParts.join(' · '));
	populationBoundary.title=selected.revision_source_ref ? `${selected.revision_source_ref} · ${selected.revision_source_digest}` : 'Checkout snapshot source unavailable';
	box.appendChild(populationBoundary);
	const divergence=repo.divergence || {};
	if (divergence.available) {
	  const both=Math.max(0,(counts.planned||0)-(divergence.omitted?.total||0));
	  const setbar=el('div','evidence-setbar');
	  for(const [label,value,cls] of [['P ∩ Δ',both,'both'],['Δ − P',divergence.added?.total||0,'added'],['P − Δ',divergence.omitted?.total||0,'omitted']]){const part=el('span',cls,`${label} · ${value}`);part.style.setProperty('--set-weight',String(Math.max(1,value)));setbar.appendChild(part);} box.appendChild(setbar);
	} else box.appendChild(el('div','evidence-setbar unavailable','? set relationship unavailable · '+(divergence.reason||'complete plan and checkout snapshot required')));
    const sourceDetails = document.createElement('details'); sourceDetails.className = 'evidence-detail-group evidence-source-details';
    const sourceSummary = document.createElement('summary'); sourceSummary.textContent = `Evidence sources · declaration ${selected.declaration_id ? '#'+selected.declaration_id : '?'} · revision ${selected.revision_id ? '#'+selected.revision_id : '?'} · claim ${selected.implementation_id ? '#'+selected.implementation_id : '?'}`;
    sourceDetails.appendChild(sourceSummary);
    if (selected.declaration_id) {
      const intent = el('div', 'sub', `◐ intent #${selected.declaration_id}: ${selected.intent_label || '(no label)'}${superseded(selected.declaration_superseded)}`);
      intent.title = `${selected.declaration_source_ref} · ${selected.declaration_source_digest}`;
      sourceDetails.appendChild(intent);
      sourceDetails.appendChild(el('div', 'sub', `source ${selected.declaration_source_ref} · ${shortDigest(selected.declaration_source_digest)} · ${when(selected.declaration_recorded_at)}`));
    }
    if (selected.revision_id) {
      const captureFacts = [
        `● Git snapshot #${selected.revision_id}`,
        when(selected.revision_captured_at),
        age(selected.revision_captured_at),
        `attempts ${selected.revision_capture_attempts || 1}`,
        selected.revision_non_atomic ? 'multi-command capture' : '',
        selected.revision_sparse_checkout ? 'sparse checkout' : '',
      ].filter(Boolean).join(' · ') + superseded(selected.revision_superseded);
      const snap = el('div', 'sub', captureFacts);
      snap.title = `${selected.revision_source_ref} · ${selected.revision_source_digest}`;
      sourceDetails.appendChild(snap);
      sourceDetails.appendChild(el('div', 'sub', `source ${selected.revision_source_ref} · ${shortDigest(selected.revision_source_digest)}`));
    }
    if (selected.implementation_id) {
      const claim = el('div', 'sub', `◐ implementation #${selected.implementation_id} · ${when(selected.implementation_recorded_at)}${superseded(selected.implementation_superseded)}`);
      claim.title = `${selected.implementation_source_ref} · ${selected.implementation_source_digest}`;
      sourceDetails.appendChild(claim);
      sourceDetails.appendChild(el('div', 'sub', `source ${selected.implementation_source_ref} · ${shortDigest(selected.implementation_source_digest)}`));
    }

    appendFileControls(box, repo, state, load);
    const fp=repo.file_page || {};
    const touchBoundary = !repo.completeness?.touched && ['session', 'touched'].includes(state.fileFilter) ? ' · T incomplete' : '';
    box.appendChild(el('div', 'sub evidence-match-count', `${pageValue(fp, 'total')} matches · ${pageValue(fp, 'returned')} shown${touchBoundary}`));
    const table = el('table', 'evidence-table evidence-tree evidence-change-tree');
    const cap = document.createElement('caption'); cap.textContent = 'File evidence membership';
    const thead = document.createElement('thead'); const hr = document.createElement('tr');
    for (const [name,title] of [['repository / file','Repository-relative file path'],['P','Declared plan membership'],['T','Observed governed touch'],['Δ','Observed checkout snapshot change'],['C','Claimed implementation membership'],['revision status','Git evidence layer and source status']]) { const th=document.createElement('th'); th.scope='col'; th.textContent=name; th.title=title; th.setAttribute('aria-label',title); hr.appendChild(th); }
    thead.appendChild(hr); table.append(cap, thead);
    const body = document.createElement('tbody');
    const fileGroups=new Map();
    for(const f of repo.files||[]){const slash=f.path.lastIndexOf('/');const dir=slash>=0?f.path.slice(0,slash):'.';if(!fileGroups.has(dir))fileGroups.set(dir,[]);fileGroups.get(dir).push({...f,display_path:slash>=0?f.path.slice(slash+1):f.path});}
    for (const [dir,files] of fileGroups) {
      appendFolder(body, state, `${repo.repository_id || ''}:${repo.checkout_id || ''}`, dir, files, f => {const tr=document.createElement('tr'); if(f.changed&&!f.planned)tr.classList.add('evidence-outside-plan');if(f.planned&&!f.changed)tr.classList.add('evidence-missing-change');if(f.touched&&!f.changed)tr.classList.add('evidence-touch-only'); const path=document.createElement('th'); path.scope='row'; path.appendChild(refAnchor(f.path, f.display_path)); attachFileDetail(ctx,path,f); path.title=f.old_path ? `from ${f.old_path}` : f.path; tr.appendChild(path);
      for (const [value,text,cls,available] of [[f.planned,'P','cl-claimed',repo.completeness?.planned],[f.touched,'T','cl-observed',repo.completeness?.touched],[f.changed,'Δ','cl-observed',repo.completeness?.changed],[f.claimed,'C','cl-claimed',repo.completeness?.claimed]]) { const td=document.createElement('td'); td.appendChild(mark(value,text,cls,available)); tr.appendChild(td); }
      const status=document.createElement('td'); const statusLayers=f.status_layers || []; status.textContent=statusLayers.map(statusMeaning).join(' · ') || 'no revision status'; status.title=statusLayers.join(', ') || 'No revision status'; tr.appendChild(status); return tr;});
    }
    table.appendChild(body); box.appendChild(table);
    if (pageValue(fp,'returned') < pageValue(fp,'total')) box.appendChild(el('div','sub',`Showing ${pageValue(fp,'returned')} of ${pageValue(fp,'total')} file rows.`));
    addPager(box, 'files', fp, state, 'changeOffset', load);
	const legend = el('div', 'evidence-change-legend');
	for (const [text, cls] of [['P plan','mark-p'],['T touch','mark-t'],['Δ revision','mark-d'],['C claim','mark-c']]) legend.appendChild(el('span', cls, text));
	box.appendChild(legend);
	box.appendChild(sourceDetails);

    if (!divergence.available) sourceDetails.appendChild(el('div','sub','? divergence unavailable — ' + (divergence.reason || 'complete declaration and revision evidence required')));
    appendGapDetails(box,repo.gaps||[]);
  }
  addPager(box, 'repository', c.repository_page, state, 'repositoryOffset', load,
    {changeOffset:0, observedOffset:0});
  appendActivity(box, ctx, c.governed_events || 0);
  appendSessionOverview(box, selection);
}

function appendActivity(box, ctx, count) {
  const disclosure=document.createElement('details');
  disclosure.className='evidence-detail-group evidence-activity';
  disclosure.dataset.sessionActivity='';
  const summary=document.createElement('summary');summary.textContent=`Activity · ${count} actions`;
  const host=el('div','evidence-lazy-subview');
  disclosure.append(summary,host);
  let loaded=false;
  disclosure.addEventListener('toggle',()=>{if(!disclosure.open||loaded)return;loaded=true;host.appendChild(el('div','sub','Loading captured actions…'));void renderSessionActivity(ctx,host);});
  box.appendChild(disclosure);
}

provider({
  id: 'session.change', order: 5, title: 'Changes', required: true,
  pane: { capability: 'evidence', alwaysOffered: true, group: 'evidence' },
  match: ctx => ctx.surface === 'session' && !!ctx.selection,
	activate: (_ctx, box, detail) => {
	  const state=box.cgState;
	  if(!state)return;
	  if(detail?.mode==='code'){
		const target=detail.codeTarget;
		const targetChanged=target && (state.codeTarget?.repository_id!==target.repository_id || state.codeTarget?.checkout_id!==target.checkout_id || state.codeTarget?.path!==target.path);
		if(targetChanged)state.codeTarget=target;
		if(state.mode!=='code'||targetChanged){state.mode='code';state.codeOffset=0;void state.reload?.();}
	  }
	  if(detail?.fileFilter&&state.fileFilter!==detail.fileFilter){state.mode='session';state.fileFilter=detail.fileFilter;state.changeOffset=0;void state.reload?.();}
	  if(detail?.activity){const activity=box.querySelector('[data-session-activity]');if(activity){activity.open=true;activity.dispatchEvent(new Event('toggle'));}}
	},
	render: async (ctx, box) => {
	const id = ctx.selection.thread_id || ctx.selection.id;
    if (!id) { box.appendChild(el('div', 'sub', '? no session id')); return; }
    const state = box.cgState || {mode:'code', repositoryOffset:0, changeOffset:0, observedOffset:0, observedScope:'all', fileFilter:'session', fileQuery:'', fileSort:'overlap', collapsedFolders:new Set(), codeCheckout:'', codeOffset:0, codeTarget:null, request:0};
    box.cgState = state;
    const load = async () => {
      const request = ++state.request;
	  if (state.mode === 'code') {
		beginChangeView(box, ctx.selection, state, load);
		const codeHost = el('div', 'evidence-code-state');
		codeHost.appendChild(el('div', 'sub', 'Loading collected code-state facts…'));
		box.appendChild(codeHost);
		await renderCodeState(ctx, codeHost, state, load, request);
		if(box.isConnected&&request===state.request)appendSessionOverview(box,ctx.selection);
		return;
	  }
	  const query = sessionQuery(ctx, {section:'change', repository_offset:state.repositoryOffset, change_offset:state.changeOffset, change_limit:25, change_filter:state.fileFilter, change_query:state.fileQuery, change_sort:state.fileSort, observed_offset:state.observedOffset, observed_limit:25, observed_scope:state.observedScope, impact_limit:1});
      const report = await api('/api/govern/session?' + query);
      if (!box.isConnected || request !== state.request) return;
	  renderChange(box, report.change || {}, report.id || id, state, load, ctx.selection, ctx);
    };
    state.reload=load;
    await load();
  },
});
