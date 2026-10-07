// A session's project is a label; only its cwd is a path (session-view plan
// §A5). Claude's escaped folder name cannot be turned back into a directory, so
// a view that falls back from cwd to project would open or diff a path that
// does not exist. This pins every place that roots a file operation.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const read = path => readFileSync(join(here, path), 'utf8');

test('no view roots a file operation at the project label', () => {
  const offenders = [];
  for (const path of ['views/workspace-diff.js', 'views/retained-body.js', 'views/session-impact.js', 'views/session-change.js']) {
    const text = read(path);
    for (const match of text.matchAll(/cwd\s*\|\|\s*[\w?.]*\.project\b/g)) offenders.push(path + ': ' + match[0]);
  }
  assert.deepEqual(offenders, []);
});

test('rail labels split the cwd, never the project label', () => {
  assert.doesNotMatch(read('views/sessions.js'), /\.project\.split\('\/'\)/);
});
