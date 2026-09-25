// Session › what governance knows about it — the bridge between the two surfaces.
//
// The gap this closes: reading a session, there was NO path to its governance record
// at all. You clicked the Governance nav, landed on Capture, and hunted your session
// among a hundred rows. Meanwhile Capture's centre is itself a session view with no
// way back. Two surfaces about the same object, no thread between them.
//
// Most of the time the honest answer is small — "we captured 462 events, 15 facts,
// nothing was held" — and that belongs in the right panel, which console-design §5
// defines as read-only information that INFORMS the centre. So the glance costs no
// navigation at all; only the deep dive switches surface, and it carries the session
// with it (cg:govern-open) so you land on it rather than looking for it.
//
// Honesty: Capture holds only the sessions the governor observed live, which is a
// strict subset of the harvested sessions this panel appears on. When there is no
// record, that is a REAL answer ("not captured"), not an empty box — the
// /api/govern/session envelope carries `found:false` with a reason precisely so this
// distinction survives (see govern_serve.go).
import { el } from "../core.js";

// openInGovernance hands the session to the Governance surface. Routed over an event
// rather than an import so this module does not depend on governance.js (which pulls
// in every tab, one of which imports sessions.js — that would be a cycle).
export function openInGovernance(sess) {
  document.dispatchEvent(new CustomEvent('cg:govern-open', {
    detail: { tab: 'capture', session: sess },
  }));
}

export function governanceLink(selection, summary={}) {
  const go = el('button', 'evidence-footer-link', 'Governance →');
  go.title = 'Open Governance scoped to this session';
  go.onclick = () => openInGovernance({
    id: summary.id || selection.thread_id || selection.id,
    runtime: selection.runtime,
    title: selection.title,
    title_source: selection.title_source,
    tags: [],
  });
  return go;
}
