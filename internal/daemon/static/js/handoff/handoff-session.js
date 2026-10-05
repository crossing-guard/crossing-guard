// What a session shows of a handoff (team rest-of-release plan §6.2, §14 Q22,
// Q23): "continues <title> from <sender>" on the second line of the header of
// a session that was opened for one, and the rows it adds under Related
// sessions — on the opened session, where it came from; on the session it was
// sent from, who continued it.
import { el } from '../core.js';
import { loadSessionHandoff, loadHandoffs } from './handoff-api.js';
import { continuesLine, relatedHandoffRows, nativeIdsOf } from './handoff-model.js';

// openedFacts asks whether this session was opened for a handoff. The route
// names a session by runtime and native id; a session's native id is its
// resume id on one runtime and its catalog id on another, so each is asked.
async function openedFacts(session) {
  for (const id of nativeIdsOf(session)) {
    const facts = await loadSessionHandoff(session.runtime, id);
    if (facts?.opened) return facts;
  }
  return null;
}

// mountContinues puts the continuation on the header's second line, before
// `before`. It opens the handoff. A session not opened for one shows nothing.
export async function mountContinues(line, session, before) {
  let facts = null;
  try { facts = await openedFacts(session); } catch { return; } // the header keeps its two facts
  if (!facts || !line.isConnected) return;
  const link = el('button', 'btn ghost session-continues', continuesLine(facts));
  link.type = 'button';
  link.onclick = () => document.dispatchEvent(new CustomEvent('cg:center', { detail: { view: 'handoff', id: facts.handoff_id } }));
  line.insertBefore(link, before?.parentNode === line ? before : null);
}

// loadRelatedHandoffs reads the rows a session's handoffs add under Related
// sessions. A failed read adds none.
export async function loadRelatedHandoffs(session, runtimeLabel) {
  const [facts, list] = await Promise.all([openedFacts(session).catch(() => null), loadHandoffs().catch(() => null)]);
  return relatedHandoffRows({ session, facts, list, runtimeLabel });
}
