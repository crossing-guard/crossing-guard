// Governance › a clicked file, as the governor knows it (the Entity node of the
// DATA axis, made clickable). When you click a file in a Capture event row, the
// right panel answers "what is this, and who touched it?" from /api/govern/entity —
// the governor's OWN cross-session record, which is the honest thing to show on the
// data axis (not a filesystem stat, not a language guess).
//
// This is a separate selection from the session scope: S.governEntity holds the
// clicked entity id. The provider matches on it and outranks nothing — it is shown
// because updateInfo passes it as the selection when it is set.
import { el, api, shortWhen } from "../core.js";
import { provider } from "../infopanel.js";
import { S } from "../state.js";
import { setGovernSession } from "./governance-ui.js";

// openEntity is what a clickable file target calls. It records the entity and asks
// the info panel to repaint; a Close button (below) clears it again.
export function openEntity(entityId) {
  S.governEntity = entityId;
  document.dispatchEvent(new CustomEvent('cg:info'));
}
// closeEntity is only used by the Close button below — not exported, nothing outside
// this module clears the entity view.
function closeEntity() {
  S.governEntity = null;
  document.dispatchEvent(new CustomEvent('cg:info'));
}

// human turns file:/a/b/c.go into a/b/c.go — the id scheme is ours, the reader's
// question is about the path.
function human(id) {
  if (!id) return '';
  for (const p of ['file:', 'url:', 'mcp:', 'db:']) if (id.startsWith(p)) return id.slice(p.length);
  return id;
}

// fetchEntity memoises the LAST entity id's in-flight/settled fetch, so the two
// providers below (header + touches) share ONE request per click instead of each
// hitting /api/govern/entity independently. A new id replaces the memo.
let entityMemo = { id: null, promise: null };
function fetchEntity(id) {
  if (entityMemo.id !== id) {
    entityMemo = { id, promise: api('/api/govern/entity?id=' + encodeURIComponent(id)) };
  }
  return entityMemo.promise;
}

provider({
  id: 'governance.entity', order: 0, zone: 'header',
  title: 'File',
  // Matches only when a file/entity has been clicked. updateInfo passes it as the
  // selection, so this outranks the session providers for as long as it is set.
  match: ctx => ctx.surface === 'governance-entity' && !!ctx.selection,
  render: async (ctx, box) => {
    const id = ctx.selection;
    const bar = el('div', 'refactions');
    const back = el('button', 'btn', '✕ Close');
    back.title = 'Stop showing this file and return to the session';
    back.onclick = () => closeEntity();
    bar.appendChild(back);
    box.appendChild(bar);

    box.appendChild(el('div', 'refrow')).append(
      el('span', 'refrow-k', 'path'),
      (() => { const v = el('span', 'refrow-v', human(id)); v.title = id; return v; })());

    let rep;
    try {
      rep = await fetchEntity(id);
    } catch (e) {
      box.appendChild(el('div', 'sub', '✖ could not read this file’s record: ' + (e.message || e)));
      return;
    }
    if (box.isConnected === false) return;

    if (!rep.found) {
      box.appendChild(el('div', 'sub', rep.note || 'The governor holds no record of this '
        + 'file — no hooked tool call has touched it, or it was touched before capture began.'));
      return;
    }

    // Folded facts about the file itself (resource-scoped state), each with the
    // detector that asserted it — provenance never dropped (INV-22).
    if ((rep.state || []).length) {
      box.appendChild(el('div', 'infolbl', 'what we believe about this file'));
      for (const s of rep.state) {
        const c = el('span', 'chip cl-observed', s.key + '=' + s.value);
        c.title = s.detector + (s.evidence ? ' · ' + s.evidence : '');
        c.style.marginRight = '4px';
        box.appendChild(c);
      }
    }

    if (rep.truncated) {
      box.appendChild(el('div', 'sub', 'The answer hit its row cap — this is a PREFIX of '
        + 'the sessions that touched this file, not all of them.'));
    }
  },
});

// Backlinks-style list: which sessions touched this file. Separate primary provider
// so it scrolls under the header. Shares the header's fetch via fetchEntity (one
// request per click), and stays independent otherwise.
provider({
  id: 'governance.entity.touches', order: 10, zone: 'primary',
  title: 'Sessions that touched it',
  match: ctx => ctx.surface === 'governance-entity' && !!ctx.selection,
  render: async (ctx, box) => {
    const id = ctx.selection;
    let rep;
    try {
      rep = await fetchEntity(id);
    } catch (e) {
      box.appendChild(el('div', 'sub', 'unavailable — ' + (e.message || e)));
      return;
    }
    if (box.isConnected === false) return;
    const touches = (rep && rep.touches) || [];
    box.appendChild(el('div', 'infolbl', touches.length + (touches.length === 1 ? ' session' : ' sessions')));
    if (!touches.length) {
      box.appendChild(el('div', 'sub', rep && rep.found
        ? 'No session touch is recorded for this file.'
        : 'The governor holds no record of this file.'));
      return;
    }
    for (const t of touches.slice(0, 12)) {
      const line = el('div', 'refback');
      // A button, not a bare span: clicking it scopes the surface, so it must be
      // keyboard-operable (Enter/Space), not mouse-only.
      const who = el('span', 'refback-p', t.title || t.session_id || '');
      if (!t.title) who.classList.add('t-unnamed');
      who.title = 'Scope the tabs to this session (' + (t.session_id || 'unknown') + ')';
      who.style.cursor = 'pointer';
      who.setAttribute('role', 'button');
      who.tabIndex = 0;
      // Hand back to the session axis: clear the file view, scope the surface to this
      // session. tags:[] on purpose — the touch list carries no folded state, and
      // Capture fills the tags in on select(); a fabricated tag set would be a lie,
      // so Policy simply shows nothing "in scope" until Capture supplies the real set.
      const scope = () => {
        S.governEntity = null;
        setGovernSession({ id: t.session_id, runtime: t.runtime, title: t.title, tags: [] });
      };
      who.onclick = scope;
      who.onkeydown = ev => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); scope(); } };
      line.appendChild(who);
      line.appendChild(el('span', 'refback-t', [(t.verbs || []).join('/'),
        t.events ? t.events + ' events' : '',
        t.last_seen ? shortWhen(new Date(t.last_seen * 1000).toISOString()) : ''].filter(Boolean).join(' · ')));
      box.appendChild(line);
    }
    if (touches.length > 12) box.appendChild(el('div', 'sub', '+' + (touches.length - 12) + ' more'));
  },
});
