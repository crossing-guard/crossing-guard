const STATES = Object.freeze({
  current: {
    label: 'Transcript index current',
    detail: 'Indexed transcripts match the latest completed local discovery pass.',
  },
  'catching-up': {
    label: 'Transcript index catching up',
    detail: 'Newer sessions are still being added, so results may be incomplete.',
  },
  stale: {
    label: 'Transcript index stale',
    detail: 'Some searchable transcripts are older than their local sources.',
  },
  incomplete: {
    label: 'Transcript index incomplete',
    detail: 'Some local sessions could not be added to the searchable transcript index.',
  },
  unavailable: {
    label: 'Transcript index unavailable',
    detail: 'The local searchable transcript index could not be read.',
  },
});

const LIMITATIONS = Object.freeze({
  'unsupported-adapter': 'a runtime has no transcript index adapter',
  'discovery-incomplete': 'local session discovery did not complete',
  'source-too-large': 'a transcript exceeds the local read limit',
  'source-unreadable': 'a local transcript could not be read',
  'source-mutated': 'a transcript changed while it was being indexed',
  'read-cancelled': 'an indexing pass was interrupted',
  'document-limit': 'a session exceeds the document limit',
  'text-limit': 'a session exceeds the indexed-text limit',
  'repository-busy': 'the local index is busy',
  'repository-unavailable': 'the local index store is unavailable',
  'invalid-projection': 'a transcript could not form a valid index projection',
});

const TRANSIENT = new Set(['source-mutated', 'read-cancelled', 'repository-busy']);
const BOUNDED = new Set(['source-too-large', 'document-limit', 'text-limit']);

function usableTimestamp(value) {
  const date = new Date(value || '');
  return !Number.isNaN(date.getTime()) && date.getUTCFullYear() > 1 ? date : null;
}

function defaultTime(value) {
  return value.toLocaleString();
}

function limitationKinds(coverage) {
  return [...new Set((Array.isArray(coverage?.limitations) ? coverage.limitations : [])
    .map(item => String(item?.kind || ''))
    .filter(kind => Object.hasOwn(LIMITATIONS, kind)))];
}

function recoveryFor(state, kinds) {
  if (kinds.includes('repository-unavailable')) {
    return 'Restore access to the local Crossing Guard data store; indexing will retry automatically.';
  }
  if (kinds.length > 0 && kinds.every(kind => TRANSIENT.has(kind))) {
    return 'Crossing Guard will retry automatically.';
  }
  if (kinds.some(kind => BOUNDED.has(kind))) {
    return 'Some transcripts are outside the fixed local indexing limits; older indexed copies, when available, are retained.';
  }
  if (state === 'catching-up') return 'Crossing Guard will continue indexing automatically.';
  if (state === 'stale' || state === 'incomplete') {
    return 'Crossing Guard will retry automatically. If this persists, run crossing-guard harvest --rebuild for a projection-only refresh.';
  }
  if (state === 'unavailable') return 'Restore local index access, then retry this view.';
  return '';
}

// The sole browser mapping from the closed transcript-index coverage model to prose.
// Consumers own DOM placement, never state interpretation.
export function presentTranscriptIndexCoverage(coverage, formatTime = defaultTime) {
  const requestedState = String(coverage?.state || '');
  const state = Object.hasOwn(STATES, requestedState) ? requestedState : 'unavailable';
  const kinds = limitationKinds(coverage);
  const asOf = usableTimestamp(coverage?.coverage_as_of);
  const indexed = Number.isFinite(coverage?.indexed_sessions) ? coverage.indexed_sessions : 0;
  const discovered = Number.isFinite(coverage?.discovered_sessions) ? coverage.discovered_sessions : 0;
  const timestamp = asOf ? formatTime(asOf) : '';
  const limitation = kinds.map(kind => LIMITATIONS[kind]).join('; ');
  const counts = discovered > 0 ? `${indexed} of ${discovered} discovered sessions indexed.` : '';
  return {
    state,
    label: STATES[state].label,
    detail: [STATES[state].detail, counts, limitation ? `Limited because ${limitation}.` : '']
      .filter(Boolean).join(' '),
    timestamp,
    timestampLabel: timestamp ? `Coverage as of ${timestamp}.` : 'Coverage time is not available.',
    recovery: recoveryFor(state, kinds),
    zeroAuthoritative: state === 'current' && Boolean(asOf),
    limitationKinds: kinds,
  };
}
