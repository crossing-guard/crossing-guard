import test from 'node:test';
import assert from 'node:assert/strict';
import { nativeOpenModel, nativeOpenControl, openNative, setNativeOpenEnabled } from './native-open.js';

const linked = { runtime: 'runtime-a', id: 's1', native_open: { url: 'app-a://threads/s1', app: 'App A' } };
const fakeDocument = () => ({ createElement: tag => {
  const listeners = {};
  return { tag, listeners, addEventListener: (name, fn) => { listeners[name] = fn; } };
} });

test('a session with a published link gets that link and the app as its label', () => {
  setNativeOpenEnabled(true);
  assert.deepEqual(nativeOpenModel(linked),
    { href: 'app-a://threads/s1', label: 'Open in App A', title: 'Opens this session in App A' });
});

test('a session without a link, or with a partial one, gets nothing', () => {
  setNativeOpenEnabled(true);
  for (const session of [null, {}, { native_open: null }, { native_open: { url: 'app-a://x' } },
    { native_open: { app: 'App A' } }, { native_open: { url: 7, app: 'App A' } }, { native_open: { url: 'no-scheme', app: 'A' } }]) {
    assert.equal(nativeOpenModel(session), null);
    assert.equal(nativeOpenControl(session, fakeDocument()), null);
  }
});

test('web, file and script schemes are never handed to the operating system', () => {
  setNativeOpenEnabled(true);
  for (const url of ['http://x/', 'https://x/', 'HTTPS://x/', 'file:///etc/passwd', 'javascript:alert(1)',
    'JavaScript:alert(1)', 'data:text/html,x', 'blob:x', 'about:blank']) {
    assert.equal(nativeOpenModel({ native_open: { url, app: 'App A' } }), null, url);
  }
});

test('the config switch removes every control and restores it', () => {
  setNativeOpenEnabled(false);
  assert.equal(nativeOpenModel(linked), null);
  assert.equal(openNative(linked, { location: {} }), false);
  setNativeOpenEnabled(true);
  assert.notEqual(nativeOpenModel(linked), null);
  setNativeOpenEnabled(undefined);
  assert.notEqual(nativeOpenModel(linked), null, 'a config that omits the switch keeps the default');
});

test('the anchor has its own class, a real href, and its click opens the app without reaching the row', () => {
  setNativeOpenEnabled(true);
  const win = { location: { href: 'console' } };
  const anchor = nativeOpenControl(linked, fakeDocument(), win);
  assert.equal(anchor.tag, 'a');
  assert.equal(anchor.href, 'app-a://threads/s1');
  const classes = anchor.className.split(/\s+/);
  assert.ok(classes.includes('session-native'));
  for (const reserved of ['ref', 'ref-ok', 'session-link']) assert.ok(!classes.includes(reserved), reserved);
  let stopped = 0, prevented = 0;
  anchor.listeners.click({ stopPropagation: () => { stopped++; }, preventDefault: () => { prevented++; } });
  assert.equal(stopped, 1, 'the row must not see the click');
  assert.equal(prevented, 1, 'the click navigates once, through the hand-off');
  assert.equal(win.location.href, 'app-a://threads/s1');
  let menuStopped = 0, menuPrevented = 0;
  anchor.listeners.contextmenu({ stopPropagation: () => { menuStopped++; }, preventDefault: () => { menuPrevented++; } });
  assert.equal(menuStopped, 1, 'the row must not replace the link menu with its tag menu');
  assert.equal(menuPrevented, 0, 'the browser shows its own link menu');
  let auxStopped = 0, auxPrevented = 0;
  anchor.listeners.auxclick({ stopPropagation: () => { auxStopped++; }, preventDefault: () => { auxPrevented++; } });
  assert.deepEqual([auxStopped, auxPrevented], [1, 1], 'a middle click opens nothing');
});

test('switching off removes the controls already on the page', () => {
  const removed = [];
  const doc = { querySelectorAll: selector => { assert.equal(selector, 'a.session-native'); return [{ remove: () => removed.push(1) }, { remove: () => removed.push(2) }]; } };
  setNativeOpenEnabled(true, doc);
  assert.deepEqual(removed, [], 'switching on removes nothing');
  setNativeOpenEnabled(false, doc);
  assert.deepEqual(removed, [1, 2]);
  setNativeOpenEnabled(true, doc);
});

test('openNative navigates the given window to the link', () => {
  setNativeOpenEnabled(true);
  const win = { location: { href: 'console' } };
  assert.equal(openNative(linked, win), true);
  assert.equal(win.location.href, 'app-a://threads/s1');
  assert.equal(openNative({}, win), false);
});
