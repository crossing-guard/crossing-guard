// Bounded chronological action facts with explicit action/result drill-down.
import { el, debounce } from '../core.js';
import { sessionEvidence, factTable, cell, observedMillis } from './session-evidence.js';
import { retainedBodyButton } from './retained-body.js';

const bytes = value => Number.isFinite(value) ? `${value.toLocaleString()} B` : '?';
const time = observed => observed ? new Date(observedMillis(observed)).toLocaleString() : '?';

function addKeyValue(host, label, value) {
  const row=el('div','evidence-overview-row'); row.append(el('span','sub',label),el('code','',value == null || value === '' ? '?' : String(value))); host.appendChild(row);
}

function renderActionDetail(host, detail, ctx) {
  host.replaceChildren();
  const action=detail.action || {}; const event=action.event || {};
  const facts=el('div','evidence-overview-rows');
  addKeyValue(facts,'action',`#${event.id} · ${event.tool || '?'} · ${event.verb || '?'}`);
  addKeyValue(facts,'observed',`${time(event.ts)} · ${event.runtime || '?'} · ${event.origin || '?'}`);
  addKeyValue(facts,'decision',`${event.decision || '?'}${event.reason ? ` · ${event.reason}` : ''}`);
  if(action.delivery) addKeyValue(facts,'delivery',`${action.delivery.delivery_mode} · ${action.delivery.delivery_attempts} attempt(s) · ${action.delivery.native_call_kind || '?'}:${action.delivery.native_call_id || '?'}`);
  if(action.input){addKeyValue(facts,'input',`${action.input.media_type || '?'} · raw ${bytes(action.input.raw_bytes)} · captured ${bytes(action.input.captured_bytes)} · ${action.input.completeness || '?'}`);addKeyValue(facts,'input digest',action.input.digest || '?');if(action.input.completeness==='complete')facts.appendChild(retainedBodyButton(ctx,'Show captured input',{body_kind:'event_input',event_id:event.id}));}
  host.appendChild(facts);
  const resources=action.resources || [];
  if(resources.length){const table=factTable('Declared resources',['operation','resource','source','completeness']);for(const resource of resources){const row=document.createElement('tr');cell(row,resource.operation||'?');cell(row,`${resource.kind}:${resource.identity}`,true);cell(row,resource.source||'?');cell(row,resource.completeness||'?');table.tBodies[0].appendChild(row);}host.appendChild(table);}
  if(!(action.results||[]).length){host.appendChild(el('div','evidence-boundary','? no exact result joined to this action'));return;}
  if(action.results_truncated)host.appendChild(el('div','sub',`${action.results.length} of ${action.result_count||0} exact result observations shown`));
  for(const item of action.results){const result=item.observation||{};const group=document.createElement('details');group.open=true;const summary=document.createElement('summary');summary.textContent=`result #${result.id} · ${result.state || '?'} · ${result.join_class || '?'} · ${bytes(result.raw_bytes)} raw`;group.appendChild(summary);const resultFacts=el('div','evidence-overview-rows');addKeyValue(resultFacts,'timing',`${result.duration_ms || 0} ms · returned ${time(result.returned_at)} · completed ${time(result.completed_at)}`);addKeyValue(resultFacts,'bytes',`raw ${bytes(result.raw_bytes)} · field ${bytes(result.raw_field_bytes)} · decoded ${bytes(result.decoded_bytes)} · retained ${bytes(result.retained_bytes)} · stdout ${bytes(result.stdout_bytes)} · stderr ${bytes(result.stderr_bytes)}`);addKeyValue(resultFacts,'payload',`${result.media_type || '?'} · ${result.completeness || '?'} · ${result.payload_digest || '?'}`);addKeyValue(resultFacts,'reconciliation',item.reconciliation ? `${item.reconciliation.join_class} · ${item.reconciliation.algorithm}` : (result.action_coverage_class || '?'));if(result.completeness==='complete'&&result.retained_bytes>0)resultFacts.appendChild(retainedBodyButton(ctx,'Show retained result',{body_kind:'result',result_id:result.id}));group.appendChild(resultFacts);
    if(item.effects_truncated)group.appendChild(el('div','sub',`${(result.effects||[]).length} of ${item.effect_count||0} typed effects shown`));if((result.effects||[]).length){const effects=factTable('Typed file effects',['operation','path','content','diff','completeness']);for(const effect of result.effects){const row=document.createElement('tr');cell(row,effect.operation||'?');cell(row,effect.raw_identity||effect.entity_id||'?',true);const content=document.createElement('td');content.append(document.createTextNode(bytes(effect.content_bytes)));if(effect.content_bytes>0)content.appendChild(retainedBodyButton(ctx,'Show',{body_kind:'effect_content',result_id:result.id,ordinal:effect.ordinal}));row.appendChild(content);const diff=document.createElement('td');diff.append(document.createTextNode(bytes(effect.diff_bytes)));if(effect.diff_completeness==='complete'&&effect.diff_bytes>0)diff.appendChild(retainedBodyButton(ctx,'Show diff',{body_kind:'effect_diff',result_id:result.id,ordinal:effect.ordinal}));row.appendChild(diff);cell(row,`${effect.completeness||'?'} / ${effect.diff_completeness||'?'}`);effects.tBodies[0].appendChild(row);}group.appendChild(effects);}host.appendChild(group);}
}

function renderTrace(box, report, ctx, state, load) {
  box.replaceChildren();
  box.appendChild(el('div','evidence-sectionhead',`${(report.events||[]).length} shown of ${report.event_count||0} matching actions · chronological snapshot #${report.snapshot_id||0}`));
  const filters=el('div','evidence-filters');const search=document.createElement('input');search.type='search';search.maxLength=200;search.placeholder='Filter tool, resource, decision, lineage';search.setAttribute('aria-label','Filter trace facts');search.value=state.query;
  const decision=document.createElement('select');decision.setAttribute('aria-label','Filter trace by decision');for(const [value,label] of [['','All decisions'],['allow','allow'],['ask','ask'],['deny','deny']]){const option=document.createElement('option');option.value=value;option.textContent=label;option.selected=state.decision===value;decision.appendChild(option);}
  const origin=document.createElement('select');origin.setAttribute('aria-label','Filter trace by lineage');for(const [value,label] of [['','All lineage'],['live','live'],['transcript','transcript'],['imported','imported']]){const option=document.createElement('option');option.value=value;option.textContent=label;option.selected=state.origin===value;origin.appendChild(option);}
  const reset=()=>{state.cursors=[''];state.page=0;void load();};search.addEventListener('input',debounce(()=>{state.query=search.value.trim();reset();},180));decision.addEventListener('change',()=>{state.decision=decision.value;reset();});origin.addEventListener('change',()=>{state.origin=origin.value;reset();});filters.append(search,decision,origin);box.appendChild(filters);
  const byEvent=new Map();for(const resource of report.event_resources||[]){if(!byEvent.has(resource.event_id))byEvent.set(resource.event_id,[]);byEvent.get(resource.event_id).push(resource);}
  const table=factTable('Observed action and decision trace',['time','runtime','tool / verb','resource','decision','lineage']);
  for(const event of report.events||[]){const resources=byEvent.get(event.id)||[];const targets=resources.length?resources.map(resource=>`${resource.kind}:${resource.identity} · ${resource.operation||resource.source}`).join('\n'):(event.target_entity_id||'? resource unavailable');const row=document.createElement('tr');row.className='evidence-action-row';row.tabIndex=0;row.setAttribute('aria-label',`Open action ${event.id} details`);cell(row,new Date(observedMillis(event.ts)).toLocaleTimeString());cell(row,event.runtime||'?');cell(row,`${event.tool||'?'} · ${event.verb||'?'}`,true);cell(row,targets);cell(row,event.decision||'?');cell(row,event.origin||'?');table.tBodies[0].appendChild(row);const detailRow=document.createElement('tr');detailRow.hidden=true;const detailCell=document.createElement('td');detailCell.colSpan=6;detailCell.className='evidence-action-detail';detailRow.appendChild(detailCell);table.tBodies[0].appendChild(detailRow);let loaded=false;const open=async()=>{detailRow.hidden=!detailRow.hidden;if(detailRow.hidden||loaded)return;loaded=true;detailCell.appendChild(el('div','sub','Loading captured call and result…'));try{const detail=await sessionEvidence(ctx,'action',{event_id:event.id});if(detailCell.isConnected)renderActionDetail(detailCell,detail,ctx);}catch{detailCell.replaceChildren(el('div','evidence-state','Request failed'),el('div','sub','Action details could not be loaded.'));}};row.addEventListener('click',open);row.addEventListener('keydown',eventKey=>{if(eventKey.key==='Enter'||eventKey.key===' '){eventKey.preventDefault();void open();}});}
  box.appendChild(table);
  const pager=el('div','panel-pager');const previous=el('button','','Previous page');previous.disabled=state.page===0;previous.addEventListener('click',()=>{if(state.page>0){state.page--;void load();}});const page=el('span','sub',`page ${state.page+1}`);const next=el('button','','Next page');next.disabled=!report.next_cursor;next.addEventListener('click',()=>{if(report.next_cursor){state.cursors[state.page+1]=report.next_cursor;state.page++;void load();}});pager.append(previous,page,next);box.appendChild(pager);
}

export async function renderSessionActivity(ctx, box) {
  const state=box.cgState||{cursors:[''],page:0,query:'',decision:'',origin:'',request:0};
  box.cgState=state;
  const load=async()=>{const request=++state.request;const extra={limit:100,query:state.query,decision:state.decision,origin:state.origin};if(state.cursors[state.page])extra.cursor=state.cursors[state.page];try{const report=await sessionEvidence(ctx,'trace',extra);if(!box.isConnected||request!==state.request)return;renderTrace(box,report,ctx,state,load);}catch{if(!box.isConnected||request!==state.request)return;box.replaceChildren(el('div','evidence-state','Request failed'),el('div','sub','Activity evidence could not be loaded.'));}};
  await load();
}
