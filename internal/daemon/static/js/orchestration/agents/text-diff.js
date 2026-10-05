// Line diff for comparing two versions of an agent's instructions (agents
// redesign plan §8, Versions tab). A bounded longest-common-subsequence over
// lines: inputs beyond the cell budget degrade to a whole-block replacement
// rather than a slow page, and the result says so.

// diffLines returns [{op:'same'|'add'|'del', text}] turning `before` into
// `after`. `maxCells` bounds the LCS table (rows × columns).
export function diffLines(before, after, { maxCells = 400000 } = {}) {
  const a = splitLines(before);
  const b = splitLines(after);
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  let endA = a.length, endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) { endA--; endB--; }
  const head = a.slice(0, start).map(text => ({ op: 'same', text }));
  const tail = a.slice(endA).map(text => ({ op: 'same', text }));
  const midA = a.slice(start, endA), midB = b.slice(start, endB);
  if ((midA.length + 1) * (midB.length + 1) > maxCells) {
    return { lines: [...head, ...midA.map(text => ({ op: 'del', text })), ...midB.map(text => ({ op: 'add', text })), ...tail], truncated: true };
  }
  return { lines: [...head, ...lcsDiff(midA, midB), ...tail], truncated: false };
}

function splitLines(text) {
  const value = String(text ?? '');
  if (value === '') return [];
  return value.replace(/\r\n/g, '\n').split('\n');
}

function lcsDiff(a, b) {
  const rows = a.length + 1, cols = b.length + 1;
  const table = new Uint32Array(rows * cols);
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      table[i * cols + j] = a[i] === b[j]
        ? table[(i + 1) * cols + j + 1] + 1
        : Math.max(table[(i + 1) * cols + j], table[i * cols + j + 1]);
    }
  }
  const out = [];
  let i = 0, j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) { out.push({ op: 'same', text: a[i] }); i++; j++; }
    else if (table[(i + 1) * cols + j] >= table[i * cols + j + 1]) { out.push({ op: 'del', text: a[i] }); i++; }
    else { out.push({ op: 'add', text: b[j] }); j++; }
  }
  while (i < a.length) out.push({ op: 'del', text: a[i++] });
  while (j < b.length) out.push({ op: 'add', text: b[j++] });
  return out;
}

// diffStats counts added and removed lines for a one-line summary.
export function diffStats(result) {
  let added = 0, removed = 0;
  for (const line of result?.lines || []) {
    if (line.op === 'add') added++;
    else if (line.op === 'del') removed++;
  }
  return { added, removed };
}
