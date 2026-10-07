// Self-test for vendor-lint-js.mjs: pins what the scanner sees and skips, so
// the gate cannot quietly share a blind spot with the code it guards.
import assert from 'node:assert/strict';
import test from 'node:test';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { execFileSync } from 'node:child_process';
import { measure, stripComments, compare, registrationIDs, registeredRuntimeIDs } from './vendor-lint-js.mjs';

test('comments are stripped, strings are kept', () => {
  assert.equal(measure("// opus in a comment\nconst a = 1;", 'js').tokens, 0);
  assert.equal(measure("/* codex\n claude */ const a = 1;", 'js').tokens, 0);
  assert.equal(measure("const url = 'http://ollama.local/x';", 'js').tokens, 1, '"//" inside a string is code');
  assert.equal(measure("const label = 'opus-class rates';", 'js').tokens, 1, 'model-family names count');
  assert.equal(stripComments("a /* x */ b", 'js'), 'a   b');
});

test('css and html are scanned with their own comment forms', () => {
  assert.equal(measure('.chip.claude { color: red; } /* codex */', 'css').tokens, 1);
  assert.equal(measure('<!-- opus --><span class="gpt"></span>', 'html').tokens, 1);
});

test('runtime branches are counted in any spelling', () => {
  assert.equal(measure("if (runtime === 'codex') x();", 'js').branches, 1);
  assert.equal(measure('if ("claude" == rt) x();', 'js').branches, 1);
  assert.equal(measure("switch (rt) { case 'opencode': break; }", 'js').branches, 1);
  assert.equal(measure("const chip = 'chip ' + runtime;", 'js').branches, 0);
});

test('a file may only go down, and a new file must be clean', () => {
  const baseline = { 'a.js': { tokens: 2, branches: 1 } };
  assert.deepEqual(compare({ 'a.js': { tokens: 2, branches: 1 } }, baseline), []);
  assert.equal(compare({ 'a.js': { tokens: 3, branches: 1 } }, baseline).length, 1);
  assert.equal(compare({ 'b.js': { tokens: 1, branches: 0 } }, baseline).length, 1);
});

test('registration inventory follows literal keys and independent Name implementations', () => {
  const source = `
    // registerChatDriver("comment", ignored{})
    /* registerIdentity("hidden", ignored{}) */
    registerChatDriver("future-chat", driver{})
    registerIdentity("future-caller", identity{})
    register(reader{})
    const runtimeID string = "future-reader"
    func (reader) Name() string { return runtimeID }
    registerSkillsProvider(&skills{})
    func (*skills) Name() string { return "future-skills" }
    registerHookInstaller(hook{})
    func (hook) Name() string { return "future-hook" }
  `;
  assert.deepEqual(registrationIDs(source), ['future-caller', 'future-chat', 'future-hook', 'future-reader', 'future-skills']);
  assert.deepEqual(registrationIDs(source, false), ['future-caller', 'future-chat', 'future-reader', 'future-skills']);
});

test('new adapters need no lint list edit; shared Go and browser leaks fail', async t => {
  const root = mkdtempSync(join(tmpdir(), 'crossing-guard-provider-lint-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  for (const dir of ['engine', 'store', 'harvest', 'memory', 'internal', 'cmd', 'ruledoc', 'schemas', 'teamwire', 'scripts']) mkdirSync(join(root, dir));
  for (const script of ['vendor-lint.sh', 'vendor-lint-js.mjs']) copyFileSync(new URL(script, import.meta.url), join(root, 'scripts', script));
  writeFileSync(join(root, 'harvest', 'future_1_2_3.go'), `package harvest
    func init() { register(futureReader{}) }
    func (futureReader) Name() string { return "future" }
  `);
  writeFileSync(join(root, 'internal', 'hook_provider.go'), `package internal
    func init() { registerHookInstaller(hookReader{}) }
    func (hookReader) Name() string { return "hook-provider" }
  `);
  writeFileSync(join(root, 'internal', 'ignored_test.go'), `package internal
    func init() { registerChatDriver("test-only", fixture{}) }
  `);
  assert.deepEqual(registeredRuntimeIDs(root), ['future', 'hook-provider']);
  assert.deepEqual(registeredRuntimeIDs(root, false), ['future']);
  const scanner = await import(pathToFileURL(join(root, 'scripts', 'vendor-lint-js.mjs')).href);
  assert.deepEqual(scanner.measure("if (runtime === 'future') {}", 'js'), { tokens: 1, branches: 1 });
  assert.equal(scanner.measure("if (runtime === 'hook-provider') {}", 'js').branches, 1);
  assert.equal(scanner.compare({ 'shared.js': scanner.measure("const name = 'future';", 'js') }, {}).length, 1);
  const run = () => execFileSync('bash', [join(root, 'scripts', 'vendor-lint.sh'), '0'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  assert.match(run(), /VENDOR-LINT PASS/, 'new versioned adapter source is allowed');
  writeFileSync(join(root, 'internal', 'shared.go'), 'package internal\nvar runtime = "future"\n');
  assert.throws(run, error => error.status === 1 && /VENDOR-LINT FAIL/.test(error.stdout), 'a planted generic Go leak must fail');
  writeFileSync(join(root, 'internal', 'shared.go'), 'package internal\nvar runtime = "neutral"\n');
  assert.match(run(), /VENDOR-LINT PASS/, 'removing the generic leak recovers');
});
