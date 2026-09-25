// Info-panel providers for a clicked reference — console-and-info-panel §4/§7,
// impl-plan Steps 1.4 and 1.5.
//
// Two providers on the existing host, both READ-ONLY (INV): a context header
// stating what the reference resolved to and where it sits, and backlinks —
// "what else points here" — merging doc references with cross-session touches
// from the governor's own event log (§10: that last part is free for us and
// structurally unavailable to git-history tools).
//
// Nothing here computes a descriptor, runs an analyzer, or names a language:
// that is Track 3, deliberately not this.
import { el, fmtTime } from '../core.js';
import { provider } from '../infopanel.js';
import * as refs from '../refs.js';

const isRef = ctx => ctx.surface === 'ref' && !!ctx.selection;

// Context header — the "place summary" the panel always shows for a reference
// (§4 zone 1). It reports only what the reference index measured; the five-axis
// dossier (role, call graph, churn) is Track 3 and is NOT implied here.
provider({
  id: 'ref.context',
  order: 0,
  zone: 'header', // what this reference IS never scrolls away (§4 zone 1)
  title: ctx => ctx.selection?.kind === 'id' ? 'Reference · item' : 'Reference',
  match: isRef,
  render: async (ctx, body) => {
    const t = ctx.selection;
    // A way OUT, and an explicit way to hand off. Before this the panel was a
    // one-way door: a reference stayed selected until you changed surface, and
    // the editor opened itself whether you wanted it or not.
    const bar = el('div', 'refactions');
    const back = el('button', 'btn', '✕ Close');
    back.title = 'Stop showing this reference and return to the session';
    back.onclick = () => document.dispatchEvent(new CustomEvent('cg:close-ref'));
    bar.appendChild(back);
    if (t.path && t.kind !== 'id') {
      const ed = el('button', 'btn', 'Open in editor ↗');
      ed.title = 'Open ' + t.path + (t.line ? ':' + t.line : '') + ' in your editor';
      ed.onclick = () => document.dispatchEvent(new CustomEvent('cg:open-editor', { detail: t }));
      bar.appendChild(ed);
    }
    body.appendChild(bar);
    body.appendChild(row('token', t.token || t.path || ''));
    if (t.path) body.appendChild(row('path', t.path + (t.line ? ':' + t.line : '')));
    if (t.kind) body.appendChild(row('kind', t.kind));
    const area = (t.path || '').split('/')[0];
    if (area && (t.path || '').includes('/')) body.appendChild(row('area', area));

    // Ask the server for the authoritative state: only it can tell "moved" from
    // "never existed" (that answer comes from git, not the manifest).
    try {
      const server = await refs.resolveOnServer(t.token || t.path, t.root);
      body.appendChild(row('state', server.state + (server.reason ? ' — ' + server.reason : '')));
      if (server.defs && server.defs.length > 1) {
        const d = el('div', 'sub', `defined in ${server.defs.length} places:`);
        body.appendChild(d);
        for (const def of server.defs) body.appendChild(row('', def.path + ':' + def.line));
      }
      if (server.subjects && server.subjects.length) {
        body.appendChild(el('div', 'sub', 'subject files:'));
        for (const s of server.subjects) body.appendChild(row('', s));
      }
    } catch (e) {
      // Say the check failed. Silence here would read as "resolved".
      body.appendChild(row('state', 'could not verify — ' + (e.message || e)));
    }
  },
});

// Backlinks — docs that point here, plus sessions that touched it.
provider({
  id: 'refs.backlinks',
  order: 10,
  zone: 'primary',
  title: 'Referenced by',
  match: isRef,
  render: async (ctx, body) => {
    const t = ctx.selection;
    const target = t.path || t.token;
    if (!target) { body.appendChild(el('div', 'sub', 'nothing to look up')); return; }
    let rep;
    try {
      rep = await refs.backlinks(target, t.root);
    } catch (e) {
      body.appendChild(el('div', 'sub', 'backlinks unavailable — ' + (e.message || e)));
      return;
    }
    // Docs
    const docs = rep.docs || [];
    body.appendChild(el('div', 'infolbl', `docs · ${docs.length}${rep.docs_truncated ? '+' : ''}`));
    if (!docs.length) body.appendChild(el('div', 'sub', 'no doc references this'));
    for (const d of docs.slice(0, 12)) {
      const line = el('div', 'refback');
      line.appendChild(el('span', 'refback-p', d.path + ':' + d.line));
      line.appendChild(el('span', 'refback-t', trim(d.text)));
      body.appendChild(line);
    }
    if (docs.length > 12) body.appendChild(el('div', 'sub', `+${docs.length - 12} more`));

    // Sessions — the cross-session half (§10).
    const sess = rep.sessions || [];
    body.appendChild(el('div', 'infolbl', `sessions · ${sess.length}`));
    if (!sess.length) body.appendChild(el('div', 'sub', 'no session has touched this'));
    for (const s of sess.slice(0, 8)) {
      const line = el('div', 'refback');
      line.appendChild(el('span', 'refback-p', s.title || s.session_id || ''));
      const when = s.last_seen ? fmtTime(new Date(s.last_seen * 1000).toISOString()) : '';
      line.appendChild(el('span', 'refback-t',
        [(s.verbs || []).join('/'), s.events ? s.events + ' events' : '', when].filter(Boolean).join(' · ')));
      body.appendChild(line);
    }
    if (sess.length > 8) body.appendChild(el('div', 'sub', `+${sess.length - 8} more`));

    // Any source that could not answer says so — an empty list and an
    // unavailable source look identical otherwise, and only one is the truth.
    for (const n of rep.notes || []) body.appendChild(el('div', 'sub', '⚠ ' + n));
  },
});

function row(label, value) {
  const d = el('div', 'refrow');
  if (label) d.appendChild(el('span', 'refrow-k', label));
  const v = el('span', 'refrow-v', value);
  v.title = String(value); // ellipsised in the rail; complete on hover
  d.appendChild(v);
  return d;
}

const trim = s => (s || '').length > 90 ? (s || '').slice(0, 90) + '…' : (s || '');
