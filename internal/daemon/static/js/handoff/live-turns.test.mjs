import test from 'node:test';
import assert from 'node:assert/strict';
import { countLiveTurn, liveTurnsOf, clearLiveTurns } from './live-turns.js';

// Red-team Low 14: "Hand off…" in a session's menu passed no live-turn count, so
// the notice never showed there. Both ways in now read one count, per session.
test('a session’s live turns are counted per session and read by any caller', () => {
  const one = { runtime: 'fixture-runtime', id: 'catalog-1' }, other = { runtime: 'fixture-runtime', id: 'catalog-2' };
  assert.equal(liveTurnsOf(one), 0);
  countLiveTurn(one);
  assert.equal(countLiveTurn(one), 2);
  assert.deepEqual([liveTurnsOf(one), liveTurnsOf(other), liveTurnsOf({ runtime: 'another-runtime', id: 'catalog-1' })], [2, 0, 0]);
  assert.equal(countLiveTurn({ runtime: 'fixture-runtime', id: '' }), 0, 'a chat bound to no session counts nothing');
  assert.equal(liveTurnsOf(null), 0);
  clearLiveTurns(one);
  assert.equal(liveTurnsOf(one), 0);
});
