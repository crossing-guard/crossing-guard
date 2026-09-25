// Exact declared/change/claim set relationships. This view projects the existing
// SessionView; it does not infer intent from conversation text or create decisions.
import { el, api, refAnchor } from '../core.js';
import { provider } from '../infopanel.js';
import { addPager } from './panel-pager.js';
import { sessionEvidence, sessionQuery } from './session-evidence.js';
import { presentTranscriptIndexCoverage } from '../transcript-index-coverage.js';

const pageValue=(page,key,fallback=0)=>page?.[key]??fallback;
const short=value=>value?value.slice(0,22)+(value.length>22?'…':''):'?';
const statementTime=value=>{const date=new Date(value);return Number.isNaN(date.getTime())?'—':date.toLocaleString();};

function membership(on,label,available) {
  const node=el('span',`evidence-cell-mark ${on?'cl-claimed':available?'mark-empty':'mark-unknown'}`,on?'●':available?'·':'?');
  node.title=on?`${label} membership recorded`:available?`Not in ${label} set`:`${label} evidence unavailable`;
  node.setAttribute('aria-label',node.title);return node;
}

function valueButton(value,label,filter,state,load,available=true) {
  if(!available)return null;
  const button=el('button','evidence-value evidence-value-button',String(value));button.appendChild(el('small','',label));
  button.title=`Show ${label} rows`;button.addEventListener('click',()=>{state.filter=filter;state.fileOffset=0;void load();});return button;
}

function renderStatements(box, response, state, load) {
  const facts=response?.statements||{};
  const page=facts?.page||{};
  const values=el('div','evidence-value-strip evidence-plan-statement-values');
  const count=el('div','evidence-value',String(pageValue(page,'total')));count.appendChild(el('small','','attributed statements'));values.appendChild(count);box.appendChild(values);
  const coverage=presentTranscriptIndexCoverage(response?.coverage);
  const coverageValue=el('div','evidence-value transcript-index-coverage');coverageValue.dataset.state=coverage.state;
  coverageValue.appendChild(el('small','transcript-index-coverage-state',coverage.label));values.appendChild(coverageValue);
  box.appendChild(el('div','sub evidence-code-family-boundary',
    `${pageValue(page,'returned')} of ${pageValue(page,'total')} shown · ${facts?.state||'unavailable'} · ${facts?.source||'source unavailable'}`));
  box.appendChild(el('div','evidence-state transcript-index-coverage-detail',coverage.detail));
  box.appendChild(el('div','evidence-boundary transcript-index-coverage-time',coverage.timestampLabel));
  if(coverage.recovery)box.appendChild(el('div','evidence-boundary transcript-index-coverage-recovery',coverage.recovery));
  if(pageValue(page,'total')===0)box.appendChild(el('div','evidence-boundary transcript-index-zero',coverage.zeroAuthoritative
    ?`No attributed statements are indexed as of ${coverage.timestamp}.`
    :'No attributed statements are shown; this zero is not authoritative while transcript coverage is limited.'));
  if(facts?.reason)box.appendChild(el('div','sub evidence-boundary',facts.reason));
  if((facts?.statements||[]).length){const table=el('table','evidence-table evidence-plan-statements');const caption=document.createElement('caption');caption.textContent='Attributed user and assistant statements';const head=document.createElement('thead');const header=document.createElement('tr');for(const name of ['time','source','statement']){const th=document.createElement('th');th.scope='col';th.textContent=name;header.appendChild(th);}head.appendChild(header);table.append(caption,head,document.createElement('tbody'));for(const statement of facts.statements){const row=document.createElement('tr');const time=document.createElement('th');time.scope='row';time.textContent=statementTime(statement.observed_at);row.appendChild(time);const source=document.createElement('td');source.textContent=`${statement.role||'?'} · ${statement.runtime||'?'}`;row.appendChild(source);const text=document.createElement('td');text.textContent=statement.snippet||'';text.title=statement.snippet_truncated?'Sanitized statement snippet is truncated':'';row.appendChild(text);table.tBodies[0].appendChild(row);}box.appendChild(table);}
  addPager(box,'statements',page,state,'statementOffset',load);
}

function unavailableStatements(state) {
  return {statements:{state:'unavailable',reason:'Attributed statement facts could not be loaded',source:'source unavailable',
    statements:[],page:{offset:state.statementOffset,limit:25,total:0,returned:0}}};
}

function renderPlan(box,change,statementResponse,state,load) {
  box.replaceChildren();
  renderStatements(box,statementResponse,state,load);
  const repo=(change.repositories||[])[0];
  if(!repo){box.appendChild(el('div','evidence-state','Declaration/change sets not captured'));box.appendChild(el('div','sub',change.reason||'No linked repository declaration and checkout snapshot are available.'));return;}

  const counts=repo.counts||{};const complete=repo.completeness||{};const divergence=repo.divergence||{};
  const values=el('div','evidence-value-strip evidence-plan-values');
  for(const value of [
    valueButton(counts.planned||0,'declared','planned',state,load,complete.planned===true),
    valueButton(counts.changed||0,'changed','changed',state,load,complete.changed===true),
    valueButton(counts.claimed||0,'claimed','claimed',state,load,complete.claimed===true),
    valueButton(divergence.added?.total||0,'change − plan','outside-plan',state,load,divergence.available===true),
    valueButton(divergence.omitted?.total||0,'plan − change','planned',state,load,divergence.available===true),
  ])if(value)values.appendChild(value);
  if(values.childNodes.length)box.appendChild(values);

  const selected=repo.selected||{};
  const identity=el('div','sub',repo.checkout_display||repo.repository_id||'?');
  identity.title=`${repo.repository_id||'?'} · checkout ${repo.checkout_id||'?'} · ${repo.portability||'?'}`;box.appendChild(identity);
  if(!divergence.available)box.appendChild(el('div','evidence-boundary',`? set relationship unavailable · ${divergence.reason||'complete declaration and checkout snapshot required'}`));

  const table=el('table','evidence-table evidence-plan-table');
  const caption=document.createElement('caption');caption.textContent='Declared, checkout-changed, and claimed file membership';
  const head=document.createElement('thead');const header=document.createElement('tr');
  for(const [name,title] of [['path','Repository-relative path'],['P','Declared plan membership'],['Δ','Checkout snapshot change'],['C','Claimed implementation membership'],['status','Checkout revision status']]){const th=document.createElement('th');th.scope='col';th.textContent=name;th.title=title;header.appendChild(th);}head.appendChild(header);table.append(caption,head,document.createElement('tbody'));
  for(const file of repo.files||[]){const row=document.createElement('tr');const path=document.createElement('th');path.scope='row';path.appendChild(refAnchor(file.path,file.path));row.appendChild(path);for(const [mark,label,available] of [[file.planned,'plan',complete.planned],[file.changed,'change',complete.changed],[file.claimed,'claim',complete.claimed]]){const td=document.createElement('td');td.appendChild(membership(mark,label,available===true));row.appendChild(td);}const status=document.createElement('td');status.textContent=(file.status_layers||[]).join(' · ')||'—';row.appendChild(status);table.tBodies[0].appendChild(row);}
  box.appendChild(table);
  const page=repo.file_page||{};box.appendChild(el('div','sub',`${pageValue(page,'returned')} of ${pageValue(page,'total')} ${state.filter} rows shown`));
  addPager(box,'files',page,state,'fileOffset',load);

  const sources=document.createElement('details');sources.className='evidence-detail-group';
  const sourceSummary=document.createElement('summary');sourceSummary.textContent='Set sources';sources.appendChild(sourceSummary);
  const sourceRows=[
    ['declaration',selected.declaration_id,selected.declaration_source_ref,selected.declaration_source_digest],
    ['checkout snapshot',selected.revision_id,selected.revision_source_ref,selected.revision_source_digest],
    ['implementation claim',selected.implementation_id,selected.implementation_source_ref,selected.implementation_source_digest],
  ];
  for(const [label,id,ref,digest] of sourceRows){const row=el('div','sub',`${id?'●':'?'} ${label} ${id?'#'+id:'unavailable'} · ${ref||'?'} · ${short(digest)}`);row.title=digest||'source digest unavailable';sources.appendChild(row);}
  sources.appendChild(el('div','sub','Structured decision records · not collected by this projection'));
  box.appendChild(sources);
  addPager(box,'repository',change.repository_page,state,'repositoryOffset',load,{fileOffset:0});
}

provider({
  id:'session.plan',order:7,title:'Plan',required:true,
  pane:{capability:'evidence',alwaysOffered:true,icon:'<svg viewBox="0 0 24 24"><path d="M9 6h11M9 12h11M9 18h11"/><path d="M4 6l1 1 2-2M4 12l1 1 2-2M4 18l1 1 2-2"/></svg>'},
  match:ctx=>ctx.surface==='session'&&!!ctx.selection,
  render:async(ctx,box)=>{
    const state=box.cgState||{repositoryOffset:0,fileOffset:0,statementOffset:0,filter:'changed',request:0};box.cgState=state;
    const load=async()=>{const request=++state.request;const query=sessionQuery(ctx,{section:'change',repository_offset:state.repositoryOffset,change_offset:state.fileOffset,change_limit:25,change_filter:state.filter,change_sort:'overlap',observed_limit:1,impact_limit:1});try{const [changeResult,statementResult]=await Promise.allSettled([api('/api/govern/session?'+query),sessionEvidence(ctx,'statements',{statement_offset:state.statementOffset,statement_limit:25})]);if(!box.isConnected||request!==state.request)return;const change=changeResult.status==='fulfilled'?changeResult.value.change:{reason:'Declaration/change set facts could not be loaded'};const statements=statementResult.status==='fulfilled'?statementResult.value:unavailableStatements(state);renderPlan(box,change||{},statements||{},state,load);}catch{if(!box.isConnected||request!==state.request)return;box.replaceChildren(el('div','evidence-state','Request failed'),el('div','sub','Plan evidence could not be loaded.'));const retry=el('button','','Retry plan');retry.addEventListener('click',()=>{void load();});box.appendChild(retry);}};
    await load();
  },
});
