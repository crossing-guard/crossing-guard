import { presentTranscriptIndexCoverage } from './transcript-index-coverage.js';

let failures = 0;
const check = (name, condition) => {
  if (!condition) { failures++; console.error('FAIL', name); }
};
const at = '2026-08-28T14:30:00Z';
const formattedAt = '2026-08-28T14:30:00.000Z';
const format = date => date.toISOString();

const states = {
  current: ['Transcript index current', true],
  'catching-up': ['Transcript index catching up', false],
  stale: ['Transcript index stale', false],
  incomplete: ['Transcript index incomplete', false],
  unavailable: ['Transcript index unavailable', false],
};
for (const [state, [label, authoritative]] of Object.entries(states)) {
  const value = presentTranscriptIndexCoverage({state, coverage_as_of: at}, format);
  check(`${state} label`, value.label === label);
  check(`${state} zero authority`, value.zeroAuthoritative === authoritative);
  check(`${state} timestamp`, value.timestamp === formattedAt && value.timestampLabel.includes(formattedAt));
}

const unknown = presentTranscriptIndexCoverage({state: 'future', coverage_as_of: at}, format);
check('unknown state fails closed', unknown.state === 'unavailable' && !unknown.zeroAuthoritative);
const missing = presentTranscriptIndexCoverage({}, format);
check('missing coverage fails closed without invented time', missing.state === 'unavailable'
  && !missing.zeroAuthoritative && missing.timestamp === '');
const zeroTime = presentTranscriptIndexCoverage({state: 'current', coverage_as_of: '0001-01-01T00:00:00Z'}, format);
check('zero timestamp cannot authorize zero', !zeroTime.zeroAuthoritative);

const limitations = {
  'unsupported-adapter': 'no transcript index adapter',
  'discovery-incomplete': 'discovery did not complete',
  'source-too-large': 'read limit',
  'source-unreadable': 'could not be read',
  'source-mutated': 'changed while',
  'read-cancelled': 'interrupted',
  'document-limit': 'document limit',
  'text-limit': 'indexed-text limit',
  'repository-busy': 'local index is busy',
  'repository-unavailable': 'store is unavailable',
  'invalid-projection': 'valid index projection',
};
for (const [kind, phrase] of Object.entries(limitations)) {
  const value = presentTranscriptIndexCoverage({state: 'incomplete', coverage_as_of: at,
    limitations: [{kind}]}, format);
  check(`${kind} is explained`, value.detail.includes(phrase));
  check(`${kind} is retained as typed value`, value.limitationKinds.length === 1
    && value.limitationKinds[0] === kind);
}

const transient = presentTranscriptIndexCoverage({state: 'incomplete', coverage_as_of: at,
  limitations: [{kind: 'repository-busy'}, {kind: 'source-mutated'}]}, format);
check('transient recovery waits for automatic retry', transient.recovery === 'Crossing Guard will retry automatically.');
const bounded = presentTranscriptIndexCoverage({state: 'incomplete', coverage_as_of: at,
  limitations: [{kind: 'text-limit'}]}, format);
check('bounded recovery is scope-neutral and does not suggest rebuild', bounded.recovery.includes('Some transcripts')
  && bounded.recovery.includes('fixed local indexing limits')
  && !bounded.recovery.includes('--rebuild'));
const unavailable = presentTranscriptIndexCoverage({state: 'unavailable', coverage_as_of: at,
  limitations: [{kind: 'repository-unavailable'}]}, format);
check('repository recovery restores access first', unavailable.recovery.startsWith('Restore access'));
const persistent = presentTranscriptIndexCoverage({state: 'stale', coverage_as_of: at,
  limitations: [{kind: 'source-unreadable'}]}, format);
check('persistent recovery offers projection-only force refresh', persistent.recovery.includes('harvest --rebuild')
  && persistent.recovery.includes('projection-only'));
const combined = presentTranscriptIndexCoverage({state: 'incomplete', coverage_as_of: at,
  discovered_sessions: 4, indexed_sessions: 2,
  limitations: [{kind: 'source-unreadable'}, {kind: 'source-unreadable'}, {kind: 'invalid-projection'}]}, format);
check('counts and distinct limitations render together', combined.detail.includes('2 of 4')
  && combined.limitationKinds.length === 2);

if (failures) process.exit(1);
console.log('transcript-index-coverage tests passed');
