// Live turns: how many turns the console sent into a session since the chat was
// bound to it — turns the session's stored extract cannot hold yet. The chat
// counts them; every way of opening the send sheet for that session reads the
// same count, so the "turns not in this text" notice does not depend on which
// control opened it. A leaf: it imports nothing.

const counts = new Map();

const key = session => String(session?.runtime || '') + '/' + String(session?.id || '');

// countLiveTurn records one answered turn for a session. A session with no id
// (a chat not yet bound to one) counts nothing.
export function countLiveTurn(session) {
  if (!session?.id) return 0;
  const next = (counts.get(key(session)) || 0) + 1;
  counts.set(key(session), next);
  return next;
}

// liveTurnsOf is the count for one session; 0 when the chat sent it nothing.
export function liveTurnsOf(session) {
  return session?.id ? counts.get(key(session)) || 0 : 0;
}

// clearLiveTurns forgets a session's count: the chat let go of it.
export function clearLiveTurns(session) {
  counts.delete(key(session));
}
