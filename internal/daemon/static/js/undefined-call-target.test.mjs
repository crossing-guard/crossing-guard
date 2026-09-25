// A call to a name that does not exist is invisible to every check this
// repository runs: `node --check` parses, it does not resolve, and a call inside
// a click handler only fails when someone clicks. That is how a "Show diff"
// button shipped and threw for months.
//
// This is a lint, not a proof. It finds a bare call to a name that is neither
// declared in the module, imported into it, nor a standard global. It does NOT
// find: a misspelled import name (a module-link failure, not an undefined name),
// a member call such as `object.missing()`, or any name reached dynamically.
// JavaScript cannot be scoped correctly by pattern matching, so the rule
// deliberately over-collects bindings: a missed binding costs a false alarm, and
// a lint that cries wolf gets deleted.
//
// A post-work review found two blind spots that a first version had, both now
// closed and both covered by their own cases below: calls inside `${...}`
// interpolations, which is where much of this codebase calls anything, and a
// regular expression containing backticks, which used to open a phantom template
// literal and silently swallow the rest of the file.
import assert from 'node:assert/strict';
import test from 'node:test';
import { readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.dirname(fileURLToPath(import.meta.url));

const GLOBALS = new Set([
  'String', 'Number', 'Boolean', 'Array', 'Object', 'Date', 'Math', 'JSON', 'Map', 'Set', 'WeakMap', 'WeakSet',
  'Promise', 'Error', 'TypeError', 'RangeError', 'RegExp', 'Symbol', 'BigInt', 'Proxy', 'Reflect', 'Intl',
  'URL', 'URLSearchParams', 'TextEncoder', 'TextDecoder', 'Uint8Array', 'Int8Array', 'Int16Array',
  'Float32Array', 'Float64Array', 'ArrayBuffer', 'DataView', 'Blob', 'File', 'FormData', 'Headers', 'Request',
  'Response', 'AbortController', 'CustomEvent', 'Event', 'KeyboardEvent', 'MouseEvent', 'PointerEvent',
  'IntersectionObserver', 'ResizeObserver', 'MutationObserver', 'EventSource', 'WebSocket', 'Worker',
  'AudioContext', 'AudioWorkletNode', 'MediaRecorder', 'Image', 'Option', 'Notification',
  'fetch', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'queueMicrotask',
  'requestAnimationFrame', 'cancelAnimationFrame', 'structuredClone',
  'encodeURIComponent', 'decodeURIComponent', 'encodeURI', 'decodeURI',
  'parseInt', 'parseFloat', 'isNaN', 'isFinite', 'alert', 'confirm', 'prompt', 'btoa', 'atob',
  'getComputedStyle', 'registerProcessor', 'super', 'constructor', 'import', '$', '$$',
]);

const KEYWORDS = new Set([
  'if', 'for', 'while', 'switch', 'catch', 'function', 'return', 'typeof', 'instanceof', 'in', 'of', 'new',
  'delete', 'void', 'await', 'yield', 'do', 'else', 'try', 'finally', 'case', 'throw', 'with', 'class',
  'const', 'let', 'var', 'export', 'default', 'from', 'as', 'get', 'set', 'static', 'async',
]);

// Replace comment bodies and string contents with spaces, preserving length and
// newlines so reported line numbers stay true. Without this, prose in a comment
// that happens to be followed by "(" reads as a call.
//
// Template literals are NOT blanked wholesale: this codebase calls functions
// inside `${...}` constantly, and wiping those would hide most of its call sites
// from the rule. Only the literal text between interpolations is blanked, and
// interpolations are scanned as the code they are, nested templates included.
// A slash starts a regular expression only where a value may begin. Anything that
// can end an expression — a name, a number, a closing bracket — means division.
const REGEX_MAY_FOLLOW = new Set(['', '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '~', '^', '<', '>', 'return', 'typeof', 'case', 'in', 'of', 'new', 'delete', 'void', 'do', 'else', 'yield', 'await', 'instanceof']);
const regexAllowedAfter = character => REGEX_MAY_FOLLOW.has(character);

function blankLiterals(source) {
  const out = source.split('');
  const blank = (start, end) => {
    for (let index = start; index < end && index < out.length; index++) {
      if (out[index] !== '\n') out[index] = ' ';
    }
  };
  // One entry per open template literal: how deep we are inside its `${ }`, and
  // where its current run of literal text began.
  const templates = [];
  const open = () => templates[templates.length - 1];
  const inLiteralText = () => templates.length > 0 && open().braces === 0;
  let index = 0;
  let previous = '';
  while (index < source.length) {
    const here = source[index];
    const next = source[index + 1];
    const before = previous;
    if (!inLiteralText() && here.trim()) previous = here;
    if (inLiteralText()) {
      if (here === '\\') { index += 2; continue; }
      if (here === '`') { blank(open().textFrom, index); templates.pop(); index++; continue; }
      if (here === '$' && next === '{') { blank(open().textFrom, index); open().braces = 1; index += 2; continue; }
      index++;
      continue;
    }
    if (here === '/' && next === '/') {
      let end = source.indexOf('\n', index);
      if (end < 0) end = source.length;
      blank(index, end);
      index = end;
      continue;
    }
    if (here === '/' && next === '*') {
      let end = source.indexOf('*/', index + 2);
      end = end < 0 ? source.length : end + 2;
      blank(index, end);
      index = end;
      continue;
    }
    if (here === '"' || here === "'") {
      let end = index + 1;
      while (end < source.length) {
        if (source[end] === '\\') { end += 2; continue; }
        if (source[end] === here) break;
        end++;
      }
      blank(index + 1, end);
      index = end + 1;
      continue;
    }
    // A regular expression literal must be recognised, not read as code: this
    // codebase has one matching Markdown code spans whose pattern contains
    // backticks, and treating those as a template literal silently swallowed
    // every comment and call site after it. The classic slash ambiguity is
    // resolved the classic way, by what precedes the slash.
    if (here === '/' && regexAllowedAfter(before)) {
      let end = index + 1;
      let inClass = false;
      while (end < source.length) {
        const character = source[end];
        if (character === '\\') { end += 2; continue; }
        if (character === '\n') break;
        if (character === '[') inClass = true;
        else if (character === ']') inClass = false;
        else if (character === '/' && !inClass) break;
        end++;
      }
      blank(index + 1, end);
      previous = '/';
      index = end + 1;
      continue;
    }
    if (here === '`') { templates.push({ braces: 0, textFrom: index + 1 }); index++; continue; }
    if (templates.length) {
      if (here === '{') { open().braces++; index++; continue; }
      if (here === '}') {
        open().braces--;
        if (open().braces === 0) open().textFrom = index + 1;
        index++;
        continue;
      }
    }
    index++;
  }
  return out.join('');
}

function boundNames(source) {
  const names = new Set();
  const addEvery = text => {
    for (const match of String(text).matchAll(/([A-Za-z_$][\w$]*)/g)) names.add(match[1]);
  };
  for (const m of source.matchAll(/\bfunction\s*\*?\s*([A-Za-z_$][\w$]*)/g)) names.add(m[1]);
  for (const m of source.matchAll(/\bclass\s+([A-Za-z_$][\w$]*)/g)) names.add(m[1]);
  // A whole declaration list, so `let a = 1, b = 2` binds both.
  for (const m of source.matchAll(/\b(?:const|let|var)\s+([\s\S]*?);/g)) addEvery(m[1].replace(/=[^,]*/g, ' '));
  for (const m of source.matchAll(/\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)/g)) names.add(m[1]);
  for (const m of source.matchAll(/import\s+([\s\S]*?)\s+from\s*['"`]/g)) {
    for (const n of m[1].matchAll(/([A-Za-z_$][\w$]*)(?:\s+as\s+([A-Za-z_$][\w$]*))?/g)) names.add(n[2] || n[1]);
  }
  for (const m of source.matchAll(/\{([^{}]*)\}\s*=/g)) addEvery(m[1]);
  // Parameter lists, allowing one level of nesting for default arrow values.
  for (const m of source.matchAll(/\(((?:[^()]|\([^()]*\))*)\)\s*(?:=>|\{)/g)) addEvery(m[1]);
  for (const m of source.matchAll(/(?:^|[^.\w$#])([A-Za-z_$][\w$]*)\s*=>/gm)) names.add(m[1]);
  for (const m of source.matchAll(/catch\s*\(\s*([A-Za-z_$][\w$]*)/g)) names.add(m[1]);
  for (const m of source.matchAll(/^\s*(?:async\s+|static\s+|\*\s*)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*\{/gm)) names.add(m[1]);
  for (const m of source.matchAll(/[,{]\s*(?:async\s+)?([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*\{/g)) names.add(m[1]);
  for (const m of source.matchAll(/\bfor\s*\(\s*(?:const|let|var)\s+([A-Za-z_$][\w$]*)/g)) names.add(m[1]);
  return names;
}

// Walked, not enumerated: a directory list would leave the next new directory
// silently unguarded.
function modules(dir) {
  const found = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) found.push(...modules(full));
    else if (entry.name.endsWith('.js')) found.push(full);
  }
  return found;
}

function unresolvedCalls(raw) {
  // Call sites come from the blanked text so prose in a comment cannot look like
  // a call. Bindings come from the RAW text, because a regex literal containing a
  // quote or a backtick defeats the blanker, and a lost binding would raise a
  // false alarm about a name declared two lines away. Over-collecting a binding
  // only ever costs a missed finding, which is the safer way to be wrong.
  const source = blankLiterals(raw);
  const bound = boundNames(raw);
  const seen = new Set();
  const found = [];
  // A bare call: an identifier followed by "(", or by "?.(", and not preceded by
  // a member accessor, an optional chain, a private marker, or another word
  // character. `object.method()` and `object?.method()` are member calls and are
  // deliberately out of scope; `callback?.()` is not.
  for (const match of source.matchAll(/(^|[^.\w$?#])([A-Za-z_$][\w$]*)\s*(?:\?\.)?\s*\(/g)) {
    const name = match[2];
    if (KEYWORDS.has(name) || bound.has(name) || GLOBALS.has(name) || seen.has(name)) continue;
    seen.add(name);
    found.push({ name, line: source.slice(0, match.index).split('\n').length });
  }
  return found;
}

test('every bare call in the console resolves to an import, a declaration, or a standard global', () => {
  const found = [];
  const files = modules(ROOT);
  for (const file of files) {
    for (const call of unresolvedCalls(readFileSync(file, 'utf8'))) {
      found.push(`${path.relative(ROOT, file)}:${call.line} calls ${call.name}, which is not defined anywhere it can reach`);
    }
  }
  assert.ok(files.length > 20, `expected to scan the console modules, scanned ${files.length}`);
  assert.deepEqual(found, []);
});

// The shape of the defect this exists for, kept so the rule above cannot quietly
// become one that passes no matter what.
test('the rule catches a handler calling a name that was renamed away', () => {
  const found = unresolvedCalls([
    "import { revealRetainedBody } from './retained-body.js';",
    "function attach(host, ctx, effect) {",
    "  const reveal = el('button', '', 'Show diff');",
    "  reveal.addEventListener('click', () => revealEffectBody(ctx, reveal, { ordinal: effect.ordinal }));",
    "  host.append(reveal);",
    "}",
  ].join('\n'));
  assert.deepEqual(found.map(call => call.name), ['el', 'revealEffectBody']);
});

// Most of this console's calls happen inside `${...}`. Blanking template
// literals wholesale would have left that whole population unscanned, which is
// where a rename would most likely leave a dangling call.
test('the rule sees calls inside template interpolations, including nested ones', () => {
  const found = unresolvedCalls([
    "import { bytes } from './core.js';",
    "const row = value => `size ${bytes(value)} and ${missingHelper(value)}`;",
    "const nested = value => `outer ${`inner ${alsoMissing(value)}`}`;",
  ].join('\n'));
  assert.deepEqual(found.map(call => call.name), ['missingHelper', 'alsoMissing']);
});

// A regular expression whose pattern contains backticks used to open a phantom
// template literal, which swallowed every comment and call site after it.
test('a regular expression containing backticks does not swallow the rest of the file', () => {
  const found = unresolvedCalls([
    "const keep = html => html;",
    "let out = '';",
    "out = out.replace(/`([^`]+)`/g, (_, code) => keep(code));",
    "// a comment mentioning ladder (mirrors something) after the regex",
    "const after = () => stillMissing();",
  ].join('\n'));
  assert.deepEqual(found.map(call => call.name), ['stillMissing']);
});

test('the rule stays quiet for imports, declarations, members, and prose', () => {
  const found = unresolvedCalls([
    "import { helper } from './helper.js';",
    "// This comment mentions somethingUndefined() in passing.",
    "const label = 'text with fake(1) inside';",
    "const local = value => value + 1;",
    "function run(input) { return helper(local(input)) + input.method() + this.thing(); }",
  ].join('\n'));
  assert.deepEqual(found, []);
});
