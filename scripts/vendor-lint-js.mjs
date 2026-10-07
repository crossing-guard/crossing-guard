#!/usr/bin/env node
// Open/closed gate for browser code (ADR 0020; runtime-model-catalog-and-usage
// plan §2.5). The console is framework code: vendor, runtime and model-family
// names belong in adapters and in what the daemon publishes, not in JS, CSS or
// HTML. The Go pass (vendor-lint.sh) cannot see this code, and its mechanics
// would miss the defects this pass exists for: model-family names ("opus-class"
// prices), block comments, "//" inside strings, CSS chips, and runtime branches.
//
// Two checks:
//   tokens    per-file counts of vendor/model-family tokens on code (comments
//             stripped). Each file may only go down against the baseline; a new
//             file with tokens fails.
//   branches  code that branches on a runtime id (=== 'codex', case 'claude').
//             Per-file counts that may only go down; target zero.
// `--update` rewrites the baseline from the current tree (after a reduction).

import { readFileSync, writeFileSync, readdirSync, statSync, existsSync } from 'node:fs';
import { join, relative, extname } from 'node:path';
import { fileURLToPath } from 'node:url';

// Provider membership comes from the existing production registrations. A new
// adapter must not also require an edit to the checks guarding its boundaries.
export function registrationIDs(source, includeHooks = true) {
  const code = stripComments(source, 'js');
  const ids = new Set();
  for (const match of code.matchAll(/\bregister(?:ChatDriver|Identity)\(\s*"([a-z][a-z0-9_-]*)"/g)) ids.add(match[1]);
  const typedRegistration = includeHooks ? 'register|registerHookInstaller|registerSkillsProvider' : 'register|registerSkillsProvider';
  for (const match of code.matchAll(new RegExp(`\\b(?:${typedRegistration})\\(\\s*&?([A-Za-z][A-Za-z0-9_]*)\\{\\s*\\}\\s*\\)`, 'g'))) {
    const name = new RegExp(`\\bfunc\\s*\\([^)]*\\b${match[1]}\\b[^)]*\\)\\s*Name\\(\\)\\s+string\\s*\\{\\s*return\\s+(?:"([a-z][a-z0-9_-]*)"|([A-Za-z][A-Za-z0-9_]*))\\s*;?\\s*\\}`);
    const method = code.match(name);
    if (method?.[1]) ids.add(method[1]);
    else if (method?.[2]) {
      const constant = code.match(new RegExp(`\\bconst\\s+${method[2]}(?:\\s+string)?\\s*=\\s*"([a-z][a-z0-9_-]*)"`));
      if (constant) ids.add(constant[1]);
    }
  }
  return [...ids].sort();
}

export function registeredRuntimeIDs(root, includeHooks = true) {
  const ids = new Set();
  for (const dir of ['harvest', 'internal']) {
    const path = join(root, dir);
    if (!existsSync(path)) continue;
    for (const file of walk(path, [], /\.go$/).filter(file => !file.endsWith('_test.go'))) {
      for (const id of registrationIDs(readFileSync(file, 'utf8'), includeHooks)) ids.add(id);
    }
  }
  return [...ids].sort();
}

const root = join(fileURLToPath(new URL('.', import.meta.url)), '..');
export const RUNTIME_NAMES = registeredRuntimeIDs(root);
// Hook-only identities can also be ordinary UI words (for example, a pointer
// cursor). Their runtime branches are checked, but bare-word counts keep the
// existing CLI/session-runtime scope rather than treating CSS as vendor logic.
export const TOKEN_RUNTIME_NAMES = registeredRuntimeIDs(root, false);
export const TOKEN = new RegExp('(' + [...new Set([...TOKEN_RUNTIME_NAMES,
  'claude', 'codex', 'opencode', 'openai', 'anthropic', 'ollama', 'gemini', 'opus', 'sonnet', 'haiku', 'gpt', 'qwen', 'models\\.dev'])].join('|') + ')', 'gi');
const RUNTIME_IDS = '(' + [...new Set([...RUNTIME_NAMES, 'claude', 'codex', 'opencode', 'cursor', 'openai'])].join('|') + ')';
export const BRANCH = new RegExp(
  `(?:===|!==|==|!=)\\s*(['"\`])${RUNTIME_IDS}\\1|(['"\`])${RUNTIME_IDS}\\3\\s*(?:===|!==|==|!=)|\\bcase\\s*(['"\`])${RUNTIME_IDS}\\5`, 'gi');

// stripComments removes comments and keeps strings, so "http://x" stays code.
// It is a scanner, not a parser: regex literals containing quotes are rare in
// this codebase and would only make a count higher, never hide one.
export function stripComments(source, kind) {
  let out = '';
  let i = 0;
  const lineComments = kind === 'js';
  while (i < source.length) {
    const c = source[i], next = source[i + 1];
    if (kind === 'html' && source.startsWith('<!--', i)) {
      const end = source.indexOf('-->', i + 4);
      i = end < 0 ? source.length : end + 3;
      continue;
    }
    if (c === '/' && next === '*') {
      const end = source.indexOf('*/', i + 2);
      i = end < 0 ? source.length : end + 2;
      out += ' ';
      continue;
    }
    if (lineComments && c === '/' && next === '/') {
      const end = source.indexOf('\n', i);
      i = end < 0 ? source.length : end;
      continue;
    }
    if (kind === 'js' && (c === '"' || c === "'" || c === '`')) {
      let j = i + 1;
      while (j < source.length && source[j] !== c) {
        if (source[j] === '\\') j++;
        else if (c !== '`' && source[j] === '\n') break;
        j++;
      }
      out += source.slice(i, j + 1);
      i = j + 1;
      continue;
    }
    out += c;
    i++;
  }
  return out;
}

export function kindOf(path) {
  const ext = extname(path);
  if (ext === '.css') return 'css';
  if (ext === '.html') return 'html';
  return 'js';
}

export function measure(source, kind) {
  const code = stripComments(source, kind);
  return { tokens: (code.match(TOKEN) || []).length, branches: kind === 'js' ? (code.match(BRANCH) || []).length : 0 };
}

function walk(dir, files = [], accept = /\.(js|mjs|css|html)$/) {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) walk(path, files, accept);
    else if (accept.test(name)) files.push(path);
  }
  return files;
}

export function measureTree(root, staticDir) {
  const result = {};
  for (const path of walk(join(root, staticDir)).sort()) {
    const counts = measure(readFileSync(path, 'utf8'), kindOf(path));
    if (counts.tokens || counts.branches) result[relative(root, path)] = counts;
  }
  return result;
}

export function compare(current, baseline) {
  const failures = [];
  for (const [file, counts] of Object.entries(current)) {
    const allowed = baseline[file] || { tokens: 0, branches: 0 };
    if (counts.tokens > allowed.tokens) failures.push(`${file}: ${counts.tokens} vendor tokens (baseline ${allowed.tokens})`);
    if (counts.branches > allowed.branches) failures.push(`${file}: ${counts.branches} runtime branches (baseline ${allowed.branches})`);
  }
  return failures;
}

const isMain = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1];
if (isMain) {
  const baselinePath = join(root, 'scripts', 'vendor-lint-js.baseline.json');
  const current = measureTree(root, 'internal/daemon/static');
  if (process.argv.includes('--update')) {
    // The ratchet only turns one way: --update refuses any rise (P-RT10).
    const previous = JSON.parse(readFileSync(baselinePath, 'utf8'));
    const rises = compare(current, previous);
    if (rises.length) {
      console.log('refusing to raise the baseline:');
      for (const rise of rises) console.log('  ' + rise);
      process.exit(1);
    }
    writeFileSync(baselinePath, JSON.stringify(current, null, 2) + '\n');
    console.log('baseline written: ' + Object.keys(current).length + ' files');
    process.exit(0);
  }
  const baseline = JSON.parse(readFileSync(baselinePath, 'utf8'));
  const failures = compare(current, baseline);
  const sum = key => Object.values(current).reduce((total, counts) => total + counts[key], 0);
  console.log(`browser vendor tokens: ${sum('tokens')} in ${Object.keys(current).length} files; runtime branches: ${sum('branches')}`);
  const shrunk = Object.entries(baseline).filter(([file, counts]) => {
    const now = current[file] || { tokens: 0, branches: 0 };
    return now.tokens < counts.tokens || now.branches < counts.branches;
  });
  if (shrunk.length) console.log(`${shrunk.length} files are below baseline: run with --update to ratchet it down`);
  if (failures.length) {
    console.log('VENDOR-LINT-JS FAIL — names a vendor, model family or runtime branch in browser code:');
    for (const failure of failures) console.log('  ' + failure);
    process.exit(1);
  }
  console.log('VENDOR-LINT-JS PASS');
}
