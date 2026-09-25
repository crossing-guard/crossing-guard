import assert from 'node:assert/strict';
import { SessionActivityStore } from './session-activity-store.js';

let now = Date.parse('2026-08-28T18:00:00Z');
const store = new SessionActivityStore({ now: () => now });
const result = store.snapshot({
  schema_version: 1, generation: 3, observed_at: '2026-08-28T18:00:00Z',
  capability: { status: 'available', detail: 'qualified' },
  items: [{ runtime: 'claude', catalog_session_id: 'a', presence: 'open', execution: 'unknown',
    evidence: 'file_open', freshness: 'live', authority: 'observed', controllable: false,
    expires_at: '2026-08-28T18:00:45Z' }],
});
assert.equal(result.error, undefined);
assert.equal(store.cursor(), 3);
assert.equal(store.activity('claude', 'a').presence, 'open');
assert.equal(store.activity('codex', 'a'), null, 'runtime is part of exact identity');
now = Date.parse('2026-08-28T18:01:00Z');
assert.equal(store.activity('claude', 'a').freshness, 'stale');
assert.match(store.activity('claude', 'a').detail, /expired/);
assert.ok(store.snapshot({ schema_version: 2, generation: 4 }).error);

console.log('session-activity-store: all pass');
