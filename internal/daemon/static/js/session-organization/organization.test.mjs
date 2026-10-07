import test from 'node:test';
import assert from 'node:assert/strict';
import { organization, activeQuery, adoptViews, isStructuredQuery, selectView } from './organization-state.js';
import { organizedRailUrl, organizedPageUrl, railStateKey, groupsAreRepositories, groupLabel, foldGroupKey } from './view-group-source.js';
import { tagLabel, parseTagText, sameTag, tagClass, tagTitle, isOwnerTag, rowTime } from './tag-chips.js';
import { isTypingTarget, shortcutMatches } from './shortcut-match.js';
import { boardConfig, boardMoveTags, cardMoveTags, moveCard, openColumnMenu, renderBoardNodes, paintBoardAttention, pinCard, releaseCard, refreshBoard, boardBusy, boardNeedsControl } from './board.js';
import { rememberCardLink, cardOpenItem, showCardOpen, settleCardOpen } from './board-card-open.js';
import { setNativeOpenEnabled } from '../session/native-open.js';

const view = { id: 'view_1', name: 'Mine', query: 'mine:follow-up', group_by: 'tag-key:topic', sort: 'longest' };

test('with no view and no filter the rail is requested exactly as it always was', () => {
  adoptViews({ views: [], rejected: [], state_token: 't0' });
  selectView('');
  assert.equal(activeQuery(), null);
  assert.equal(organizedRailUrl(null, 500), '');
  assert.equal(organizedPageUrl(null, { key: '/repo', mode: 'all', offset: 0, limit: 15 }), '');
  assert.equal(railStateKey(null, '/repo'), '/repo');
  assert.equal(groupsAreRepositories(null), true);
});

test('a fresh installation adopts an empty list: no view exists until the owner saves one', () => {
  adoptViews({ views: [], rejected: [], state_token: 't0' });
  assert.deepEqual(organization.views, []);
  adoptViews(undefined);
  assert.deepEqual(organization.views, []);
});

test('a selected view becomes the rail query, grouped and sorted as the owner saved it', () => {
  adoptViews({ views: [view], rejected: [], state_token: 't1' });
  selectView('view_1');
  const active = activeQuery();
  assert.deepEqual(active, { query: 'mine:follow-up', groupBy: 'tag-key:topic', sort: 'longest', scope: 'view:view_1' });
  assert.equal(organizedRailUrl(active, 500),
    '/api/sessions?view=rail&repository_limit=500&query=mine%3Afollow-up&group_by=tag-key%3Atopic&sort=longest');
  assert.equal(organizedPageUrl(active, { key: 'checkout', mode: 'all', offset: 15, limit: 15, selected: { runtime: 'claude', id: 'a b' } }),
    '/api/sessions?view=group&group=checkout&mode=all&offset=15&limit=15&query=mine%3Afollow-up&group_by=tag-key%3Atopic&sort=longest&selected_runtime=claude&selected_id=a%20b');
});

test('a view with an empty query still selects the filtered rail', () => {
  assert.match(organizedRailUrl({ query: '', groupBy: '', sort: '', scope: 'view:x' }, 500), /&query=&group_by=repository$/);
});

test('a filter typed over a view searches everything unless narrowed on purpose', () => {
  adoptViews({ views: [view], rejected: [], state_token: 't1' });
  selectView('view_1');
  organization.barQuery = 'tag:phase=plan';
  assert.equal(activeQuery().query, 'tag:phase=plan');
  organization.within = true;
  assert.equal(activeQuery().query, 'mine:follow-up tag:phase=plan');
  assert.equal(activeQuery().sort, 'longest');
  selectView('view_1');
  assert.equal(organization.barQuery, '');
  assert.equal(organization.within, false);
  assert.equal(organization.editingID, '');
});

test('a deleted view is no longer selected', () => {
  adoptViews({ views: [view], rejected: [], state_token: 't1' });
  selectView('view_1');
  adoptViews({ views: [], rejected: [], state_token: 't2' });
  assert.equal(organization.viewID, '');
  assert.equal(activeQuery(), null);
});

test('only a term with a colon is a filter; plain words stay the global search', () => {
  for (const text of ['tag:approved', '-tag:vcs=commit', 'indexer repo:app', 'title:"sprint retro"']) assert.equal(isStructuredQuery(text), true, text);
  for (const text of ['', 'indexer latency', 'what about: this', '12:30 standup', 'a -b']) assert.equal(isStructuredQuery(text), false, text);
});

test('group memory is kept apart per view and per grouping', () => {
  const a = { scope: 'view:1', groupBy: 'tag-key:topic' }, b = { scope: 'view:2', groupBy: 'tag-key:topic' };
  assert.notEqual(railStateKey(a, 'checkout'), railStateKey(b, 'checkout'));
  assert.notEqual(railStateKey(a, 'checkout'), 'checkout');
});

test('a tag group is named as the owner spelled it, and the group without the tag says which tag', () => {
  const active = { groupBy: 'tag-key:topic' };
  assert.equal(groupLabel(active, { key: 'order-routing', label: 'Order-Routing' }), 'Order-Routing');
  assert.equal(groupLabel(active, { key: 'checkout' }), 'checkout');
  assert.equal(groupLabel(active, { key: '' }), 'no topic');
  assert.equal(groupLabel({ groupBy: 'none' }, { key: '' }), 'all');
  assert.equal(groupLabel({ groupBy: '' }, { key: '/work/app' }), '');
});

test('what the owner types is the tag: first colon separates, nothing else does', () => {
  assert.deepEqual(parseTagText('approved'), { value: 'approved' });
  assert.deepEqual(parseTagText('  topic : order-routing '), { key: 'topic', value: 'order-routing' });
  assert.deepEqual(parseTagText(':odd'), { value: ':odd' });
  assert.equal(tagLabel({ key: 'topic', value: 'checkout' }), 'topic:checkout');
  assert.equal(tagLabel({ value: 'approved' }), 'approved');
  assert.equal(sameTag({ value: 'Approved' }, { value: 'approved' }), true);
  assert.equal(sameTag({ key: 'topic', value: 'a' }, { value: 'a' }), false);
});

test('who said a tag is a colour and a hover, never a word on the chip', () => {
  const mine = { value: 'approved', provenance: 'user-asserted', at: 1789000000 };
  const agent = { value: 'plan', provenance: 'model-claimed', by: 'Plan follower', at: 1789000000 };
  const seen = { key: 'phase', value: 'plan', provenance: 'observed', at: 1789000000 };
  assert.equal(tagClass(mine), 'chip cl-user-asserted');
  assert.equal(tagClass(agent), 'chip cl-model-claimed');
  assert.equal(tagClass(seen), 'chip cl-observed');
  assert.equal(isOwnerTag(mine), true);
  assert.equal(isOwnerTag(agent), false);
  assert.match(tagTitle(mine), /^you · /);
  assert.match(tagTitle(agent), /^Plan follower · /);
  for (const tag of [mine, agent, seen]) {
    assert.doesNotMatch(tagTitle(tag) + tagLabel(tag), /asserted|claimed|observed|provenance|detector|index/i);
  }
});

test('a row in a view shows how long it has been there, otherwise its last activity', () => {
  assert.equal(rowTime({ modified: '2026-09-01T10:00:00Z' }), '2026-09-01T10:00:00Z');
  assert.equal(rowTime({ modified: '2026-09-01T10:00:00Z', in_view_since: 1789000000 }), new Date(1789000000000).toISOString());
});

test('the tag shortcut never fires while typing, and modifiers must match exactly', () => {
  assert.equal(isTypingTarget({ tagName: 'TEXTAREA' }), true);
  assert.equal(isTypingTarget({ tagName: 'DIV', isContentEditable: true }), true);
  assert.equal(isTypingTarget({ tagName: 'BUTTON' }), false);
  assert.equal(isTypingTarget(null), false);
  const key = (k, mods = {}) => ({ key: k, metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, ...mods });
  assert.equal(shortcutMatches('t', key('t')), true);
  assert.equal(shortcutMatches('t', key('T')), true);
  assert.equal(shortcutMatches('t', key('t', { metaKey: true })), false);
  assert.equal(shortcutMatches('Meta+Shift+T', key('t', { metaKey: true, shiftKey: true })), true);
  assert.equal(shortcutMatches('', key('t')), false, 'an empty binding switches the shortcut off');
});

import { tagChoices } from './tag-popover.js';
import { recentToggles } from './header-tags.js';

test('typing offers the owner\'s matching tags, and Create only for text that is not already one', () => {
  const vocabulary = [{ value: 'approved', sessions: 3 }, { key: 'topic', value: 'Order-Routing', sessions: 2 }];
  assert.deepEqual(tagChoices('', vocabulary), { typed: { value: '' }, known: vocabulary, isNew: false });
  const typing = tagChoices('orde', vocabulary);
  assert.equal(typing.isNew, true);
  assert.deepEqual(typing.known.map(tag => tag.value), ['Order-Routing']);
  assert.equal(tagChoices('APPROVED', vocabulary).isNew, false, 'another letter case is the same tag, not a new one');
  assert.equal(tagChoices('topic:order-routing', vocabulary).isNew, false);
  assert.deepEqual(tagChoices('topic:indexer', vocabulary).typed, { key: 'topic', value: 'indexer' });
  assert.equal(tagChoices('anything', []).isNew, true, 'with no tags yet, whatever is typed can be created');
});

test('header toggles are the most recent tags this session does not already carry — never a shipped list', () => {
  const vocabulary = [{ value: 'approved' }, { value: 'follow-up' }, { key: 'topic', value: 'checkout' }, { value: 'dropped' }, { value: 'later' }];
  const carried = [{ value: 'Approved', provenance: 'user-asserted' }, { value: 'follow-up', provenance: 'model-claimed' }];
  assert.deepEqual(recentToggles(vocabulary, carried, 3).map(tag => tag.value), ['follow-up', 'checkout', 'dropped'],
    'an agent\'s tag of the same name does not stop the owner applying his own');
  assert.deepEqual(recentToggles([], carried, 3), [], 'nothing is offered before the owner has tagged anything');
  assert.deepEqual(recentToggles(vocabulary, [], 0), []);
});

// A move writes the rendered board's key and the card's own column, or nothing
// (board-view-query-correctness plan invariant 6).
test('a board move applies the destination and retracts the origin under the board key', () => {
  assert.deepEqual(boardMoveTags('flow', 'building', 'review'),
    { apply: [{ key: 'flow', value: 'review' }], retract: [{ key: 'flow', value: 'building' }] });
  assert.deepEqual(boardMoveTags('flow', '', 'review'), { apply: [{ key: 'flow', value: 'review' }], retract: [] });
  assert.equal(boardMoveTags('flow', 'review', 'review'), null);
  assert.equal(boardMoveTags('', 'building', 'review'), null);
  assert.equal(boardMoveTags('flow', 'building', ''), null);
});

// A stand-in for a rendered card: closest() answers for its own board and column.
function renderedCard(groupKey, column) {
  const hosts = { '.board': { dataset: groupKey == null ? {} : { groupKey } }, '.board-column': { dataset: { column } } };
  return { closest: selector => hosts[selector] || null };
}

test('every move reads the key and the origin from the card it moves, whichever came before', () => {
  // A keyboard move before any drag retracts the card's own column.
  assert.deepEqual(cardMoveTags(renderedCard('phase', 'plan'), 'build').retract, [{ key: 'phase', value: 'plan' }]);
  // After card A was dragged from build, card B still retracts its own column.
  cardMoveTags(renderedCard('phase', 'build'), 'review');
  assert.deepEqual(cardMoveTags(renderedCard('phase', 'plan'), 'review').retract, [{ key: 'phase', value: 'plan' }]);
  // Two boards keyed differently, rendered in turn: each writes its own key.
  assert.equal(cardMoveTags(renderedCard('flow', 'building'), 'review').apply[0].key, 'flow');
  assert.equal(cardMoveTags(renderedCard('phase', 'plan'), 'build').apply[0].key, 'phase');
  // A board that does not say which key it groups by writes nothing.
  assert.equal(cardMoveTags(renderedCard(null, 'plan'), 'build'), null);
  assert.equal(cardMoveTags({ closest: () => null }, 'build'), null);
});

// A DOM stand-in: just enough tree, events and focus for the board's menu and
// announcements. Clicks bubble through parentNode, as in a browser.
class FakeElement {
  constructor(doc, tag) {
    Object.assign(this, { doc, tagName: tag.toUpperCase(), children: [], parentNode: null, dataset: {}, style: {},
      attributes: {}, listeners: {}, textContent: '', classes: new Set(), scrollTop: 0, scrollLeft: 0 });
    this.classList = { add: c => this.classes.add(c), remove: c => this.classes.delete(c),
      contains: c => this.classes.has(c), toggle: (c, on) => (on ? this.classes.add(c) : this.classes.delete(c)) };
  }
  set className(value) { this.classes = new Set(String(value).split(/\s+/).filter(Boolean)); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return name in this.attributes ? this.attributes[name] : null; }
  appendChild(child) { child.remove(); child.parentNode = this; this.children.push(child); return child; }
  replaceChildren(...nodes) { for (const child of [...this.children]) child.remove(); for (const node of nodes) this.appendChild(node); }
  get isConnected() { return this.doc.body.contains(this); }
  // detached counts every time the node leaves a parent (a move counts too),
  // so a test can prove a card was never taken out of the tree.
  remove() {
    if (!this.parentNode) return;
    this.detached = (this.detached || 0) + 1;
    // Leaving the tree takes focus away, as in a browser.
    if (this.doc.activeElement && this.contains(this.doc.activeElement)) this.doc.activeElement = null;
    this.parentNode.children = this.parentNode.children.filter(c => c !== this);
    this.parentNode = null;
  }
  insertBefore(child, before) {
    child.remove();
    child.parentNode = this;
    const at = before ? this.children.indexOf(before) : -1;
    if (at < 0) this.children.push(child); else this.children.splice(at, 0, child);
    return child;
  }
  contains(node) { for (let at = node; at; at = at.parentNode) if (at === this) return true; return false; }
  matches(selector) { return selector.startsWith('.') ? this.classes.has(selector.slice(1)) : this.tagName === selector.toUpperCase(); }
  closest(selector) { for (let at = this; at; at = at.parentNode) if (at.matches(selector)) return at; return null; }
  querySelectorAll(selector) {
    const out = [];
    const walk = node => { for (const child of node.children) { if (child.matches(selector)) out.push(child); walk(child); } };
    walk(this);
    return out;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  click() { for (let at = this; at; at = at.parentNode) if (at.onclick) at.onclick({ type: 'click', target: this }); }
  focus() { this.doc.activeElement = this; }
  getBoundingClientRect() { return { top: 0, bottom: 20, left: 0, right: 40 }; }
}

function fakeDocument() {
  const doc = { events: [], listeners: new Map(), activeElement: null };
  doc.createElement = tag => new FakeElement(doc, tag);
  doc.body = doc.createElement('body');
  doc.querySelector = selector => doc.body.querySelector(selector);
  doc.dispatchEvent = event => { doc.events.push(event.type); return true; };
  doc.addEventListener = (type, fn) => doc.listeners.set(fn, type);
  doc.removeEventListener = (type, fn) => doc.listeners.delete(fn);
  return doc;
}

// A rendered board of two declared columns with one card in the first. The
// card's click opens the session, exactly as boardCard wires it.
function boardWithCard(doc, groupKey) {
  const host = renderBoardNodes({ name: 'Flow', board: {} }, { groupKey, columns: ['building', 'review'] }, []);
  const card = doc.createElement('button');
  card.className = 'board-card';
  Object.assign(card.dataset, { runtime: 'r', sessionId: 's1' });
  card.onclick = () => doc.dispatchEvent(new CustomEvent('cg:board-open'));
  host.querySelector('.board-column').querySelector('.board-cards').appendChild(card);
  return { host, card };
}

function withDocument(run) {
  const prior = globalThis.document;
  const doc = fakeDocument();
  globalThis.document = doc;
  return Promise.resolve(run(doc)).finally(() => { globalThis.document = prior; });
}

// A tag group's key is its value folded by the daemon (Go strings.ToLower). The
// browser's fold must agree; these pairs are pinned in Go too
// (TestSessionGroupKeyFoldsOnlyTagGroups, board-column-letter-case plan §6).
test('a column name folds to the daemon\'s group key', () => {
  for (const [name, key] of [['Review', 'review'], ['İstanbul', 'istanbul'], ['ΟΔΟΣ', 'οδοσ'], ['ΑΣ Β', 'ασ β'], ['', '']]) {
    assert.equal(foldGroupKey(name), key);
  }
});

test('a declared column holds the group spelled in another letter case', () => withDocument(async doc => {
  selectView(''); // no active query: columns render from the groups and request nothing
  const groups = [{ key: 'review', total: 2 }, { key: 'building', total: 1 }, { key: 'stray', total: 1 }];
  const host = renderBoardNodes({ name: 'Flow', board: {} }, { groupKey: 'flow', columns: ['Building', 'Review', 'Done'] }, groups);
  const columns = host.querySelectorAll('.board-column');
  assert.deepEqual(columns.map(c => c.dataset.column), ['Building', 'Review', 'Done', 'stray'],
    'declared columns keep the owner\'s spelling; a folded twin is not a second column');
  assert.deepEqual(columns.map(c => c.querySelector('.board-column-count')?.textContent ?? null), ['1', '2', null, '1']);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(host.querySelectorAll('.board-empty').length, 0, 'no column request was sent, so none failed');
  // A move into the column writes the name as declared, from the name as declared.
  const card = doc.createElement('button');
  card.className = 'board-card';
  Object.assign(card.dataset, { runtime: 'r', sessionId: 's1' });
  columns[0].querySelector('.board-cards').appendChild(card);
  const writes = [];
  openColumnMenu(card, (moved, column) => moveCard(moved, column, async (sessions, change) => { writes.push(change); }));
  host.querySelectorAll('.board-menu-item').find(item => item.textContent === 'Review').click();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(writes, [{ apply: [{ key: 'flow', value: 'Review' }], retract: [{ key: 'flow', value: 'Building' }] }]);
}));

test('a board\'s declared columns are read trimmed, and a column page asks for the declared spelling', () => {
  const board = boardConfig({ group_by: 'tag-key:flow', board: { columns: [' Review ', 'Done'] } });
  assert.deepEqual(board.columns, ['Review', 'Done']);
  const active = { scope: 'view:v', query: 'tag:flow=*', groupBy: 'tag-key:flow', sort: '' };
  assert.match(organizedPageUrl(active, { key: board.columns[0], mode: 'all', offset: 0, limit: 50 }), /[?&]group=Review&/);
});

test('the column menu lives on the board, and choosing a column moves the card without opening it', () => withDocument(async doc => {
  const { host, card } = boardWithCard(doc, 'flow');
  const moves = [];
  openColumnMenu(card, (moved, column) => moves.push([moved, column]));
  openColumnMenu(card, (moved, column) => moves.push([moved, column])); // a second press replaces, never stacks
  const menus = host.querySelectorAll('.board-menu');
  assert.equal(menus.length, 1);
  assert.equal(card.contains(menus[0]), false, 'the menu is never inside the card button');
  assert.equal(doc.listeners.size, 2, 'one menu, one keydown and one pointerdown listener');
  const items = menus[0].querySelectorAll('button');
  assert.deepEqual(items.map(item => item.textContent), ['review']);
  items[0].click();
  assert.deepEqual(moves, [[card, 'review']]);
  assert.deepEqual(doc.events, [], 'no cg:board-open: the click never reached the card');
  assert.equal(host.querySelectorAll('.board-menu').length, 0);
  assert.equal(doc.listeners.size, 0, 'closing removes every listener the menu added');
}));

test('Escape closes the menu, returns focus to the card and stops there', () => withDocument(doc => {
  const { host, card } = boardWithCard(doc, 'flow');
  openColumnMenu(card, () => assert.fail('Escape must not move'));
  let stopped = false;
  for (const [fn] of doc.listeners) {
    if (doc.listeners.get(fn) === 'keydown') fn({ key: 'Escape', target: doc.activeElement, preventDefault() {}, stopPropagation() { stopped = true; } });
  }
  assert.equal(host.querySelectorAll('.board-menu').length, 0);
  assert.equal(doc.activeElement, card);
  assert.equal(stopped, true);
  assert.equal(doc.listeners.size, 0);
}));

test('a refused move says why and writes nothing; a drop on its own column says nothing', () => withDocument(async doc => {
  const writes = [];
  const write = async (...args) => { writes.push(args); };
  const { card } = boardWithCard(doc, '');
  await moveCard(card, 'review', write);
  assert.match(doc.querySelector('.board-live').textContent, /does not say which tag key/);
  const keyed = boardWithCard(doc, 'flow');
  await moveCard(keyed.card, '', write);
  assert.match(doc.querySelector('.board-live').textContent, /no column to move to/);
  doc.querySelector('.board-live').textContent = '';
  await moveCard(keyed.card, 'building', write);
  assert.equal(doc.querySelector('.board-live').textContent, '');
  assert.deepEqual(writes, []);
  await moveCard(keyed.card, 'review', write);
  assert.deepEqual(writes[0][1], { apply: [{ key: 'flow', value: 'review' }], retract: [{ key: 'flow', value: 'building' }] });
}));

// An agent's line repaints on the card in place, keyed by runtime + catalog
// id, with no refetch: the card node (and a drag holding it) survives, and
// the helper's words are text (escalation-delivery plan §6, RT-13).
test('a card repaints its agent line in place from the rail status', () => withDocument(async doc => {
  let ask = { unseen: true, text: 'The owner needs to pick <b>A</b>', drafts: 0, draftText: '' };
  const calls = [];
  const status = (runtime, catalogID, nativeID) => { calls.push([runtime, catalogID, nativeID]); return { ask }; };
  const host = renderBoardNodes({ name: 'Flow', board: {} }, { groupKey: 'flow', columns: ['building'] }, [], { status });
  const card = doc.createElement('button');
  card.className = 'board-card';
  Object.assign(card.dataset, { runtime: 'r', sessionId: 's1', resumeId: 'n1' });
  card.setAttribute('aria-label', 'Title · r');
  host.querySelector('.board-cards').appendChild(card);
  paintBoardAttention(host);
  assert.deepEqual(calls, [['r', 's1', 'n1']]);
  const line = card.querySelector('.board-card-agent');
  assert.ok(line && line.classes.has('board-card-ask'));
  assert.equal(line.children[1].textContent, 'asks you: The owner needs to pick <b>A</b>');
  assert.equal(card.attributes['aria-label'], 'Title · r · asks you: The owner needs to pick <b>A</b>');
  // Acknowledged ask, one unsent reply: a quiet line, no dot, same card node.
  ask = { unseen: false, text: 'x', drafts: 1, draftText: 'Go ahead.' };
  paintBoardAttention(host);
  assert.equal(host.querySelectorAll('.board-card')[0], card);
  assert.equal(card.querySelectorAll('.board-card-agent').length, 1);
  assert.equal(card.querySelector('.board-card-agent').textContent, 'reply not sent: Go ahead.');
  assert.equal(card.querySelector('.session-status-dot'), null);
  assert.equal(card.attributes['aria-label'], 'Title · r · reply not sent: Go ahead.');
  ask = { unseen: false, drafts: 0 };
  paintBoardAttention(host);
  assert.equal(card.querySelector('.board-card-agent'), null);
  assert.equal(card.attributes['aria-label'], 'Title · r');
}));

// J1: the card names the agent; J8: a column that fails to load says so.
test('the card attributes an ask to its agent', () => withDocument(async doc => {
  const status = () => ({ ask: { unseen: true, text: 'Pick one.', agent: 'Deploy helper', drafts: 0 } });
  const host = renderBoardNodes({ name: 'Flow', board: {} }, { groupKey: 'flow', columns: ['building'] }, [], { status });
  const card = doc.createElement('button');
  card.className = 'board-card';
  Object.assign(card.dataset, { runtime: 'r', sessionId: 's1', resumeId: 's1' });
  host.querySelector('.board-cards').appendChild(card);
  paintBoardAttention(host);
  assert.equal(card.querySelector('.board-card-agent').children[1].textContent, 'asks you: Pick one. (Deploy helper)');
}));

// A board view selected with a stubbed fetch and localStorage: each populated
// column's card read goes through api(), and the test sees every URL it asked.
async function withBoardColumnFetch(respond, run, columns = ['building', 'review']) {
  const board = { id: 'view_board', name: 'Flow', query: 'tag:flow', group_by: 'tag-key:flow', sort: 'longest',
    board: { columns, empty_columns: true } };
  const priorFetch = globalThis.fetch;
  const priorStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  const urls = [];
  try {
    globalThis.fetch = async url => { urls.push(url); return respond(url); };
    Object.defineProperty(globalThis, 'localStorage', { configurable: true,
      value: { getItem: () => null, setItem() {}, removeItem() {} } });
    adoptViews({ views: [board], rejected: [], state_token: 't-board' });
    selectView('view_board');
    await withDocument(doc => run(doc, board, urls));
  } finally {
    selectView('');
    adoptViews({ views: [], rejected: [], state_token: 't0' });
    globalThis.fetch = priorFetch;
    if (priorStorage) Object.defineProperty(globalThis, 'localStorage', priorStorage);
    else delete globalThis.localStorage;
  }
}

const flushColumnLoads = () => new Promise(resolve => setImmediate(resolve));

// board-column-mode-fix plan: a column holding sessions asks for mode=all —
// the daemon refuses any mode but all/open, and an empty one broke every
// populated column on every board.
test('a populated board column requests its group page with mode=all and renders the cards', () =>
  withBoardColumnFetch(() => ({ ok: true, status: 200, text: async () => '',
    json: async () => ({ sessions: [{ runtime: 'runtime-a', id: 's1', title: 'Fix the board' }] }) }),
  async (doc, board, urls) => {
    const host = renderBoardNodes(board, boardConfig(board), [{ key: 'building', total: 1 }]);
    await flushColumnLoads();
    assert.deepEqual(urls, ['/api/v1/sessions?view=group&group=building&mode=all&offset=0'
      + '&query=tag%3Aflow&group_by=tag-key%3Aflow&sort=longest&board_view=view_board']);
    const [building, review] = host.querySelectorAll('.board-column');
    assert.deepEqual(building.querySelectorAll('.board-card').map(card => card.dataset.sessionId), ['s1']);
    assert.equal(building.querySelector('.board-empty'), null);
    assert.equal(building.querySelector('.board-overflow'), null, 'a page with no total claims no overflow');
    assert.equal(review.querySelector('.board-empty').textContent, 'No sessions', 'an empty column fetches nothing');
  }));

// The daemon's group page as the board reads it (board-column-overflow plan
// §4): min(limit, total) rows with the total, where an owner-placed column
// read with no limit gets the daemon's configured bound.
function columnOf(total, bound) {
  return url => {
    const params = new URL(url, 'http://local').searchParams;
    const asked = params.get('limit') ? Number(params.get('limit'))
      : params.get('board_view') ? bound : 15;
    const sessions = Array.from({ length: Math.min(asked, total) },
      (_, index) => ({ runtime: 'runtime-a', id: 's' + index, title: 'Session ' + index }));
    return { ok: true, status: 200, text: async () => '', json: async () => ({ total, sessions }) };
  };
}

test('a column of 51 sessions renders every card: the bound is the board\'s, not the rail page', () =>
  withBoardColumnFetch(columnOf(51, 200), async (doc, board) => {
    const host = renderBoardNodes(board, boardConfig(board), [{ key: 'building', total: 51 }]);
    await flushColumnLoads();
    const building = host.querySelector('.board-column');
    assert.equal(building.querySelectorAll('.board-card').length, 51);
    assert.equal(building.querySelector('.board-overflow'), null);
  }));

test('a column past the bound says how many sessions it does not show, after the cards', () =>
  withBoardColumnFetch(columnOf(251, 200), async (doc, board) => {
    // The rail read's count draws the header first; the page's own total then
    // replaces it, and the line counts from that same total.
    const host = renderBoardNodes(board, boardConfig(board), [{ key: 'building', total: 260 }]);
    await flushColumnLoads();
    const building = host.querySelector('.board-column');
    assert.equal(building.querySelector('.board-column-count').textContent, '251');
    assert.equal(building.querySelectorAll('.board-card').length, 200);
    const overflow = building.querySelector('.board-overflow');
    assert.equal(overflow?.textContent, '51 more sessions not shown');
    assert.equal(overflow.parentNode, building, 'the line is outside the card list');
  }));

test('a column the daemon refuses says it could not load instead of looking empty (J8)', () =>
  withBoardColumnFetch(() => ({ ok: false, status: 400, text: async () => 'mode must be all or open\n',
    json: async () => ({}) }),
  async (doc, board) => {
    const host = renderBoardNodes(board, boardConfig(board), [{ key: 'building', total: 1 }]);
    await flushColumnLoads();
    assert.equal(host.querySelector('.board-column').querySelector('.board-empty').textContent,
      'Could not load this column: mode must be all or open');
  }));

// board-move-refresh plan §4. A stand-in daemon: column pages come from a
// membership map, read when the request arrives, and the tag write changes
// that map as the real write changes the store. `hold` keeps answers back.
function boardDaemon(members, { bound = 200 } = {}) {
  const daemon = { members, reads: [], fail: new Set(), hold: null, gated: false, waiting: [] };
  daemon.respond = async url => {
    const group = new URL(url, 'http://local').searchParams.get('group');
    daemon.reads.push(group);
    const ids = [...(daemon.members[group] || [])];
    const failed = daemon.fail.has(group);
    if (daemon.hold) await daemon.hold;
    // gated: every answer waits for the test to let it go, one at a time.
    if (daemon.gated) await new Promise(resolve => daemon.waiting.push(resolve));
    if (failed) return { ok: false, status: 500, text: async () => 'store unavailable\n', json: async () => ({}) };
    const sessions = ids.slice(0, bound).map(id => ({ runtime: 'runtime-a', id, title: 'Session ' + id }));
    return { ok: true, status: 200, text: async () => '', json: async () => ({ total: ids.length, sessions }) };
  };
  daemon.write = async ([session], change) => {
    for (const tag of change.retract) daemon.members[tag.value] = (daemon.members[tag.value] || []).filter(id => id !== session.id);
    for (const tag of change.apply) daemon.members[tag.value] = [session.id, ...(daemon.members[tag.value] || [])];
  };
  return daemon;
}

// A board rendered from the stand-in daemon, its first column reads finished.
async function renderedBoard(daemon, board) {
  const groups = Object.entries(daemon.members).filter(([, ids]) => ids.length).map(([key, ids]) => ({ key, total: ids.length }));
  const host = renderBoardNodes(board, boardConfig(board), groups);
  await flushColumnLoads();
  daemon.reads.length = 0;
  return host;
}

const columnNamed = (host, name) => host.querySelectorAll('.board-column').find(column => column.dataset.column === name);
const cardIds = column => column.querySelectorAll('.board-card').map(card => card.dataset.sessionId);
const cardFor = (column, id) => column.querySelectorAll('.board-card').find(card => card.dataset.sessionId === id);
const countOf = column => column.querySelector('.board-column-count')?.textContent ?? null;

test('a move shows the card in its new column with both counts updated, reading only the columns it touched', () => {
  const daemon = boardDaemon({ building: ['s1', 's2'], review: [], done: ['s3'] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building'), review = columnNamed(host, 'review');
    const stays = cardFor(building, 's2');
    await moveCard(cardFor(building, 's1'), 'review', daemon.write);
    assert.deepEqual(cardIds(building), ['s2']);
    assert.equal(countOf(building), '1');
    assert.deepEqual(cardIds(review), ['s1']);
    assert.equal(countOf(review), '1');
    assert.equal(review.querySelector('.board-empty'), null, 'a column that gained a card no longer says No sessions');
    assert.deepEqual([...daemon.reads].sort(), ['building', 'review'], 'one read per touched column, none for done');
    assert.deepEqual(doc.events, ['cg:board-moved']);
    // The card that stayed is the same node and never left the tree: a drag
    // holding it, or focus on it, survives the refresh (RT-13).
    assert.equal(cardFor(building, 's2'), stays);
    assert.equal(stays.detached || 0, 0);
    assert.equal(doc.activeElement, cardFor(review, 's1'), 'focus follows the moved card');
  }, ['building', 'review', 'done']);
});

test('the moved card leaves its origin when the write succeeds, before any column answers', () => {
  const daemon = boardDaemon({ building: ['s1', 's2'], review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building');
    let release;
    daemon.hold = new Promise(resolve => { release = resolve; });
    const moving = moveCard(cardFor(building, 's1'), 'review', daemon.write);
    await flushColumnLoads();
    assert.deepEqual(cardIds(building), ['s2'], 'a stale card could be moved a second time into two columns');
    release();
    await moving;
    assert.deepEqual(cardIds(columnNamed(host, 'review')), ['s1']);
  });
});

test('a move out of a column past the bound updates its overflow line', () => {
  const ids = Array.from({ length: 201 }, (_, index) => 's' + index);
  const daemon = boardDaemon({ building: ids, review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building');
    assert.equal(building.querySelector('.board-overflow').textContent, '1 more session not shown');
    await moveCard(cardFor(building, 's0'), 'review', daemon.write);
    assert.equal(building.querySelectorAll('.board-card').length, 200);
    assert.equal(building.querySelector('.board-overflow'), null);
    assert.equal(countOf(building), '200');
  });
});

test('a burst of moves costs one read in flight and one queued per column, and never paints the older answer', () => {
  const daemon = boardDaemon({ building: ['s1', 's2', 's3', 's4'], review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building');
    daemon.gated = true;
    const moves = [moveCard(cardFor(building, 's1'), 'review', daemon.write)];
    await flushColumnLoads();
    for (const id of ['s2', 's3']) moves.push(moveCard(cardFor(building, id), 'review', daemon.write));
    await flushColumnLoads();
    // The reads in flight were asked after the first move only: their answer
    // still lists s2 and s3. Let them go; a newer read is queued behind each.
    daemon.waiting.splice(0).forEach(release => release());
    await flushColumnLoads();
    assert.deepEqual(cardIds(building), ['s4'], 'the superseded answer would put moved cards back');
    daemon.gated = false;
    daemon.waiting.splice(0).forEach(release => release());
    await Promise.all(moves);
    assert.equal(daemon.reads.filter(group => group === 'building').length, 2);
    assert.deepEqual(cardIds(building), ['s4']);
    assert.equal(countOf(building), '1');
    assert.deepEqual(cardIds(columnNamed(host, 'review')), ['s3', 's2', 's1']);
  });
});

test('a column holding the card being dragged waits for the drag to end before it repaints', () => {
  const daemon = boardDaemon({ building: ['s1', 's2'], review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building');
    const dragged = cardFor(building, 's2');
    dragged.listeners.dragstart[0]({ dataTransfer: { setData() {} } });
    await moveCard(cardFor(building, 's1'), 'review', daemon.write);
    assert.equal(countOf(building), '2', 'the page is not applied under a drag');
    assert.equal(dragged.detached || 0, 0);
    dragged.listeners.dragend[0]({});
    await flushColumnLoads();
    assert.equal(countOf(building), '1');
    assert.deepEqual(cardIds(building), ['s2']);
    assert.equal(cardFor(building, 's2'), dragged);
  });
});

test('a column whose refresh fails keeps its cards and says so; the next move recovers it', () => {
  const daemon = boardDaemon({ building: ['s1', 's2'], review: ['s9'] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building'), review = columnNamed(host, 'review');
    daemon.fail.add('review');
    await moveCard(cardFor(building, 's1'), 'review', daemon.write);
    assert.deepEqual(cardIds(building), ['s2']);
    assert.deepEqual(cardIds(review), ['s9']);
    assert.match(review.querySelector('.board-empty').textContent, /^Could not load this column: store unavailable/);
    daemon.fail.clear();
    await moveCard(cardFor(building, 's2'), 'review', daemon.write);
    assert.deepEqual(cardIds(review), ['s2', 's1', 's9']);
    assert.equal(review.querySelector('.board-empty'), null);
    assert.equal(countOf(review), '3');
  });
});

test('a failed write, or a card that is on no board, reads nothing and announces no move', () => {
  const daemon = boardDaemon({ building: ['s1'], review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building');
    const card = cardFor(building, 's1');
    await moveCard(card, 'review', async () => { throw new Error('tags are read-only'); });
    assert.equal(doc.querySelector('.board-live').textContent, 'Move failed: tags are read-only');
    assert.deepEqual(cardIds(building), ['s1']);
    card.remove();
    doc.querySelector('.board-live').textContent = '';
    let wrote = false;
    await moveCard(card, 'review', async () => { wrote = true; });
    assert.equal(wrote, false);
    assert.equal(doc.querySelector('.board-live').textContent, '', 'never the untrue "no tag key" refusal');
    assert.deepEqual(daemon.reads, []);
    assert.deepEqual(doc.events, []);
  });
});

test('an emptied declared column says No sessions, an emptied observed one goes, and a menu closes with its card', () => {
  const daemon = boardDaemon({ building: ['s1'], review: [], parked: ['s5', 's6'] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building'), parked = columnNamed(host, 'parked');
    await moveCard(cardFor(building, 's1'), 'review', daemon.write);
    assert.equal(building.querySelector('.board-empty').textContent, 'No sessions');
    assert.equal(countOf(building), null);
    // Another writer took s6 out of parked while its menu was open here.
    openColumnMenu(cardFor(parked, 's6'), () => {});
    daemon.members.parked = ['s5'];
    await moveCard(cardFor(parked, 's5'), 'review', daemon.write);
    assert.equal(host.querySelectorAll('.board-menu').length, 0);
    assert.equal(doc.listeners.size, 0);
    assert.equal(columnNamed(host, 'parked'), undefined, 'an observed column with no sessions is not drawn');
  });
});

test('a board keeps reading its columns under the query it was drawn from, whatever is typed in the bar later', () => {
  const daemon = boardDaemon({ building: ['s1', 's2'], review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board, urls) => {
    const host = await renderedBoard(daemon, board);
    organization.barQuery = 'tag:phase=plan';
    try {
      urls.length = 0;
      await moveCard(cardFor(columnNamed(host, 'building'), 's1'), 'review', daemon.write);
      assert.equal(urls.length, 2);
      for (const url of urls) assert.match(url, /&query=tag%3Aflow&group_by=tag-key%3Aflow&sort=longest&board_view=view_board$/);
    } finally { organization.barQuery = ''; }
  });
});

test('a session in two columns keeps its other card through a move, and that column is read too', () => {
  const daemon = boardDaemon({ building: ['s1'], review: [], done: ['s1', 's3'] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const done = columnNamed(host, 'done');
    const other = cardFor(done, 's1');
    await moveCard(cardFor(columnNamed(host, 'building'), 's1'), 'review', daemon.write);
    assert.deepEqual([...daemon.reads].sort(), ['building', 'done', 'review']);
    assert.equal(cardFor(done, 's1'), other);
    assert.equal(other.detached || 0, 0);
    assert.deepEqual(cardIds(columnNamed(host, 'review')), ['s1']);
  }, ['building', 'review', 'done']);
});

test('focus is never taken from where the owner put it, and a reordered card keeps the focus it had', () => {
  const daemon = boardDaemon({ building: ['s1', 's2', 's3'], review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await renderedBoard(daemon, board);
    const building = columnNamed(host, 'building');
    const elsewhere = doc.createElement('input');
    doc.body.appendChild(elsewhere);
    elsewhere.focus();
    await moveCard(cardFor(building, 's1'), 'review', daemon.write);
    assert.equal(doc.activeElement, elsewhere);
    // s3 now sorts first: the reconcile takes it out to move it, focus and all.
    const focused = cardFor(building, 's3');
    focused.focus();
    daemon.members.building = ['s3', 's2', 's4'];
    daemon.members.review = ['s1'];
    await moveCard(cardFor(columnNamed(host, 'review'), 's1'), 'building', async () => {});
    assert.deepEqual(cardIds(building), ['s3', 's2', 's4']);
    assert.equal(cardFor(building, 's3'), focused);
    assert.equal(doc.activeElement, focused);
  });
});

// Board placement rules (board-observed-columns plan). The stand-in daemon
// answers the rail read with the groups it is told and each column read with
// that column's rows, as the daemon places them.
function ruledDaemon(columns) {
  const daemon = { columns, urls: [] };
  daemon.respond = async url => {
    daemon.urls.push(url);
    const params = new URL(url, 'http://local').searchParams;
    const body = params.get('view') === 'rail'
      ? { repositories: Object.entries(daemon.columns).filter(([, rows]) => rows.length).map(([key, rows]) => ({ key, total: rows.length })) }
      : { total: (daemon.columns[params.get('group').toLowerCase()] || []).length, sessions: daemon.columns[params.get('group').toLowerCase()] || [] };
    return { ok: true, status: 200, text: async () => '', json: async () => body };
  };
  return daemon;
}
const ruledRow = (id, more = {}) => ({ runtime: 'runtime-a', id, title: 'Session ' + id, ...more });
const withRules = board => { board.board.placement = [{ column: 'review', query: 'tag:vcs=push' }]; return board; };
async function ruledBoard(daemon, board) {
  const groups = Object.entries(daemon.columns).filter(([, rows]) => rows.length).map(([key, rows]) => ({ key, total: rows.length }));
  const host = renderBoardNodes(board, boardConfig(board), groups);
  globalThis.document.body.appendChild(host);
  await flushColumnLoads();
  daemon.urls.length = 0;
  return host;
}

test('a board with rules shows the sessions nothing placed in a last column that takes no drop', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1', { placed_by: 'rule' })], '': [ruledRow('s2'), ruledRow('s3')] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, withRules(board));
    const columns = host.querySelectorAll('.board-column');
    const last = columns[columns.length - 1];
    assert.deepEqual(columns.map(column => column.dataset.column), ['building', 'review', '']);
    assert.equal(last.querySelector('.board-column-name').textContent, 'no flow');
    assert.deepEqual(cardIds(last), ['s2', 's3']);
    assert.equal(last.listeners.drop, undefined, 'the unplaced column is not a place to put a card');
    assert.equal(columns[0].listeners.drop.length, 1);
    openColumnMenu(cardFor(last, 's2'), () => {});
    assert.deepEqual(host.querySelectorAll('.board-menu-item').map(item => item.textContent), ['building', 'review']);
    openColumnMenu(cardFor(columns[0], 's1'), () => {});
    assert.deepEqual(host.querySelectorAll('.board-menu-item').map(item => item.textContent), ['review', 'Pin here']);
  });
});

test('a board without rules hides the sessions with no value, and its menu offers only columns', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1', { placed_by: 'owner' })], '': [ruledRow('s2')] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, board);
    assert.deepEqual(host.querySelectorAll('.board-column').map(column => column.dataset.column), ['building', 'review']);
    openColumnMenu(cardFor(columnNamed(host, 'building'), 's1'), () => {});
    assert.deepEqual(host.querySelectorAll('.board-menu-item').map(item => item.textContent), ['review']);
  });
});

test('Pin here applies the card\'s own column and retracts nothing', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1', { placed_by: 'rule' })] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, withRules(board));
    const writes = [];
    daemon.columns.building = [ruledRow('s1', { placed_by: 'owner' })];
    const card = cardFor(columnNamed(host, 'building'), 's1');
    await pinCard(card, async (sessions, change) => { writes.push([sessions, change]); });
    assert.deepEqual(writes, [[[{ runtime: 'runtime-a', id: 's1' }], { apply: [{ key: 'flow', value: 'building' }], retract: [] }]]);
    assert.equal(daemon.urls.length, 1, 'only the card\'s column is read again');
    assert.equal(cardFor(columnNamed(host, 'building'), 's1'), card, 'the card is kept');
    assert.equal(card.dataset.placedBy, 'owner', 'and repainted from the fresh row');
    assert.equal(doc.querySelector('.board-live').textContent, 'Pinned in building');
  });
});

test('Follow observed retracts every owner value under the key and reads the board again', () => {
  const pinned = ruledRow('s1', { placed_by: 'owner', observed_group: 'review' });
  const daemon = ruledDaemon({ building: [pinned], review: [] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, withRules(board));
    const card = cardFor(columnNamed(host, 'building'), 's1');
    assert.equal(card.querySelector('.board-card-observed').textContent, 'matches review');
    openColumnMenu(card, () => {});
    assert.deepEqual(host.querySelectorAll('.board-menu-item').map(item => item.textContent), ['review', 'Pin here', 'Follow observed']);
    const writes = [];
    const read = async () => ({ sessions: [{ tags: [
      { key: 'Flow', value: 'building', provenance: 'user-asserted' }, { key: 'flow', value: 'done', provenance: 'user-asserted' },
      { key: 'flow', value: 'fact', provenance: 'observed' }, { key: 'topic', value: 'x', provenance: 'user-asserted' }] }] });
    const write = async (sessions, change) => {
      writes.push(change);
      daemon.columns.building = [];
      daemon.columns.review = [ruledRow('s1', { placed_by: 'rule' })];
      return { sessions: [{ tags: [{ key: 'topic', value: 'x', provenance: 'user-asserted' }] }] };
    };
    await releaseCard(card, { read, write });
    assert.deepEqual(writes, [{ apply: [], retract: [{ key: 'flow', value: 'building' }, { key: 'flow', value: 'done' }] }]);
    assert.match(daemon.urls[0], /view=rail&repository_limit=500&.*&board_view=view_board$/);
    assert.deepEqual(cardIds(columnNamed(host, 'building')), []);
    assert.deepEqual(cardIds(columnNamed(host, 'review')), ['s1']);
    assert.equal(cardFor(columnNamed(host, 'review'), 's1').querySelector('.board-card-observed'), null);
    assert.match(doc.querySelector('.board-live').textContent, /^Released/);
  });
});

test('a card that cannot be released says so', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1', { placed_by: 'owner' })] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, withRules(board));
    const tags = { sessions: [{ tags: [{ key: 'flow', value: 'building', provenance: 'user-asserted' }] }] };
    await releaseCard(cardFor(columnNamed(host, 'building'), 's1'), { read: async () => tags, write: async () => tags });
    assert.match(doc.querySelector('.board-live').textContent, /could not be released/);
  });
});

test('a board refresh adds the columns that appeared, keeps the cards that stayed, and waits for a drag', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1'), ruledRow('s2')] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, withRules(board));
    const kept = cardFor(columnNamed(host, 'building'), 's1');
    daemon.columns = { building: [ruledRow('s1')], stray: [ruledRow('s4')], '': [ruledRow('s2')] };
    kept.listeners.dragstart[0]({ dataTransfer: { setData() {} } });
    assert.equal(boardBusy(), true);
    await refreshBoard(host);
    assert.equal(daemon.urls.length, 0, 'nothing is read while a card is in hand');
    kept.listeners.dragend[0]({});
    await flushColumnLoads();
    await flushColumnLoads();
    assert.deepEqual(host.querySelectorAll('.board-column').map(column => column.dataset.column), ['building', 'review', 'stray', '']);
    assert.equal(cardFor(columnNamed(host, 'building'), 's1'), kept);
    assert.equal(kept.detached || 0, 0, 'a card that stays is never taken out of the tree');
    assert.deepEqual(cardIds(columnNamed(host, '')), ['s2']);
    // Two requests while one is in flight are one more read, not two.
    daemon.urls.length = 0;
    await Promise.all([refreshBoard(host), refreshBoard(host), refreshBoard(host)]);
    assert.equal(daemon.urls.filter(url => url.includes('view=rail')).length, 2);
  });
});

test('arrow keys move between cards and columns, and the board is one tab stop', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1'), ruledRow('s2')], review: [], done: [ruledRow('s3')] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, board);
    const [s1, s2] = columnNamed(host, 'building').querySelectorAll('.board-card');
    const s3 = cardFor(columnNamed(host, 'done'), 's3');
    assert.deepEqual([s1, s2, s3].map(card => card.tabIndex), [0, -1, -1]);
    const press = (card, key) => { let stopped = false; host.listeners.keydown[0]({ key, target: card, preventDefault() { stopped = true; } }); return stopped; };
    assert.equal(press(s1, 'ArrowDown'), true);
    assert.equal(doc.activeElement, s2);
    host.listeners.focusin[0]({ target: s2 });
    assert.deepEqual([s1, s2, s3].map(card => card.tabIndex), [-1, 0, -1]);
    press(s2, 'ArrowRight');
    assert.equal(doc.activeElement, s3, 'an empty column is passed over, and the row is clamped');
    press(s3, 'ArrowLeft');
    assert.equal(doc.activeElement, s1);
    assert.equal(press(s1, 'ArrowUp'), false, 'at the top an arrow is left to the browser');
  }, ['building', 'review', 'done']);
});

test('a column a refresh adds reads under the board\'s query, whatever is typed in the bar', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1')] });
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const host = await ruledBoard(daemon, withRules(board));
    daemon.columns = { building: [ruledRow('s1')], stray: [ruledRow('s2')] };
    organization.barQuery = 'tag:phase=plan';
    try {
      await refreshBoard(host);
      assert.equal(daemon.urls.length, 4, 'the rail read, the kept column, the empty declared one and the new one');
      for (const url of daemon.urls) assert.match(url, /&query=tag%3Aflow&group_by=tag-key%3Aflow&sort=longest&board_view=view_board$/);
      assert.deepEqual(cardIds(columnNamed(host, 'stray')), ['s2'], 'the new column\'s first read was waited for');
    } finally { organization.barQuery = ''; }
  });
});

test('a board whose refresh cannot be read says so in each column, and Pin here returns focus to its card', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1', { placed_by: 'rule' })] });
  return withBoardColumnFetch(url => (daemon.down ? { ok: false, status: 503, text: async () => 'tags cannot be read right now\n', json: async () => ({}) } : daemon.respond(url)),
  async (doc, board) => {
    const host = await ruledBoard(daemon, withRules(board));
    const card = cardFor(columnNamed(host, 'building'), 's1');
    await pinCard(card, async () => {});
    assert.equal(doc.activeElement, card);
    daemon.down = true;
    await refreshBoard(host);
    assert.match(columnNamed(host, 'building').querySelector('.board-empty').textContent, /^Could not load this column: tags cannot be read right now/);
    assert.equal(cardFor(columnNamed(host, 'building'), 's1'), card, 'the cards it had stay');
  });
});

// "Needs you" (board-in-flight plan §2.2). The status reader stands in for
// the session surface's: it answers a rendered status per session id.
function statusReader(states) {
  return (runtime, id) => states[id] || { execution: 'unknown', indicator: { kind: 'none' }, ask: { count: 0, unseen: false } };
}
const waiting = { execution: 'waiting', indicator: { kind: 'none' }, ask: { count: 0, unseen: false } };
const running = { execution: 'running', indicator: { kind: 'running' }, ask: { count: 0, unseen: false } };
const approval = { execution: 'running', indicator: { kind: 'approval' }, ask: { count: 0, unseen: false } };
async function needsBoard(daemon, board, states) {
  const groups = Object.entries(daemon.columns).filter(([, rows]) => rows.length).map(([key, rows]) => ({ key, total: rows.length }));
  const host = renderBoardNodes(board, boardConfig(board), groups, { status: statusReader(states) });
  globalThis.document.body.appendChild(host);
  const control = boardNeedsControl(host);
  globalThis.document.body.appendChild(control);
  await flushColumnLoads();
  return { host, control };
}
const countOn = control => control.querySelector('.board-needs-count').textContent;
const shownIds = host => host.querySelectorAll('.board-card').filter(card => !host.dataset.needsOnly || card.dataset.needs).map(card => card.dataset.sessionId);

test('Needs you counts the cards waiting on the reader as soon as the columns load, and follows the status feed', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1'), ruledRow('s2')], review: [ruledRow('s3')] });
  const states = { s1: waiting, s2: running, s3: approval };
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const { host, control } = await needsBoard(daemon, board, states);
    assert.equal(countOn(control), '2', 'no status event was needed');
    assert.equal(control.disabled, false);
    assert.equal(control.attributes['aria-pressed'], 'false');
    states.s1 = running;
    daemon.urls.length = 0;
    paintBoardAttention(host);
    assert.equal(countOn(control), '1');
    assert.equal(daemon.urls.length, 0, 'a repaint reads nothing');
    states.s3 = running;
    paintBoardAttention(host);
    assert.equal(countOn(control), '');
    assert.equal(control.disabled, true, 'released with nothing waiting, it is off');
  });
});

test('pressed, Needs you leaves only the waiting cards, keeps the tab stop and arrows on them, and can always be released', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1'), ruledRow('s2'), ruledRow('s3')], review: [ruledRow('s4')], done: [ruledRow('s5')] });
  const states = { s1: running, s2: waiting, s3: running, s4: running, s5: approval };
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const { host, control } = await needsBoard(daemon, board, states);
    const card = id => host.querySelectorAll('.board-card').find(each => each.dataset.sessionId === id);
    assert.equal(card('s1').tabIndex, 0);
    openColumnMenu(card('s1'), () => {});
    control.click();
    assert.equal(control.attributes['aria-pressed'], 'true');
    assert.deepEqual(shownIds(host), ['s2', 's5']);
    assert.equal(host.querySelectorAll('.board-menu').length, 0, 'a menu for a card no longer drawn is closed');
    assert.deepEqual(host.querySelectorAll('.board-card').map(each => each.tabIndex), [-1, 0, -1, -1, -1], 'the tab stop is on a drawn card');
    assert.equal(host.querySelectorAll('.board-column').map(column => countOf(column)).join(','), '3,1,1', 'columns keep their full counts');
    // Arrows pass over what is not drawn: right from s2 skips review (s4 is hidden).
    host.listeners.keydown[0]({ key: 'ArrowRight', target: card('s2'), preventDefault() {} });
    assert.equal(doc.activeElement, card('s5'));
    host.listeners.keydown[0]({ key: 'ArrowDown', target: card('s2'), preventDefault() {} });
    assert.equal(doc.activeElement, card('s5'), 's3 below is hidden, so nothing moves');
    // The focused card stops needing the reader: focus goes to another drawn card.
    states.s5 = running;
    paintBoardAttention(host);
    assert.equal(doc.activeElement, card('s2'));
    // Nothing is left: the control stays pressed and usable.
    states.s2 = running;
    paintBoardAttention(host);
    assert.deepEqual(shownIds(host), []);
    assert.equal(doc.activeElement, control);
    assert.equal(control.disabled, false);
    control.click();
    assert.equal(shownIds(host).length, 5);
    assert.equal(control.disabled, false, 'it holds focus, so it is not switched off under the reader');
    doc.activeElement = null;
    paintBoardAttention(host);
    assert.equal(control.disabled, true);
    assert.equal(card('s1').detached || 0, 0, 'no card was rebuilt');
  }, ['building', 'review', 'done']);
});

test('the pressed state belongs to the view and outlives the board that was replaced', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1'), ruledRow('s2')] });
  const states = { s1: waiting };
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const first = await needsBoard(daemon, board, states);
    first.control.click();
    first.host.remove();
    const again = await needsBoard(daemon, board, states);
    assert.equal(again.host.dataset.needsOnly, '1');
    assert.equal(again.control.attributes['aria-pressed'], 'true');
    assert.deepEqual(shownIds(again.host), ['s1']);
    again.control.click(); // leave the next test a released view
    const other = await needsBoard(daemon, { ...board, id: 'view_other' }, states);
    assert.equal(other.host.dataset.needsOnly, '');
  });
});

test('with the filter on, a column that cannot load still says so', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1')] });
  let down = false;
  return withBoardColumnFetch(url => (down ? { ok: false, status: 503, text: async () => 'tags cannot be read right now\n', json: async () => ({}) } : daemon.respond(url)),
  async (doc, board) => {
    const { host, control } = await needsBoard(daemon, board, { s1: waiting });
    control.click();
    down = true;
    await refreshBoard(host);
    const line = columnNamed(host, 'building').querySelector('.board-failed');
    assert.match(line.textContent, /^Could not load this column/);
    assert.equal(line.classes.has('board-empty'), true);
    control.click();
  });
});

test('under the filter a move, a release and an emptied column keep the number, the tab stop and focus right', () => {
  const daemon = ruledDaemon({ building: [ruledRow('s1'), ruledRow('s2'), ruledRow('s3')], review: [], stray: [ruledRow('s9')] });
  const states = { s1: waiting, s2: waiting, s3: waiting, s9: approval };
  return withBoardColumnFetch(daemon.respond, async (doc, board) => {
    const { host, control } = await needsBoard(daemon, withRules(board), states);
    const card = id => host.querySelectorAll('.board-card').find(each => each.dataset.sessionId === id);
    control.click();
    assert.equal(countOn(control), '4');
    // A move: the card lands in review, still waiting, still drawn, focus on it.
    daemon.columns = { building: [ruledRow('s2'), ruledRow('s3')], review: [ruledRow('s1', { placed_by: 'owner' })], stray: [ruledRow('s9')] };
    await moveCard(card('s1'), 'review', async () => {});
    assert.deepEqual(shownIds(host), ['s2', 's3', 's1', 's9']);
    assert.equal(countOn(control), '4');
    assert.equal(doc.activeElement, card('s1'));
    // The observed column empties and goes: its card is no longer counted.
    daemon.columns.stray = [];
    await refreshBoard(host);
    assert.equal(columnNamed(host, 'stray'), undefined);
    assert.equal(countOn(control), '3');
    // The focused card, second of two drawn in its column, stops waiting while a
    // menu for it holds focus: the menu closes and focus goes on, not to the page.
    card('s2').focus();
    openColumnMenu(card('s2'), () => {});
    assert.equal(doc.activeElement.textContent, 'review');
    states.s2 = running;
    paintBoardAttention(host);
    assert.equal(host.querySelectorAll('.board-menu').length, 0);
    assert.equal(doc.activeElement, card('s3'), 'the next drawn card below, not the top of the column');
    assert.equal(card('s3').tabIndex, 0);
    control.click();
  });
});

test('released and holding focus, Needs you is not switched off under the reader', () => withDocument(async doc => {
  const states = { s1: waiting };
  const view = { id: 'view_focus', name: 'Flow', board: {} };
  const host = renderBoardNodes(view, { groupKey: 'flow', columns: ['building'] }, [], { status: statusReader(states) });
  doc.body.appendChild(host);
  const control = boardNeedsControl(host);
  doc.body.appendChild(control);
  const card = doc.createElement('button');
  card.className = 'board-card';
  Object.assign(card.dataset, { runtime: 'runtime-a', sessionId: 's1' });
  host.querySelector('.board-cards').appendChild(card);
  paintBoardAttention(host);
  assert.equal(countOn(control), '1');
  control.focus();
  states.s1 = running;
  paintBoardAttention(host);
  assert.equal(countOn(control), '');
  assert.equal(control.disabled, false, 'focus is never left on a dead control');
  doc.activeElement = null;
  paintBoardAttention(host);
  assert.equal(control.disabled, true);
}));

// --- opening a card's session in its desktop app (board-native-open plan) ---

const linkedRow = { native_open: { url: 'app-a://threads/s1', app: 'App A' } };
const pointer = (host, type, target, relatedTarget = null) => { for (const fn of host.listeners[type] || []) fn({ target, relatedTarget, preventDefault() {} }); };

test('a linked card offers the open item first in its menu; an unlinked card offers what it always did', () => withDocument(doc => {
  setNativeOpenEnabled(true);
  const { host, card } = boardWithCard(doc, 'flow');
  assert.equal(cardOpenItem(card), null, 'a card whose row had no link has no item');
  openColumnMenu(card, () => {});
  assert.deepEqual(host.querySelector('.board-menu').querySelectorAll('button').map(b => b.textContent), ['review']);
  host.querySelector('.board-menu').closeMenu();
  rememberCardLink(card, linkedRow);
  openColumnMenu(card, () => assert.fail('opening must not move the card'));
  const menu = host.querySelector('.board-menu');
  assert.equal(menu.getAttribute('aria-label'), 'Card actions');
  assert.deepEqual(menu.querySelectorAll('button').map(b => b.textContent), ['Open in App A', 'review']);
  const win = { location: { href: 'console' } };
  const priorWindow = globalThis.window;
  globalThis.window = win;
  try { menu.querySelectorAll('button')[0].click(); } finally { globalThis.window = priorWindow; }
  assert.equal(win.location.href, 'app-a://threads/s1');
  assert.deepEqual(doc.events, [], 'the session was not opened in the console');
  assert.equal(host.querySelectorAll('.board-menu').length, 0, 'choosing closes the menu');
  assert.equal(doc.activeElement, card, 'the board did not change, so focus returns to the card');
  // A refresh whose row lost the link takes the item away; the switch takes
  // the item and the hover control away.
  rememberCardLink(card, {});
  assert.equal(cardOpenItem(card), null);
  rememberCardLink(card, linkedRow);
  try {
    setNativeOpenEnabled(false);
    assert.equal(cardOpenItem(card), null);
    assert.equal(showCardOpen(host, card), null);
    pointer(host, 'pointerover', card);
    assert.equal(host.querySelectorAll('.board-card-open').length, 0);
  } finally { setNativeOpenEnabled(true); }
}));

test('a card with nothing to offer keeps the browser\'s own right-click menu', () => withDocument(doc => {
  const host = renderBoardNodes({ name: 'One', board: {} }, { groupKey: 'flow', columns: ['only'] }, []);
  const card = doc.createElement('button');
  card.className = 'board-card';
  host.querySelector('.board-cards').appendChild(card);
  let prevented = 0;
  for (const fn of host.listeners.contextmenu) fn({ target: card, preventDefault: () => { prevented++; } });
  assert.equal(prevented, 0);
  assert.equal(host.querySelectorAll('.board-menu').length, 0, 'no empty menu');
}));

test('right-click on a card opens its menu; right-click elsewhere on the board is left alone', () => withDocument(doc => {
  const { host, card } = boardWithCard(doc, 'flow');
  let prevented = 0;
  for (const fn of host.listeners.contextmenu) fn({ target: host.querySelector('.board-column'), preventDefault: () => { prevented++; } });
  assert.equal(host.querySelectorAll('.board-menu').length, 0);
  assert.equal(prevented, 0);
  for (const fn of host.listeners.contextmenu) fn({ target: card, preventDefault: () => { prevented++; } });
  assert.equal(prevented, 1);
  assert.equal(host.querySelector('.board-menu').forCard, card);
  host.querySelector('.board-menu').closeMenu();
}));

test('the hover control sits on the board, never in the card, one at a time, and goes when the pointer leaves', () => withDocument(doc => {
  setNativeOpenEnabled(true);
  const { host, card } = boardWithCard(doc, 'flow');
  doc.body.appendChild(host);
  const other = doc.createElement('button');
  other.className = 'board-card';
  host.querySelectorAll('.board-cards')[1].appendChild(other);
  pointer(host, 'pointerover', other);
  assert.equal(host.querySelectorAll('.board-card-open').length, 0, 'a card without a link shows nothing');
  rememberCardLink(card, linkedRow);
  rememberCardLink(other, linkedRow);
  pointer(host, 'pointerover', card);
  const control = host.querySelector('.board-card-open');
  assert.equal(control.forCard, card);
  assert.equal(control.parentNode, host, 'the control is the board\'s child');
  assert.equal(card.querySelectorAll('.board-card-open').length, 0, 'nothing clickable inside the card (PO-17)');
  assert.equal(control.tabIndex, -1, 'the board keeps one tab stop');
  assert.equal(control.draggable, false, 'a drag from the corner never drags the link');
  assert.equal(control.textContent, '\u2197', 'a small mark, so the title stays the card\'s');
  assert.equal(control.getAttribute('aria-label'), 'Open in App A');
  assert.equal(control.href, 'app-a://threads/s1');
  pointer(host, 'pointerover', card);
  assert.equal(host.querySelectorAll('.board-card-open').length, 1, 'hovering again does not stack');
  pointer(host, 'pointerout', card, control);
  assert.equal(host.querySelector('.board-card-open'), control, 'moving from the card onto the control keeps it');
  pointer(host, 'pointerout', control, card);
  assert.equal(host.querySelector('.board-card-open'), control, 'and back');
  pointer(host, 'pointerover', other);
  assert.equal(host.querySelectorAll('.board-card-open').length, 1);
  assert.equal(host.querySelector('.board-card-open').forCard, other, 'the control follows the pointer');
  pointer(host, 'pointerout', other, host);
  assert.equal(host.querySelectorAll('.board-card-open').length, 0, 'leaving the card takes it away');
  // A drag, a scroll and a right-click each take it away; so does its card leaving.
  for (const type of ['dragstart', 'scroll', 'contextmenu']) { // right-click: the menu replaces it
    showCardOpen(host, card);
    pointer(host, type, card);
    assert.equal(host.querySelectorAll('.board-card-open').length, 0, type);
    host.querySelector('.board-menu')?.closeMenu();
  }
  // A touch has no hover: a control under the finger would take the card's tap.
  for (const fn of host.listeners.pointerover) fn({ target: card, pointerType: 'touch' });
  assert.equal(host.querySelectorAll('.board-card-open').length, 0);
  // After the board changes under it the control follows its card, or goes
  // when the card is no longer drawn.
  const placed = showCardOpen(host, card);
  placed.style.top = 'stale';
  settleCardOpen(host, () => true);
  assert.notEqual(placed.style.top, 'stale', 'put back over its card');
  settleCardOpen(host, shown => shown !== card);
  assert.equal(host.querySelectorAll('.board-card-open').length, 0, 'a hidden card keeps no control');
  showCardOpen(host, card);
  card.remove();
  settleCardOpen(host, () => true);
  assert.equal(host.querySelectorAll('.board-card-open').length, 0, 'a card that left keeps no control');
  showCardOpen(host, other);
  host.querySelectorAll('.board-cards')[0].appendChild(card);
  showCardOpen(host, card);
  card.remove();
  pointer(host, 'pointerover', other);
  assert.equal(host.querySelector('.board-card-open').forCard, other, 'a control whose card left the board is replaced');
  // Clicking the control hands off without opening the session in the console.
  const win = { location: { href: 'console' } };
  const priorWindow = globalThis.window;
  globalThis.window = win;
  try {
    const fresh = showCardOpen(host, other);
    for (const fn of fresh.listeners.click) fn({ preventDefault() {}, stopPropagation() {} });
  } finally { globalThis.window = priorWindow; }
  assert.deepEqual(doc.events, []);
}));
