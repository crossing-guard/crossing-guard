import assert from 'node:assert/strict';
import test from 'node:test';
import { createEffortPicker } from './thinking-effort.js';
import { modelPicker } from '../orchestration/agents/model-picker.js';

// Only DOM selection/focus semantics used by these controls. Layout, native
// keyboard behavior and the full composer are verified in the compiled browser.
class Element {
  constructor(tag) {
    this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.events = {}; this.hidden = false; this.disabled = false; this.isConnected = true;
    this.classList = { toggle() {} }; this._value = ''; this.textContent = '';
  }
  append(...nodes) { this.children.push(...nodes); }
  appendChild(node) { this.append(node); }
  replaceChildren(...nodes) { this.children = nodes; this._value = nodes[0]?.value || ''; }
  setAttribute(name, value) { this.attrs[name] = value; }
  addEventListener(name, fn) { this.events[name] = fn; }
  get options() { return this.children; }
  get value() { return this._value; }
  set value(value) { this._value = this.tagName === 'SELECT' && !this.options.some(item => item.value === value) ? '' : value; }
  get selectedOptions() { return this.options.filter(item => item.value === this.value); }
  querySelector(tag) { return this.children.find(node => node.tagName === tag.toUpperCase()) || this.children.map(node => node.querySelector(tag)).find(Boolean) || null; }
  contains(node) { return this === node || this.children.some(child => child.contains(node)); }
  focus() { document.activeElement = this; }
}
globalThis.document = { createElement: tag => new Element(tag) };
globalThis.localStorage = { getItem: () => null };
const all = node => [node, ...node.children.flatMap(all)];
const byText = (node, text) => all(node).find(item => item.textContent === text);
const byLabel = (node, text) => all(node).find(item => item.attrs['aria-label'] === text);
const tick = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; };
const catalog = models => ({ ok: true, json: async () => ({ state: 'fresh', models: models.map(id => ({ id, thinking_effort: { state: 'supported', choices: [{ id: 'native-high', label: 'High' }] } })) }) });

test('default prerequisite, direct/nested model focus and explicit inheritance recovery', async () => {
  for (const nested of [false, true]) {
    const model = document.createElement('select'), wrapper = document.createElement('label'); wrapper.append(model);
    const calls = [], changes = [];
    const picker = createEffortPicker({ modelControls: [nested ? wrapper : model], onChange: value => changes.push(value), onRefresh: async force => { calls.push(force); } });
    picker.update({}); picker.open(); await tick();
    assert.equal(document.activeElement, model);
    const effort = byLabel(picker.node, 'Thinking effort'); assert.equal(effort.disabled, true);
    byText(picker.node, 'Choose a model').onclick(); assert.equal(document.activeElement, model);
    byText(picker.node, 'Refresh models').onclick(); await tick(); assert.deepEqual(calls, [false, true]);
    picker.update({ selection: { kind: 'level', value: 'retired' }, unavailable: 'Refresh failed.' });
    assert.equal(effort.disabled, false, 'explicit unavailable level retains inherit recovery');
    assert.ok(all(picker.node).some(item => item.textContent.includes('Choose a model above') && item.textContent.includes('Refresh failed.')));
    effort.value = ''; effort.onchange(); assert.deepEqual(changes, [{ kind: 'inherit' }]);
  }
});

test('managed refresh preserves choices made while discovery runs and isolates independent pickers', async () => {
  const capabilities = [{ runtime: 'native', displayName: 'Native', canStart: true, acceptsCustomModel: true, models: [], modes: [{ id: 'read', risk: 'normal' }, { id: 'audit', risk: 'normal' }] }];
  globalThis.fetch = async () => catalog(['one', 'two']);
  const primary = modelPicker(capabilities, { runtime: 'native', model: 'one', mode: 'read' });
  const fallback = modelPicker(capabilities, { runtime: 'native', model: 'one', mode: 'read' }); await tick();
  const wait = deferred(); globalThis.fetch = () => wait.promise;
  byText(primary.node, 'Refresh models').onclick();
  const model = byLabel(primary.node, 'Model'); model.value = 'two'; model.onchange();
  const mode = byLabel(primary.node, 'Read-only mode'); mode.value = 'audit';
  const effort = byLabel(primary.node, 'Thinking effort'); effort.value = 'native-high'; effort.onchange();
  wait.resolve(catalog(['one'])); await tick();
  assert.deepEqual(primary.value(), { runtime: 'native', model: 'two', mode: 'audit', thinking_effort: { kind: 'level', value: 'native-high' } });
  assert.equal(fallback.value().model, 'one'); assert.equal(fallback.value().mode, 'read');
  const customWait = deferred(); globalThis.fetch = () => customWait.promise;
  byText(primary.node, 'Refresh models').onclick(); model.value = '\u0000custom'; model.onchange();
  const custom = byLabel(primary.node, 'Model id'); custom.value = 'typed-during-refresh'; custom.oninput();
  customWait.resolve(catalog(['one', 'two'])); await tick();
  assert.equal(primary.value().model, 'typed-during-refresh'); assert.equal(primary.value().mode, 'audit');
});

test('late managed runtime response cannot repaint current runtime; failure can recover', async () => {
  const capabilities = ['first', 'second'].map(runtime => ({ runtime, displayName: runtime, canStart: true, models: [], modes: [{ id: 'read', risk: 'normal' }] }));
  const old = deferred(); globalThis.fetch = url => url.includes('first') ? old.promise : Promise.resolve(catalog(['second-model']));
  const picker = modelPicker(capabilities, { runtime: 'first' });
  const runtime = byLabel(picker.node, 'Runs on'); runtime.value = 'second'; runtime.onchange(); await tick();
  old.resolve(catalog(['first-model'])); await tick();
  const model = byLabel(picker.node, 'Model'); assert.ok(model.options.some(item => item.value === 'second-model')); assert.ok(!model.options.some(item => item.value === 'first-model'));
  model.value = 'second-model'; model.onchange();
  globalThis.fetch = async () => { throw new Error('offline'); };
  byText(picker.node, 'Refresh models').onclick(); await tick();
  assert.equal(picker.value().model, 'second-model');
  assert.ok(all(picker.node).some(item => item.textContent.includes('Refresh models to try again')));
  globalThis.fetch = async () => catalog(['second-model']); byText(picker.node, 'Refresh models').onclick(); await tick();
  assert.equal(byLabel(picker.node, 'Thinking effort').options[1].disabled, false);
});

test('refresh stays focusable while busy and ignores duplicate clicks', async () => {
  const wait = deferred(); let calls = 0;
  const picker = createEffortPicker({ onChange() {}, onRefresh: () => { calls++; return wait.promise; } });
  picker.update({});
  const refresh = byText(picker.node, 'Refresh models');
  refresh.focus(); refresh.onclick(); refresh.onclick();
  assert.equal(calls, 1); assert.equal(refresh.disabled, false);
  assert.equal(refresh.attrs['aria-disabled'], 'true'); assert.equal(document.activeElement, refresh);
  wait.resolve(); await tick(); assert.equal(refresh.attrs['aria-disabled'], 'false');
});
