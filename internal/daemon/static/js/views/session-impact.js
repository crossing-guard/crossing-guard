// C7 session Impact. Facts only: exact generation identity, bounded typed
// relationships, direct revision nodes, and explicit coverage boundaries.
import { el, api } from '../core.js';
import { provider } from '../infopanel.js';
import { activatePane } from '../pane-host.js';
import { addPager } from './panel-pager.js';
import { sessionEvidence, sessionQuery, factTable, cell, observedMillis } from './session-evidence.js';
import { renderSessionVerification } from './session-verification.js';
import { loadExposure } from './sessions.js';

const short = value => value ? value.slice(0, 22) + (value.length > 22 ? '…' : '') : '?';
const pageValue = (page, key, fallback=0) => page?.[key] ?? fallback;

function impactBoundary(state) {
  return ({
    stale_source:'Understanding does not match the selected revision.',
    stale_analyzer:'The selected analyzer bundle differs from the stored understanding.',
    partial:'Some measured relationship families are incomplete.',
    failed:'Repository understanding was not completed for this snapshot.',
    unavailable:'Repository understanding is unavailable for this snapshot.',
  })[state] || '';
}

function candidateBoundary(impact) {
  if (impact.candidate_state === 'exact') return '';
  if (impact.candidate_reason === 'file center required') return 'Select one file to measure mechanical similarity.';
  return 'Exact file-centered similarity evidence is unavailable.';
}

function coverageBoundary(fact) {
  return ({
    partial:'producer reported partial coverage',
    failed:'producer reported failure',
    unsupported:'no selected producer for this fact family',
    unavailable:'fact family unavailable',
  })[fact.state] || '—';
}

function metricLink(value, label, target) {
  const button=el('button','evidence-value evidence-value-button',String(value));
  button.appendChild(el('small','',label));
  button.addEventListener('click',()=>target?.scrollIntoView({block:'start'}));
  return button;
}

function appendLazySubview(box, label, render) {
  const disclosure=document.createElement('details');disclosure.className='evidence-detail-group evidence-effect-subview';
  const summary=document.createElement('summary');summary.textContent=label;
  const host=el('div','evidence-lazy-subview');disclosure.append(summary,host);
  let loaded=false;disclosure.addEventListener('toggle',()=>{if(!disclosure.open||loaded)return;loaded=true;host.appendChild(el('div','sub','Loading captured facts…'));void render(host);});
  box.appendChild(disclosure);
}

function renderCoverage(box, coverage) {
  const table = factTable('Coverage by fact family', ['family', 'state', 'producer', 'facts / inputs', 'errors', 'boundary']);
  const body = table.tBodies[0];
  for (const fact of coverage || []) {
    const row = document.createElement('tr');
    cell(row, fact.family, true); cell(row, fact.state); cell(row, fact.analyzer_id);
    cell(row, `${fact.produced || 0} / ${fact.attempted || 0}`); cell(row, String(fact.errors || 0)); cell(row, coverageBoundary(fact));
    body.appendChild(row);
  }
  box.appendChild(table);
}

function detailText(detail) {
  const total = detail?.total || 0;
  const values = detail?.values || [];
  const returned = detail?.returned ?? values.length;
  if (!total) return '0';
  return `${total} · ${values.join(', ')}` + (detail?.exact ? '' : ` · ${returned} shown`);
}

function bodyShapeCell(row, candidate) {
  const node = document.createElement('td');
  const count = candidate.shared_body_shapes || {};
  const total = count.total || 0;
  const returned = count.returned ?? (candidate.body_shapes || []).length;
  node.appendChild(el('div', 'sub', count.exact === false ? `${returned} of ${total} shown` : String(total)));
  for (const shape of candidate.body_shapes || []) {
    const disclosure = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = `${short(shape.digest)} · ${shape.node_count} nodes`;
    disclosure.appendChild(summary);
    disclosure.appendChild(el('code', '', shape.digest));
    disclosure.appendChild(el('div', 'sub', `left declarations · ${detailText(shape.left_declarations)}`));
    disclosure.appendChild(el('div', 'sub', `right declarations · ${detailText(shape.right_declarations)}`));
    node.appendChild(disclosure);
  }
  row.appendChild(node);
}

function renderCandidates(box, impact, state, load, repository) {
  if(impact.candidate_state!=='exact'&&!(impact.candidates||[]).length)return null;
  const host=el('div','evidence-effect-population');host.dataset.effectPopulation='candidates';box.appendChild(host);
  host.appendChild(el('div', 'sub', 'candidate ≠ duplicate · exact mechanical matches only'));
  host.appendChild(el('div', 'sub', `candidate facts ${impact.candidate_state || 'unavailable'}` + (impact.candidate_analyzer_id ? ` · ${impact.candidate_analyzer_id}` : '')));
  const boundary=candidateBoundary(impact);if(boundary)host.appendChild(el('div', 'sub', boundary));
  const table = factTable('Mechanical code-shape candidates', ['other file', 'shared declarations', 'shared body shapes', 'shared internal dependencies', 'shared external dependencies']);
  table.dataset.effectPopulation='candidates';
  for (const candidate of impact.candidates || []) {
    const row = document.createElement('tr');
    const path=document.createElement('th');path.scope='row';path.appendChild(document.createTextNode(candidate.right_path || '?'));
    const inspect=el('button','evidence-inline-action','Code facts');inspect.title=`Open collected code-state facts for ${candidate.right_path || 'this file'}`;inspect.addEventListener('click',()=>activatePane('session.change',{mode:'code',codeTarget:{repository_id:repository.repository_id,checkout_id:repository.checkout_id,path:candidate.right_path}}));path.appendChild(inspect);row.appendChild(path);
    cell(row, detailText(candidate.shared_declarations));
    bodyShapeCell(row, candidate);
    cell(row, detailText(candidate.shared_internal_dependencies));
    cell(row, detailText(candidate.shared_external_dependencies));
    table.tBodies[0].appendChild(row);
  }
  host.appendChild(table);
  if (impact.candidate_state === 'exact' && pageValue(impact.candidate_page, 'total') === 0) {
    host.appendChild(el('div', 'sub', 'No exact declaration-name or body-shape candidate was measured for this file.'));
  }
  addPager(host, 'candidates', impact.candidate_page, state, 'candidateOffset', load);
  return host;
}

function renderEffectFacts(box, codeChanges, checkFacts, state, load) {
  const aggregate=codeChanges?.aggregates||{};
  const callers=aggregate.callers||{};
  const checks=checkFacts?.checks||[];
  const values=el('div','evidence-value-strip evidence-effect-primary-values');
  const add=(value,label,target,available=true)=>{if(!available)return;values.appendChild(metricLink(value,label,target));};

  let callerTable=null;
  if(['exact','partial'].includes(callers.state)){
    callerTable=factTable('Incoming calls to changed declarations',['caller','changed declaration','source','producer']);
    callerTable.dataset.effectPopulation='callers';
    for(const edge of callers.edges||[]){const row=document.createElement('tr');cell(row,edge.from_ref,true);cell(row,edge.to_ref);cell(row,edge.source_path?`${edge.source_path}${edge.source_line?':'+edge.source_line:''}`:'—');cell(row,edge.analyzer_id||'—');callerTable.tBodies[0].appendChild(row);}
  }
  let dependencyTable=null;
  if(['exact','partial'].includes(aggregate.dependency_state)){
    dependencyTable=factTable('Dependent code populations',['population','total','values','state']);
    for(const [label,facts] of [['dependent packages',aggregate.dependent_packages],['referencing files',aggregate.referencing_files]]){const row=document.createElement('tr');cell(row,label,true);cell(row,String(facts?.total||0));cell(row,(facts?.values||[]).join(', ')||'—');cell(row,aggregate.dependency_state);dependencyTable.tBodies[0].appendChild(row);}
  }
  const checkTable=factTable('Observed test and build actions',['action','time','kind','tool','result link','raw / retained']);
  checkTable.dataset.effectPopulation='checks';
  for(const check of checks){const row=document.createElement('tr');cell(row,`#${check.event_id}`,true);cell(row,new Date(observedMillis(check.observed_at)).toLocaleString());cell(row,(check.kinds||[]).join(', ')||'—');cell(row,`${check.tool||'?'} · ${check.verb||'?'}`);cell(row,check.result_state||'missing');const raw=(check.results||[]).reduce((sum,result)=>sum+(result.raw_bytes||0),0);const retained=(check.results||[]).reduce((sum,result)=>sum+(result.retained_bytes||0),0);cell(row,`${raw} B / ${retained} B`);checkTable.tBodies[0].appendChild(row);}

  add(callers.total||0,'call edges',callerTable,['exact','partial'].includes(callers.state));
  add(aggregate.dependent_packages?.total||0,'dependent packages',dependencyTable,['exact','partial'].includes(aggregate.dependency_state));
  add(checkFacts?.page?.total||0,'checks',checkTable,checkFacts?.state==='observed');
  if(values.childNodes.length)box.appendChild(values);
  if(callerTable){box.appendChild(el('div','sub evidence-code-family-boundary',`${callers.returned||0} of ${callers.total||0} incoming call edges · ${callers.declaration_returned||0} of ${callers.declaration_total||0} current changed declarations · ${callers.state}`));box.appendChild(callerTable);}
  else if(callers.reason)box.appendChild(el('div','sub evidence-boundary',`call edges · ${callers.state||'unavailable'} · ${callers.reason}`));
  if(dependencyTable){box.appendChild(el('div','sub evidence-code-family-boundary',`package dependency facts · ${aggregate.dependency_state}`));box.appendChild(dependencyTable);}
  else if(aggregate.dependency_reason)box.appendChild(el('div','sub evidence-boundary',`package dependencies · ${aggregate.dependency_state||'unavailable'} · ${aggregate.dependency_reason}`));
  if(checkFacts?.state!=='observed'&&checkFacts?.reason)box.appendChild(el('div','sub evidence-boundary',`checks · ${checkFacts.state||'unavailable'} · ${checkFacts.reason}`));
  box.appendChild(checkTable);
  addPager(box,'checks',checkFacts?.page,state,'checkOffset',load);
}

function unavailableCodeEffects() {
  return {aggregates:{callers:{state:'unavailable',reason:'Code effect facts could not be loaded',edges:[],coverage:[]},
    dependency_state:'unavailable',dependency_reason:'Code dependency facts could not be loaded',
    dependent_packages:{values:[]},referencing_files:{values:[]}}};
}

function unavailableChecks(state) {
  return {state:'unavailable',reason:'Observed check facts could not be loaded',checks:[],
    page:{offset:state.checkOffset,limit:25,total:0,returned:0}};
}

function renderImpact(box, change, codeChanges, checkFacts, selection, state, load) {
  box.replaceChildren();
  renderEffectFacts(box,codeChanges,checkFacts,state,load);
  const repo = (change.repositories || [])[0];
  const repositoryPage = change.repository_page || {};
  const repositoryCount = change.repository_count || 0;
  if (!repo) {
    box.appendChild(el('div', 'evidence-state', 'Repository snapshot not captured'));
    box.appendChild(el('div', 'sub', 'No linked repository change record is available for this session.'));
    addPager(box, 'repository', repositoryPage, state, 'repositoryOffset', load,
      {nodeOffset:0, edgeOffset:0, candidateOffset:0, center:''});
    return;
  }
  const repositoryNumber = pageValue(repositoryPage, 'offset') + 1;
  const repository = el('div', 'sub', `repository ${repositoryNumber} of ${repositoryCount} · ${repo.checkout_display || repo.repository_id}`);
  repository.title = `${repo.repository_id} · checkout ${repo.checkout_id} · ${repo.portability}`;
  box.appendChild(repository);
  addPager(box, 'repository', repositoryPage, state, 'repositoryOffset', load,
    {nodeOffset:0, edgeOffset:0, candidateOffset:0, center:''});
  const impact = repo.downstream || {state:'unavailable', reason:'impact evidence unavailable'};
  const generation = impact.generation;
  box.appendChild(el('div', 'sub', `${impact.state === 'exact' ? '●' : impact.state === 'partial' ? '◆' : '?'} ${impact.state}` + (generation ? ` · generation #${generation.id}` : '')));
  const boundary=impactBoundary(impact.state);if(boundary)box.appendChild(el('div', 'sub', boundary));
  if (!generation) {
    box.appendChild(el('div','sub','No exact stored understanding generation is linked to this checkout snapshot.'));
    return;
  }
  if (impact.center) box.appendChild(el('div', 'sub', `center ${impact.center.kind}:${impact.center.ref} · ${impact.center.provenance} #${impact.center.source_id}`));

  const candidates=renderCandidates(box, impact, state, load, repo);

  const nodes = factTable('Nodes', ['scope', 'kind', 'identity', 'provenance', 'source', 'inspect']);
  nodes.dataset.effectPopulation='nodes';
  for (const node of impact.nodes || []) {
    const row = document.createElement('tr');
    cell(row, node.direct ? 'direct Δ' : 'related'); cell(row, node.kind); cell(row, node.ref, true);
    cell(row, node.provenance); cell(row, `#${node.source_id}`);
    const actionCell = document.createElement('td');
    const recenter = el('button', '', 'Center');
    recenter.title = `Center impact facts on ${node.kind}:${node.ref}`;
    recenter.setAttribute('aria-label', `Center on ${node.kind}:${node.ref}`);
    recenter.addEventListener('click', () => { state.center = `${node.kind}:${node.ref}`; state.nodeOffset = 0; state.edgeOffset = 0; state.candidateOffset = 0; void load(); });
    actionCell.appendChild(recenter);
    if (node.kind === 'file' && repositoryCount === 1) {
      const codeFacts=el('button','','Code facts');codeFacts.title=`Open collected code-state facts for ${node.ref}`;codeFacts.addEventListener('click',()=>activatePane('session.change',{mode:'code',codeTarget:{repository_id:repo.repository_id,checkout_id:repo.checkout_id,path:node.ref}}));actionCell.appendChild(codeFacts);
      const open = el('button', '', 'Editor ↗');
      open.title = `Open ${node.ref} in the editor`;
      open.setAttribute('aria-label', `Open file:${node.ref} in editor`);
      open.addEventListener('click', () => document.dispatchEvent(new CustomEvent('cg:open-editor', {detail:{root:selection.cwd || selection.project || '', path:node.ref}})));
      actionCell.appendChild(open);
    } else if (node.kind === 'file') {
      const unavailable = el('button', '', 'Editor unavailable · multiple repositories');
      unavailable.disabled = true;
      unavailable.title = 'Editor unavailable: multiple repositories';
      unavailable.setAttribute('aria-label', 'Editor unavailable: multiple repositories');
      actionCell.appendChild(unavailable);
    }
    row.appendChild(actionCell); nodes.tBodies[0].appendChild(row);
  }
  box.appendChild(nodes);
  if (pageValue(impact.node_page, 'returned') < pageValue(impact.node_page, 'total')) box.appendChild(el('div', 'sub', `Showing ${pageValue(impact.node_page, 'returned')} of ${pageValue(impact.node_page, 'total')} nodes.`));
  addPager(box, 'nodes', impact.node_page, state, 'nodeOffset', load);

  const edges = factTable('Typed edges', ['from', 'relation', 'to', 'provenance', 'source', 'producer', 'evidence']);
  edges.dataset.effectPopulation='edges';
  for (const edge of impact.edges || []) {
    const row = document.createElement('tr');
    cell(row, `${edge.from_kind}:${edge.from_ref}`, true); cell(row, edge.relation); cell(row, `${edge.to_kind}:${edge.to_ref}`); cell(row, edge.provenance);
    cell(row, edge.source_path ? `${edge.source_path}${edge.source_line ? ':' + edge.source_line : ''}` : '—');
    cell(row, edge.analyzer_id); cell(row, short(edge.evidence), false, edge.evidence); edges.tBodies[0].appendChild(row);
  }
  box.appendChild(edges);
  if (pageValue(impact.edge_page, 'returned') < pageValue(impact.edge_page, 'total')) box.appendChild(el('div', 'sub', `Showing ${pageValue(impact.edge_page, 'returned')} of ${pageValue(impact.edge_page, 'total')} edges.`));
  addPager(box, 'edges', impact.edge_page, state, 'edgeOffset', load);
  const values=el('div','evidence-value-strip evidence-effect-values');
  if(impact.candidate_state==='exact'&&impact.candidate_page?.exact===true)values.appendChild(metricLink(pageValue(impact.candidate_page,'total'),'matches',candidates));
  if(impact.node_page?.exact===true)values.appendChild(metricLink(pageValue(impact.node_page,'total'),'nodes',nodes));
  if(impact.edge_page?.exact===true)values.appendChild(metricLink(pageValue(impact.edge_page,'total'),'edges',edges));
  if(values.childNodes.length)box.insertBefore(values,candidates||nodes);

  const diagnostics=document.createElement('details');diagnostics.className='evidence-detail-group evidence-effect-diagnostics';
  const diagnosticsSummary=document.createElement('summary');diagnosticsSummary.textContent='Analysis source and coverage';diagnostics.appendChild(diagnosticsSummary);
  const source = el('div', 'sub', `source ${generation.snapshot_protocol} · ${short(generation.snapshot_digest)}`);
  source.title = generation.snapshot_digest; diagnostics.appendChild(source);
  const analyzers = el('div', 'sub', `schema ${generation.structural_schema} · analyzers ${short(generation.analyzer_bundle_digest)}`);
  analyzers.title = generation.analyzer_bundle_digest; diagnostics.appendChild(analyzers);
  const configuration = el('div', 'sub', `configuration ${generation.convention_state}` + (generation.convention_source_ref ? ` · ${generation.convention_source_ref} · ${short(generation.convention_source_digest)}` : ''));
  if (generation.convention_source_digest) configuration.title = generation.convention_source_digest;
  diagnostics.appendChild(configuration);
  diagnostics.appendChild(el('div','sub',`similarity ${impact.candidate_state||'unavailable'}${candidateBoundary(impact)?` · ${candidateBoundary(impact)}`:''}`));
  renderCoverage(diagnostics, impact.coverage);box.appendChild(diagnostics);
}

provider({
  id: 'session.impact', order: 6, title: 'Effects', required: true,
  pane: { capability: 'evidence', alwaysOffered: true, group: 'evidence' },
  match: ctx => ctx.surface === 'session' && !!ctx.selection,
  render: async (ctx, box) => {
    const id = ctx.selection.thread_id || ctx.selection.id;
    if (!id) { box.appendChild(el('div', 'sub', '? no session id')); return; }
    const state = box.cgState || {repositoryOffset:0, nodeOffset:0, edgeOffset:0, candidateOffset:0, checkOffset:0, center:'', request:0};
    box.cgState = state;
    const load = async () => {
      const request = ++state.request;
      const query = sessionQuery(ctx, {section:'impact', repository_offset:state.repositoryOffset, impact_node_offset:state.nodeOffset, impact_edge_offset:state.edgeOffset, impact_candidate_offset:state.candidateOffset, impact_limit:100});
      if (state.center) query.set('impact_center', state.center);
      try {
        const [impactResult,codeResult,checkResult] = await Promise.allSettled([
          api('/api/govern/session?' + query),
          sessionEvidence(ctx,'code-changes',{code_limit:1}),
          sessionEvidence(ctx,'checks',{check_offset:state.checkOffset,check_limit:25}),
        ]);
        if (!box.isConnected || request !== state.request) return;
        const report=impactResult.status==='fulfilled'?impactResult.value:{change:{reason:'Repository effect facts could not be loaded'}};
        const codeReport=codeResult.status==='fulfilled'?codeResult.value.code_changes:unavailableCodeEffects();
        const checkReport=checkResult.status==='fulfilled'?checkResult.value.checks:unavailableChecks(state);
        renderImpact(box, report.change || {}, codeReport || {}, checkReport || {}, ctx.selection, state, load);
        appendLazySubview(box,'Verification',host=>renderSessionVerification(ctx,host));
        appendLazySubview(box,'Data and secrets',host=>loadExposure(host,null,ctx.selection.runtime,ctx.selection.thread_id||ctx.selection.id));
      } catch {
        if (!box.isConnected || request !== state.request) return;
        box.replaceChildren();
        box.appendChild(el('div', 'evidence-state', 'Request failed'));
        box.appendChild(el('div', 'sub', 'Effect evidence could not be loaded.'));
        const retry = el('button', '', 'Retry effects');
        retry.addEventListener('click', () => { void load(); });
        box.appendChild(retry);
      }
    };
    await load();
  },
});
