import { normalizeDiffModel } from './diff-model.js';

// Retained providers give us text, not typed Git facts. This defensive parser is
// presentation-only: gaps fall back to visible raw text and never authorize mutation.

function decodeGitQuoted(value) {
  const source = String(value || '').trim();
  if (!source.startsWith('"') || !source.endsWith('"')) return source;
  const body = source.slice(1, -1);
  const bytes = [];
  const encoder = new TextEncoder();
  const appendText = value => bytes.push(...encoder.encode(value));
  const simple = { a: '\x07', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t', v: '\v', '\\': '\\', '"': '"' };
  for (let index = 0; index < body.length;) {
    if (body[index] !== '\\') {
      const point = String.fromCodePoint(body.codePointAt(index));
      appendText(point);
      index += point.length;
      continue;
    }
    index++;
    const octal = body.slice(index).match(/^[0-7]{1,3}/)?.[0];
    if (octal) {
      bytes.push(parseInt(octal, 8));
      index += octal.length;
      continue;
    }
    const escaped = body[index++] || '';
    appendText(simple[escaped] ?? escaped);
  }
  try { return new TextDecoder('utf-8', { fatal: true }).decode(Uint8Array.from(bytes)); }
  catch { return new TextDecoder('utf-8').decode(Uint8Array.from(bytes)); }
}

function headerPath(value, prefix) {
  let path = decodeGitQuoted(value);
  if (path === '/dev/null') return path;
  if (path.startsWith(prefix)) path = path.slice(prefix.length);
  return path;
}

function splitGitHeader(line) {
  const body = line.slice('diff --git '.length);
  const quoted = body.match(/^("(?:[^"\\]|\\.)*")\s+("(?:[^"\\]|\\.)*")$/);
  if (quoted) return [headerPath(quoted[1], 'a/'), headerPath(quoted[2], 'b/')];
  const marker = body.lastIndexOf(' b/');
  if (marker < 0) return ['', ''];
  return [headerPath(body.slice(0, marker), 'a/'), headerPath(body.slice(marker + 1), 'b/')];
}

const newFile = (oldPath, newPath) => ({ oldPath, newPath, kind: 'text', summary: '', hunks: [], metadata: [] });

const nameOf = file => file.newPath || file.oldPath || '(unknown file)';

function startFile(files, line) {
  const [oldPath, newPath] = splitGitHeader(line);
  const file = newFile(oldPath, newPath);
  files.push(file);
  return file;
}

// applyFileFacts consumes a line that describes the FILE rather than its content:
// its paths, and the shapes that mean there is no readable text at all. It
// answers what the caller must do next, because a binary or conflicted marker
// also ends whatever hunk was open.
function applyFileFacts(file, line, insideHunk) {
  if (!insideHunk && line.startsWith('--- ')) { file.oldPath = headerPath(line.slice(4).split('\t')[0], 'a/'); return 'consumed'; }
  if (!insideHunk && line.startsWith('+++ ')) { file.newPath = headerPath(line.slice(4).split('\t')[0], 'b/'); return 'consumed'; }
  if (line.startsWith('Binary files ') || line === 'GIT binary patch') { file.kind = 'binary'; file.summary = line; return 'ends-hunk'; }
  if (line.startsWith('rename from ')) { file.kind = 'rename'; file.oldPath = line.slice(12); return 'consumed'; }
  if (line.startsWith('rename to ')) { file.kind = 'rename'; file.newPath = line.slice(10); return 'consumed'; }
  if (/^(old mode|new mode|new file mode|deleted file mode) /.test(line)) {
    if (file.kind === 'text') file.kind = 'mode';
    file.metadata.push(line);
    return 'consumed';
  }
  if (line.startsWith('@@@ ')) { file.kind = 'conflict'; file.summary = line; return 'ends-hunk'; }
  return '';
}

// appendHunkLine adds one content line and advances the line cursor. It returns
// false for a line that belongs to neither side, which the caller reports.
function appendHunkLine(hunk, cursor, line) {
  if (line.startsWith('\\ No newline at end of file')) {
    hunk.lines.push({ kind: 'meta', oldLine: null, newLine: null, text: line });
    return true;
  }
  if (line.startsWith('+')) {
    hunk.lines.push({ kind: 'add', oldLine: null, newLine: cursor.newLine, text: line.slice(1) });
    cursor.newLine++;
    return true;
  }
  if (line.startsWith('-')) {
    hunk.lines.push({ kind: 'delete', oldLine: cursor.oldLine, newLine: null, text: line.slice(1) });
    cursor.oldLine++;
    return true;
  }
  if (line.startsWith(' ')) {
    hunk.lines.push({ kind: 'context', oldLine: cursor.oldLine, newLine: cursor.newLine, text: line.slice(1) });
    cursor.oldLine++;
    cursor.newLine++;
    return true;
  }
  hunk.lines.push({ kind: 'meta', oldLine: null, newLine: null, text: line });
  return false;
}

// settleFiles resolves the facts that can only be known once a file is finished:
// a mode change that turned out to carry text is text, and collected metadata
// becomes the summary when nothing better claimed it.
function settleFiles(files) {
  for (const file of files) {
    if (file.kind === 'mode' && file.hunks.length) file.kind = 'text';
    if (file.metadata.length && !file.summary) file.summary = file.metadata.join(' · ');
    delete file.metadata;
  }
}

function hunkRange(header) {
  const match = header.match(/^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@/);
  if (!match) return null;
  return { oldLine: Number(match[1]), newLine: Number(match[3]) };
}

// A retained body can arrive with no file header at all: a runtime that reports
// one file per change has no reason to repeat the path inside the text. The path
// then comes from the typed effect the caller already holds, which is a stronger
// source than a patch header, never weaker. seed carries it; without one the
// parse is exactly what it always was.
function parseBody(lines, seed) {
  const files = [];
  const gaps = [];
  const cursor = { oldLine: 0, newLine: 0 };
  let file = seed ? newFile(seed.oldPath, seed.newPath) : null;
  let hunk = null;
  let preamble = 0;
  let hunkCount = 0;
  if (file) files.push(file);

  for (let index = 0; index < lines.length; index++) {
    const line = lines[index];
    if (index === lines.length - 1 && line === '') continue;
    if (line.startsWith('diff --git ')) {
      file = startFile(files, line);
      hunk = null;
      continue;
    }
    if (!file) {
      if (line.startsWith('--- ') && lines[index + 1]?.startsWith('+++ ')) {
        file = newFile(headerPath(line.slice(4).split('\t')[0], 'a/'), '');
        files.push(file);
        continue;
      }
      // One fact, not one per line: a body with no file boundary produces as many
      // of these as it has lines, which drowns the reader in accounting.
      if (line.trim()) preamble++;
      continue;
    }
    const fact = applyFileFacts(file, line, !!hunk);
    if (fact) {
      if (fact === 'ends-hunk') hunk = null;
      continue;
    }
    if (line.startsWith('@@ ')) {
      const range = hunkRange(line);
      if (!range) {
        gaps.push(`Malformed hunk header in ${nameOf(file)}`);
        hunk = null;
        continue;
      }
      cursor.oldLine = range.oldLine;
      cursor.newLine = range.newLine;
      hunk = { header: line, lines: [] };
      file.hunks.push(hunk);
      hunkCount++;
      continue;
    }
    if (!hunk) {
      if (line && !/^(index |similarity index |dissimilarity index )/.test(line)) file.metadata.push(line);
      continue;
    }
    if (!appendHunkLine(hunk, cursor, line)) gaps.push(`Unparsed hunk line in ${nameOf(file)}`);
  }

  settleFiles(files);
  if (preamble) gaps.push(`${preamble} line${preamble === 1 ? '' : 's'} before any file boundary were not read.`);
  return { files, gaps, hunkCount };
}

// headerlessHunks is the exact trigger for using a caller-supplied path: nothing
// parsed, and the body opens on a hunk header. Anything else parses as it always
// did, so no existing body changes shape.
function headerlessHunks(lines) {
  for (const line of lines) {
    if (!line.trim()) continue;
    return /^@@ /.test(line);
  }
  return false;
}

export function parseUnifiedDiff(value, source = {}, hints = {}) {
  const raw = String(value ?? '').replace(/\r\n?/g, '\n');
  const lines = raw.split('\n');
  let parsed = parseBody(lines, null);

  const path = typeof hints.path === 'string' ? hints.path : '';
  if (!parsed.files.length && path && headerlessHunks(lines)) {
    const newPath = typeof hints.newPath === 'string' ? hints.newPath : path;
    const seeded = parseBody(lines, { oldPath: path, newPath });
    // A hinted body that yields no hunks is still unreadable. Keeping the seeded
    // result would show an empty file where the captured text belongs.
    if (seeded.hunkCount > 0) parsed = seeded;
  }

  const gaps = parsed.gaps.slice();
  if (!parsed.files.length && raw.trim()) gaps.push('No file boundary was recognized.');

  return normalizeDiffModel({
    source: { kind: 'retained', label: 'Retained captured diff', ...source },
    files: parsed.files,
    parseGaps: gaps,
    // Any line that never landed in a hunk means the captured text still holds
    // something the rendering does not, so it must remain available.
    rawFallback: gaps.length || !parsed.files.length ? raw : '',
  });
}
