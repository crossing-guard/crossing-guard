import { readDataEvents } from './task-stream.js';

const encoder = new TextEncoder();
const body = chunks => ({ getReader: () => ({
  index: 0,
  async read() {
    if (this.index >= chunks.length) return { done: true };
    return { done: false, value: encoder.encode(chunks[this.index++]) };
  },
}) });

let failures = 0;
const check = (name, condition) => {
  if (!condition) { failures++; console.error('FAIL', name); }
};

const events = [];
await readDataEvents(body(['data: {"type":"one"}\n', '\ndata: {"type":"two"}\n\n',
  'event: ignored\n\ndata: {"type":"tail"}']), event => events.push(event));
check('split, multiple frames, and EOF tail stay ordered', events.map(event => event.type).join(',') === 'one,two,tail');

let malformed = 0;
const afterMalformed = [];
await readDataEvents(body(['data: nope\r\n\r\ndata: {"type":\r\ndata: "recovered"}\r\n\r\n']),
  event => afterMalformed.push(event), () => { malformed++; });
check('malformed JSON is isolated and reported', malformed === 1);
check('CRLF and multiline data recover after malformed input', afterMalformed[0]?.type === 'recovered');

let cancelled = false;
const throwingBody = { getReader: () => ({
  sent: false,
  async read() {
    if (this.sent) return { done: true };
    this.sent = true;
    return { done: false, value: encoder.encode('data: {"type":"throw"}\n\n') };
  },
  async cancel() { cancelled = true; },
}) };
try {
  await readDataEvents(throwingBody, () => { throw new Error('projection failed'); });
} catch { /* expected */ }
check('reader is cancelled when a consumer throws', cancelled);

if (failures) process.exit(1);
console.log('task-stream: all pass');
