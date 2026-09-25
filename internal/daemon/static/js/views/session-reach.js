// Session-observed reach stays separate from installed global runtime/config state.
import { el, api } from '../core.js';
import { sessionEvidence, factTable, cell } from './session-evidence.js';

export async function renderSessionDiagnostics(ctx,box) {
  try {
    const [report,health,runtimes,detectors]=await Promise.all([sessionEvidence(ctx,'reach'),api('/api/govern/health').catch(()=>null),api('/api/govern/runtimes').catch(()=>null),api('/api/policy/detectors').catch(()=>null)]);if(!box.isConnected)return;
    box.replaceChildren();
    box.appendChild(el('div','evidence-sectionhead','Observed in this session · client surface unavailable'));
    const table=factTable('Runtime and decision modes',['runtime','capture','allow','ask','deny','client']);
    for(const stat of report.reach?.runtimes||[]){const row=document.createElement('tr');cell(row,stat.runtime||'?',true);cell(row,String(stat.actions||0));for(const d of ['allow','ask','deny'])cell(row,String(stat[d]||0));cell(row,'?');table.tBodies[0].appendChild(row);}box.appendChild(table);
    box.appendChild(el('div','evidence-sectionhead','Installed runtime / configuration state · global scope'));
    const global=factTable('Installed facts',['owner','available / active evidence']);
    for(const [owner,value] of [['capture',health?`${health.configured?'●':'?'} ${health.total_events||0} events · ${health.observe_failures||0} observe failures`:'? unavailable'],['runtimes',runtimes?.configured?`● ${(runtimes.runtimes||[]).map(r=>`${r.runtime||'?'}:${r.events}`).join(' · ')}`:'? unavailable'],['detectors',detectors?.available?`● selected ${detectors.selected_digest||detectors.digest||'?'} · running ${detectors.runtime_governor?.digest||'?'}`:'? unavailable']]){const row=document.createElement('tr');cell(row,owner,true);cell(row,value);global.tBodies[0].appendChild(row);}box.appendChild(global);
  } catch {
    if(!box.isConnected)return;
    box.replaceChildren(el('div','evidence-state','Request failed'),el('div','sub','Capture diagnostics could not be loaded.'));
    const retry=el('button','','Retry diagnostics');retry.addEventListener('click',()=>{void renderSessionDiagnostics(ctx,box);});box.appendChild(retry);
  }
}
