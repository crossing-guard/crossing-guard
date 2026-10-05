// CSS token lints (session-view-and-console-preferences plan §C3).
//
// 1. Every var(--x) the console uses must be defined: in tokens.css, by the
//    generated appearance sheet (theme tokens, type steps, fonts, widths), or
//    at runtime by JS (setProperty('--x')). A use with no definition and no
//    fallback drops its whole declaration silently, which is how --surface and
//    --line went unnoticed; those fail. A use with a fallback only ever renders
//    the fallback, so it is counted and may not grow.
// 2. Font sizes are the appearance's type steps. A pixel font size outside
//    tokens.css would not follow the reader's text size, so the count is held
//    at zero.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const staticDir = join(here, '..');
const daemonDir = join(staticDir, '..');

function files(dir, suffix) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...files(path, suffix));
    else if (name.endsWith(suffix) && !name.includes('.test.')) out.push(path);
  }
  return out;
}

function definedNames() {
  const names = new Set();
  const tokens = readFileSync(join(staticDir, 'css', 'tokens.css'), 'utf8');
  for (const match of tokens.matchAll(/--([a-z0-9-]+)\s*:/g)) names.add(match[1]);
  // Emitted by /appearance.css (appearance.go): theme tokens, --chip-<runtime>-*,
  // type steps, fonts and widths. Theme token names come from the built-in theme.
  const dark = JSON.parse(readFileSync(join(daemonDir, 'themes', 'dark.json'), 'utf8'));
  for (const name of Object.keys(dark.tokens)) names.add(name);
  for (const name of ['ui-font', 'mono', 'measure', 'chat-measure']) names.add(name);
  for (let step = 1; step <= 15; step++) names.add('fs-' + step);
  for (const path of [...files(join(staticDir, 'js'), '.js'), ...files(join(staticDir, 'css'), '.css')]) {
    const text = readFileSync(path, 'utf8');
    for (const match of text.matchAll(/setProperty\(\s*['"`]--([a-z0-9-]+)/g)) names.add(match[1]);
    for (const match of text.matchAll(/['"`]--([a-z0-9-]+)['"`]\s*[,:]/g)) names.add(match[1]);
    // Custom properties a rule declares for its own subtree count as defined.
    if (path.endsWith('.css')) for (const match of text.matchAll(/[{;\s]--([a-z0-9-]+)\s*:/g)) names.add(match[1]);
  }
  return names;
}

function uses() {
  const out = [];
  for (const path of [...files(join(staticDir, 'css'), '.css'), ...files(join(staticDir, 'js'), '.js')]) {
    const text = readFileSync(path, 'utf8');
    for (const match of text.matchAll(/var\(\s*--([a-z0-9-]+)\s*(,)?/g)) out.push({ path, name: match[1], fallback: !!match[2] });
  }
  return out;
}

test('every CSS custom property used without a fallback is defined', () => {
  const defined = definedNames();
  const missing = uses().filter(use => !use.fallback && !defined.has(use.name)).map(use => use.path.replace(staticDir, '') + ': --' + use.name);
  assert.deepEqual([...new Set(missing)], []);
});

// Undefined-with-fallback renders only its fallback; the count ratchets down.
const UNDEFINED_WITH_FALLBACK_CEILING = 0;
test('undefined custom properties with a fallback do not grow', () => {
  const defined = definedNames();
  const withFallback = uses().filter(use => use.fallback && !defined.has(use.name)).map(use => use.path.replace(staticDir, '') + ': --' + use.name);
  assert.ok(withFallback.length <= UNDEFINED_WITH_FALLBACK_CEILING, 'undefined with fallback: ' + withFallback.join(', '));
});

test('no pixel font size outside the type steps', () => {
  const offenders = [];
  for (const path of [...files(join(staticDir, 'css'), '.css'), ...files(join(staticDir, 'js'), '.js')]) {
    if (path.endsWith('tokens.css')) continue;
    const text = readFileSync(path, 'utf8');
    for (const match of text.matchAll(/font-size:\s*[0-9.]+px|fontSize\s*=\s*['"][0-9.]+px|font:\s*(?:[0-9]{3}\s+)?[0-9.]+px/g)) {
      offenders.push(path.replace(staticDir, '') + ': ' + match[0]);
    }
  }
  assert.deepEqual(offenders, []);
});
