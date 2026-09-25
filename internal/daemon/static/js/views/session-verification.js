// Verification facts pivot only boundary labels already claimed in records.
import { el } from '../core.js';
import { sessionEvidence, factTable, cell } from './session-evidence.js';

export async function renderSessionVerification(ctx,box) {
  try {
    const report=await sessionEvidence(ctx,'verify',{repository_limit:25,change_limit:1,verification_offset:0});if(!box.isConnected)return;
    box.replaceChildren();
    const repos=report.change?.repositories||[]; const checks=repos.flatMap(r=>r.verification||[]);
    if(!checks.length){box.appendChild(el('div','evidence-state','Verification witness not captured'));box.appendChild(el('div','sub','No verification record is linked to this session.'));return;}
    const boundaries=[...new Set(checks.map(v=>v.boundary||'?'))].sort(); const shown=boundaries.slice(0,6);
    const table=factTable('Verification evidence · boundary labels are claims',['check',...shown]);
    for(const name of [...new Set(checks.map(v=>v.name))].sort()){const row=document.createElement('tr');cell(row,name,true);
      for(const boundary of shown){const found=checks.filter(v=>v.name===name&&v.boundary===boundary).sort((a,b)=>b.source_id-a.source_id)[0];
        cell(row,found?(found.result_class==='observed'?'● ':'◐ ')+found.result:'?',false,found?`source #${found.source_id} · ${found.executable_identity||'executable unavailable'}`:'no record');}
      table.tBodies[0].appendChild(row);} box.appendChild(table); if(boundaries.length>shown.length)box.appendChild(el('div','evidence-boundary',`? ${boundaries.length-shown.length} boundary columns not shown`));
  } catch {
    if(!box.isConnected)return;
    box.replaceChildren(el('div','evidence-state','Request failed'),el('div','sub','Verification evidence could not be loaded.'));
    const retry=el('button','','Retry verification');retry.addEventListener('click',()=>{void renderSessionVerification(ctx,box);});box.appendChild(retry);
  }
}
