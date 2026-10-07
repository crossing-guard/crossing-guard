// C9 fact-first session evidence shell. Summary and section reads are deliberately
// separate: a tab never downloads unrelated action/result/change populations.
import { el, api } from '../core.js';
import { provider } from '../infopanel.js';
import { activatePane } from '../pane-host.js';
import { governanceLink, openInGovernance } from './session-governance.js';
import { renderUsageStrip } from './usage-strip.js';
import { loadSessionTags } from '../session-organization/organization-api.js';
import { tagLabel } from '../session-organization/tag-chips.js';
import { nativeOpenControl } from '../session/native-open.js';

const FRESH_MS = 30000;
const cache = new Map();
export const sessionQuery = (ctx, extra={}) => new URLSearchParams({
  id: ctx.selection.thread_id || ctx.selection.id,
  runtime: ctx.selection.runtime || '', ...extra,
});
export function clearSessionEvidence() { cache.clear(); }
document.addEventListener('cg:panel-selection-reset', clearSessionEvidence);

export function sessionEvidence(ctx, section='summary', extra={}) {
  const query = sessionQuery(ctx, {section, ...extra});
  const key = query.toString();
  const existing = cache.get(key);
  if (existing?.data && Date.now() - existing.at < FRESH_MS) return Promise.resolve(existing.data);
  if (existing?.request) return existing.request;
  while (cache.size >= 64) cache.delete(cache.keys().next().value);
  const request = api('/api/govern/session?' + query).then(data => {
    cache.set(key, {data, at:Date.now()});
    return data;
  }).catch(error => {
    if (existing?.data) cache.set(key, existing); else cache.delete(key);
    throw error;
  });
  cache.set(key, {...existing, request});
  return request;
}
export function factTable(captionText, headings) {
  const table=el('table','evidence-table'); const cap=document.createElement('caption'); cap.textContent=captionText;
  const head=document.createElement('thead'); const row=document.createElement('tr');
  for (const heading of headings) { const th=document.createElement('th'); th.scope='col'; th.textContent=heading; row.appendChild(th); }
  head.appendChild(row); table.append(cap,head,document.createElement('tbody')); return table;
}
export function cell(row, value, heading=false, title='') {
  const node=document.createElement(heading?'th':'td'); if (heading) node.scope='row';
  node.textContent=value ?? '—'; if (title) node.title=title; row.appendChild(node); return node;
}

export const qualifiedKnown = (value, complete) => `${value}${complete ? '' : '+?'}`;
export const observedMillis = value => {
  const n=Number(value)||0;
  if(n>=1e17)return n/1e6;
  if(n>=1e14)return n/1e3;
  if(n>=1e11)return n;
  return n*1000;
};
export function fileEvidencePopulations(change={}) {
  const repos = change.repositories || [];
  const sum = key => repos.reduce((total, repo) => total + (repo.counts?.[key] || 0), 0);
  const allComplete = key => repos.length > 0 && repos.every(repo => repo.completeness?.[key] === true);
  const nonFileValues = repos.map(repo => repo.counts?.non_file_events || 0);
  const nonFileConsistent = nonFileValues.length < 2 || nonFileValues.every(value => value === nonFileValues[0]);
  const knownTouched = sum('touched_total');
  const checkoutChanged = sum('changed');
  const overlap = sum('touched_changed_known');
  const changedComplete = allComplete('changed');
  return {
    available: change.available === true && repos.length > 0,
    repositoryCount: change.repository_count || repos.length,
    knownTouched,
    touchComplete: allComplete('touched'),
    checkoutChanged,
    changedComplete,
    overlap,
    checkoutWithoutExplicitTouch: changedComplete ? Math.max(0, checkoutChanged - overlap) : null,
    explicitTouchAbsentSnapshot: changedComplete ? Math.max(0, knownTouched - overlap) : null,
    nonFileActions: nonFileConsistent ? (nonFileValues[0] || 0) : null,
    nonFileConsistent,
  };
}

function headerFacts(summary) {
  const held=(summary.decisions?.deny || 0) + (summary.decisions?.ask || 0);
  return [
    {label:'actions',value:summary.event_count||0,title:'Exact governed actions captured for this session',open:()=>activatePane('session.change',{activity:true})},
    {label:'file targets',value:summary.file_target_count||0,title:'Distinct exact file targets observed in governed actions',open:()=>activatePane('session.change',{fileFilter:'touched'})},
    {label:'checkout changed',value:summary.facts?.checkout_changed||0,title:'Paths changed in the latest captured checkout snapshots; actor/session attribution is not established',open:()=>activatePane('session.change',{fileFilter:'changed'})},
    {label:'hold',value:held,title:'Exact deny or ask decisions',open:null},
  ];
}

provider({id:'session.evidence.header', order:0, zone:'header', title:'Session evidence', required:true,
  match:ctx=>ctx.surface==='session' && !!ctx.selection,
  render:async(ctx,box)=>{
    const report=await sessionEvidence(ctx,'summary'); if(!box.isConnected)return;
    const summary=report.summary || {};
    const meta=el('div','evidence-meta'); meta.append(el('span','',ctx.selection.runtime || summary.runtime || '? runtime'),el('span','',`source · ${ctx.selection.title_source || summary.title_source || 'unavailable'}`));
    const native=nativeOpenControl(ctx.selection); if(native) meta.append(native);
    box.append(meta,el('h2','evidence-title',ctx.selection.title || summary.title || 'Untitled session'));
    const counts=el('div','evidence-value-strip evidence-header-values');
    for(const fact of headerFacts(summary)){const card=el('button','evidence-value evidence-value-button',String(fact.value));card.title=fact.title;card.setAttribute('aria-label',`${fact.label}: ${fact.value}. ${fact.title}`);card.appendChild(el('small','',fact.label));if(fact.open)card.addEventListener('click',fact.open);else card.addEventListener('click',()=>openInGovernance({id:summary.id||ctx.selection.thread_id||ctx.selection.id,runtime:ctx.selection.runtime,title:ctx.selection.title,title_source:ctx.selection.title_source,tags:[]}));counts.appendChild(card);} box.appendChild(counts);
    if(!summary.found) box.appendChild(el('div','evidence-boundary','? no captured session evidence'));
    // What detectors saw: derived facts, shown
    // as evidence (◆) rather than as tags beside the owner's own.
    const derived=el('div','evidence-derived-facts');
    box.appendChild(derived);
    void loadSessionTags({runtime:ctx.selection.runtime,id:ctx.selection.id}).then(found=>{
      if(!derived.isConnected)return;
      for(const fact of found.sessions?.[0]?.facts||[]) derived.appendChild(el('span','evidence-fact','◆ '+tagLabel(fact)));
    }).catch(()=>{});
  }});

provider({id:'session.evidence.pinned', order:0, zone:'pinned', title:'Evidence boundary', required:true,
  match:ctx=>ctx.surface==='session' && !!ctx.selection,
  render:async(ctx,box)=>{const report=await sessionEvidence(ctx,'summary'); if(!box.isConnected)return; const summary=report.summary || {};
    const footer=el('div','evidence-footer');
    footer.appendChild(el('span','evidence-fresh',`last action ${summary.last_action_at ? new Date(observedMillis(summary.last_action_at)).toLocaleString() : 'unavailable'}`));
    footer.appendChild(el('span','evidence-fresh',`${summary.facts?.results||0} results · ${summary.facts?.open_issues||0} gaps`));
    footer.appendChild(governanceLink(ctx.selection,summary));
    box.appendChild(footer);
    const transcript=el('div','evidence-footer');
    const events=(ctx.selection.events||[]).length;
    transcript.appendChild(el('span','evidence-fresh',events+' transcript event'+(events===1?'':'s')));
    if(ctx.selection.modified) transcript.appendChild(el('span','evidence-fresh','last written '+new Date(ctx.selection.modified).toLocaleString()));
    box.appendChild(transcript);
    if(ctx.selection.usage){const usage=renderUsageStrip(ctx.selection.usage);usage.classList.add('evidence-footer-usage');box.appendChild(usage);}
    const diagnostics=document.createElement('details');diagnostics.className='evidence-detail-group evidence-diagnostics';
    const diagnosticsSummary=document.createElement('summary');diagnosticsSummary.dataset.sessionDiagnostics='';diagnosticsSummary.textContent='Diagnostics · provenance · capture';diagnostics.appendChild(diagnosticsSummary);
    // Lines the transcript reader could not map are a count, stated plainly.
    if(ctx.selection.unparsed>0) diagnostics.appendChild(el('div','sub',ctx.selection.unparsed+' transcript line'+(ctx.selection.unparsed===1?'':'s')+' not shown'));
    if(ctx.selection.transcript_note) diagnostics.appendChild(el('div','sub',ctx.selection.transcript_note));
    const host=el('div','evidence-lazy-subview');diagnostics.appendChild(host);let loaded=false;
    diagnostics.addEventListener('toggle',()=>{if(!diagnostics.open||loaded)return;loaded=true;host.appendChild(el('div','sub','Loading capture diagnostics…'));void import('./session-reach.js').then(module=>module.renderSessionDiagnostics(ctx,host)).catch(()=>host.replaceChildren(el('div','evidence-state','Request failed'),el('div','sub','Capture diagnostics could not be loaded.')));});
    box.appendChild(diagnostics);
    box.appendChild(el('div','evidence-legend','● observed   ◆ derived   ◐ claimed   ? unavailable'));
  }});
