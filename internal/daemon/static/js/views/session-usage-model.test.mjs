import assert from 'node:assert/strict';
import test from 'node:test';

import { activeSpan, depthLabel, gapSegments, groupByRole, measureOf, memberTree, splitShares, statedNote, sumKinds, timeScale } from './session-usage-model.js';

const kind = { calls: 3, input_tokens: 10, cache_read: 80, cache_create: 5, output_tokens: 20, total: 115, reasoning_tokens: null };

test('a measure reads the totals the route states, and unknown reasoning stays unknown', () => {
  assert.equal(measureOf(kind, 'all'), 115);
  assert.equal(measureOf(kind, 'input'), 95);
  assert.equal(measureOf(kind, 'output'), 20);
  assert.equal(measureOf(kind, 'calls'), 3);
  assert.equal(measureOf(kind, 'reasoning'), null);
  assert.equal(measureOf(null, 'all'), 0);
});

test('shares skip empty kinds and sum to one', () => {
  const shares = splitShares({ main: 30, subagent: 70, agent: 0 });
  assert.deepEqual(shares.map(share => share.kind), ['main', 'subagent']);
  assert.equal(shares.reduce((sum, share) => sum + share.share, 0), 1);
  assert.deepEqual(splitShares({ main: null, subagent: 0, agent: 0 }), []);
});

test('a partial class says how many calls stated it, a whole one says nothing', () => {
  assert.equal(statedNote(3, 73), 'stated by 3 of 73');
  assert.equal(statedNote(5, 5), '');
});

test('idle gaps longer than the threshold split the timeline, and the scale keeps order', () => {
  const minute = 60000;
  const segments = gapSegments([10 * minute, 0, 5 * minute, 100 * minute, 101 * minute], 20 * minute);
  assert.deepEqual(segments, [[0, 10 * minute], [100 * minute, 101 * minute]]);
  const scale = timeScale(segments, 100, 400, 12);
  assert.equal(scale.at(0), 100);
  assert.equal(scale.breaks.length, 1);
  assert.ok(scale.at(5 * minute) < scale.at(100 * minute));
  assert.ok(scale.at(101 * minute) <= 500 + 0.001);
  assert.deepEqual(gapSegments([], minute), []);
  const many = gapSegments(Array.from({ length: 60 }, (_, index) => index * 60 * minute), 20 * minute);
  const wide = timeScale(many, 0, 400, 12);
  assert.ok(wide.at(59 * 60 * minute) - wide.at(0) >= 400 * 0.7 - 1, 'gap marks never take more than 30% of the plot');
});

test('members nest under a stated parent that is itself a member; the rest are top level', () => {
  const rows = memberTree([
    { id: 'digest', first_at: '2026-09-25T15:00:00Z' },
    { id: 'reader-b', parent: 'digest', first_at: '2026-09-25T15:20:00Z' },
    { id: 'reader-a', parent: 'digest', first_at: '2026-09-25T15:10:00Z' },
    { id: 'orphan', parent: 'gone', first_at: '2026-09-25T14:00:00Z' },
    { id: 'loop', parent: 'loop', first_at: '2026-09-25T16:00:00Z' },
  ]);
  assert.deepEqual(rows.map(row => `${row.level}:${row.member.id}`), ['0:orphan', '0:digest', '1:reader-a', '1:reader-b', '0:loop']);
  assert.equal(rows.find(row => row.member.id === 'digest').children, 2);
});

test('grouping by role sums members and names an empty role', () => {
  const groups = groupByRole([
    { role: 'Explore', calls: 1, total: 10, output_tokens: 1 },
    { role: '', calls: 2, total: 50, output_tokens: 4, reasoning_tokens: 3 },
    { role: 'Explore', calls: 3, total: 30, output_tokens: 2 },
  ]);
  assert.deepEqual(groups.map(group => `${group.role}:${group.count}:${group.totals.total}`), ['no role stated:1:50', 'Explore:2:40']);
  assert.equal(groups[0].totals.reasoning_tokens, 3);
  assert.equal(groups[1].totals.reasoning_tokens, null);
  assert.equal(sumKinds([]).calls, 0);
});

test('an active span needs both ends', () => {
  assert.equal(activeSpan({ first_at: '2026-09-25T15:00:00Z', last_at: '2026-09-25T15:30:00Z' }), 1800000);
  assert.equal(activeSpan({ first_at: '' }), null);
});

test('a depth is shown as stated, never guessed', () => {
  assert.equal(depthLabel({ depth: 2 }), '2');
  assert.equal(depthLabel({ depth: null }), 'not stated');
  assert.equal(depthLabel({}), 'not stated');
});
