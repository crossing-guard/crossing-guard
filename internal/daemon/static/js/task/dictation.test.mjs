import assert from 'node:assert/strict';
import { test } from 'node:test';

globalThis.localStorage ??= { getItem: () => '', setItem() {}, removeItem() {} };
const { adjustOffset, parseBinding, matchesBinding, holdOrTap, autoSendAllowed, splitTail, formatClock } = await import('./dictation.js');

test('insertion offset follows edits before it, ignores edits after it, and dies when spanned', () => {
  assert.equal(adjustOffset(10, 'hello world', 'hello big world'), 14);   // insert before
  assert.equal(adjustOffset(10, 'hello world', 'hello world!!'), 10);     // append after
  assert.equal(adjustOffset(10, 'hello world', 'hllo world'), 9);         // delete before
  assert.equal(adjustOffset(5, 'abcdefghij', 'abXYij'), null);            // edit spans the point
  assert.equal(adjustOffset(null, 'a', 'ab'), null);
  assert.equal(adjustOffset(0, '', 'typed'), 0);
});

test('the push-to-talk binding parses and matches modifier keys exactly', () => {
  const binding = parseBinding('Alt+Space');
  assert.deepEqual(binding, { alt: true, ctrl: false, meta: false, shift: false, key: 'space' });
  assert.equal(matchesBinding({ code: 'Space', altKey: true }, binding), true);
  assert.equal(matchesBinding({ code: 'Space', altKey: false }, binding), false);
  assert.equal(matchesBinding({ code: 'Space', altKey: true, shiftKey: true }, binding), false);
  const meta = parseBinding('meta+k');
  assert.equal(matchesBinding({ code: 'KeyK', metaKey: true }, meta), true);
});

test('hold versus tap is decided by the configured threshold, never a compiled one', () => {
  assert.equal(holdOrTap(299, 300), 'tap');
  assert.equal(holdOrTap(300, 300), 'hold');
});

test('auto-send needs the setting, an empty starting draft, and enough words', () => {
  const ui = { auto_send: true, auto_send_min_words: 3 };
  assert.equal(autoSendAllowed(ui, true, 'run the gate'), true);
  assert.equal(autoSendAllowed(ui, true, 'run it'), false);
  assert.equal(autoSendAllowed(ui, false, 'run the gate now'), false);
  assert.equal(autoSendAllowed({ auto_send: false }, true, 'run the gate now'), false);
});

test('the dim tail is the last configured number of provisional words', () => {
  assert.deepEqual(splitTail('rebase the', 'branch onto main', 2), { stable: 'rebase the branch', dim: 'onto main' });
  assert.deepEqual(splitTail('', 'one', 2), { stable: '', dim: 'one' });
  assert.deepEqual(splitTail('done', '', 2), { stable: 'done', dim: '' });
});

test('the clock renders minutes and zero-padded seconds', () => {
  assert.equal(formatClock(0), '0:00');
  assert.equal(formatClock(11400), '0:11');
  assert.equal(formatClock(120000), '2:00');
});
