import test from 'node:test';
import assert from 'node:assert/strict';
import { changeEvidenceNote } from './change-evidence-note.js';

test('no problem, no sentence', () => {
  for (const problem of [undefined, null, '', '   ']) assert.equal(changeEvidenceNote(problem), '');
});

test('a problem is stated with its reason, verbatim', () => {
  const reason = 'store schema is v99 but this crossing-guard understands only v39 — upgrade the binary';
  assert.equal(changeEvidenceNote(reason),
    'Sessions known only from recorded change evidence are not listed: ' + reason);
});
