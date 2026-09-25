import assert from 'node:assert/strict';
import test from 'node:test';

import { atMentionToken, insertMention, matchingCommands } from './composer.js';

test('mention token is caret-relative and requires a whitespace boundary', () => {
  assert.deepEqual(atMentionToken('review @src/app', 15), { query: 'src/app', start: 7, end: 15 });
  assert.deepEqual(atMentionToken('@docs/a-b.md', 12), { query: 'docs/a-b.md', start: 0, end: 12 });
  assert.equal(atMentionToken('email@example', 13), null);
  assert.deepEqual(atMentionToken('use @src then', 8), { query: 'src', start: 4, end: 8 });
});

test('mention insertion replaces only the active token and preserves the tail', () => {
  const value = 'review @src old tail';
  const token = atMentionToken(value, 11);
  assert.equal(insertMention(value, token, 'src/app.js'), 'review src/app.js  old tail');
});

test('command matching is prefix-only and leaves domain actions injected', () => {
  const commands = [{ cmd: '/new' }, { cmd: '/note' }, { cmd: '/usage' }];
  assert.deepEqual(matchingCommands(commands, '/n').map(item => item.cmd), ['/new', '/note']);
  assert.deepEqual(matchingCommands(commands, 'hello'), []);
});
