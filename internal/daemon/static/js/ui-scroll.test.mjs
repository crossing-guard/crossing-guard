import test from 'node:test';
import assert from 'node:assert/strict';
import { atScrollEnd, captureTranscriptPosition, restoreTranscriptPosition } from './ui.js';

function fixture({ scrollTop = 0, scrollHeight = 1000, clientHeight = 300, rows = [] } = {}) {
  const scroller = {
    scrollTop, scrollHeight, clientHeight,
    getBoundingClientRect: () => ({ top: 100 }),
  };
  const children = rows.map(({ key, top, height = 40 }) => ({
    dataset: { transcriptKey: key },
    getBoundingClientRect: () => ({ top, bottom: top + height }),
  }));
  return { scroller, log: { children }, children };
}

test('background output follows only when already at the end', () => {
  const atEnd = fixture({ scrollTop: 700 });
  assert.equal(atScrollEnd(atEnd.scroller), true);
  const captured = captureTranscriptPosition(atEnd.scroller, atEnd.log);
  atEnd.scroller.scrollHeight = 1200;
  restoreTranscriptPosition(atEnd.scroller, atEnd.log, captured);
  assert.equal(atEnd.scroller.scrollTop, 1200);

  const reading = fixture({ scrollTop: 200, rows: [{ key: 'row-a', top: 80 }, { key: 'row-b', top: 120 }] });
  const held = captureTranscriptPosition(reading.scroller, reading.log);
  reading.scroller.scrollHeight = 1300;
  restoreTranscriptPosition(reading.scroller, reading.log, held);
  assert.equal(reading.scroller.scrollTop, 200, 'new output does not steal the reader position');
});

test('reconciliation preserves the first visible stable row and its pixel offset', () => {
  const view = fixture({ scrollTop: 250, rows: [{ key: 'before', top: 50 }, { key: 'reading', top: 115 }] });
  const captured = captureTranscriptPosition(view.scroller, view.log);
  assert.equal(captured.key, 'reading');
  assert.equal(captured.offset, 15);
  view.children[1].getBoundingClientRect = () => ({ top: 175, bottom: 215 });
  restoreTranscriptPosition(view.scroller, view.log, captured);
  assert.equal(view.scroller.scrollTop, 310, 'the row returns to the same viewport offset');
});

test('a surviving predecessor anchors recovery when the visible row is replaced', () => {
  const view = fixture({ scrollTop: 250, rows: [{ key: 'before', top: 60 }, { key: 'removed', top: 110 }] });
  const captured = captureTranscriptPosition(view.scroller, view.log);
  view.log.children = [view.children[0]];
  view.children[0].getBoundingClientRect = () => ({ top: 90, bottom: 130 });
  restoreTranscriptPosition(view.scroller, view.log, captured);
  assert.equal(view.scroller.scrollTop, 240);
});
