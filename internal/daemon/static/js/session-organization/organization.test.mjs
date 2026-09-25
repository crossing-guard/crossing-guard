import test from 'node:test';
import assert from 'node:assert/strict';
import { organization, activeQuery, adoptViews, isStructuredQuery, selectView } from './organization-state.js';
import { organizedRailUrl, organizedPageUrl, railStateKey, groupsAreRepositories, groupLabel } from './view-group-source.js';
import { tagLabel, parseTagText, sameTag, tagClass, tagTitle, isOwnerTag, rowTime } from './tag-chips.js';
import { isTypingTarget, shortcutMatches } from './shortcut-match.js';

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
  assert.equal(organizedPageUrl(active, { key: 'walmart', mode: 'all', offset: 15, limit: 15, selected: { runtime: 'claude', id: 'a b' } }),
    '/api/sessions?view=group&group=walmart&mode=all&offset=15&limit=15&query=mine%3Afollow-up&group_by=tag-key%3Atopic&sort=longest&selected_runtime=claude&selected_id=a%20b');
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
  for (const text of ['tag:approved', '-tag:vcs=commit', 'repricer repo:oms', 'title:"due diligence"']) assert.equal(isStructuredQuery(text), true, text);
  for (const text of ['', 'repricer margin', 'what about: this', '12:30 standup', 'a -b']) assert.equal(isStructuredQuery(text), false, text);
});

test('group memory is kept apart per view and per grouping', () => {
  const a = { scope: 'view:1', groupBy: 'tag-key:topic' }, b = { scope: 'view:2', groupBy: 'tag-key:topic' };
  assert.notEqual(railStateKey(a, 'walmart'), railStateKey(b, 'walmart'));
  assert.notEqual(railStateKey(a, 'walmart'), 'walmart');
});

test('a tag group is named as the owner spelled it, and the group without the tag says which tag', () => {
  const active = { groupBy: 'tag-key:topic' };
  assert.equal(groupLabel(active, { key: 'amazon-routing', label: 'Amazon-Routing' }), 'Amazon-Routing');
  assert.equal(groupLabel(active, { key: 'walmart' }), 'walmart');
  assert.equal(groupLabel(active, { key: '' }), 'no topic');
  assert.equal(groupLabel({ groupBy: 'none' }, { key: '' }), 'all');
  assert.equal(groupLabel({ groupBy: '' }, { key: '/work/oms' }), '');
});

test('what the owner types is the tag: first colon separates, nothing else does', () => {
  assert.deepEqual(parseTagText('approved'), { value: 'approved' });
  assert.deepEqual(parseTagText('  topic : amazon-routing '), { key: 'topic', value: 'amazon-routing' });
  assert.deepEqual(parseTagText(':odd'), { value: ':odd' });
  assert.equal(tagLabel({ key: 'topic', value: 'walmart' }), 'topic:walmart');
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
  const vocabulary = [{ value: 'approved', sessions: 3 }, { key: 'topic', value: 'Amazon-Routing', sessions: 2 }];
  assert.deepEqual(tagChoices('', vocabulary), { typed: { value: '' }, known: vocabulary, isNew: false });
  const typing = tagChoices('amaz', vocabulary);
  assert.equal(typing.isNew, true);
  assert.deepEqual(typing.known.map(tag => tag.value), ['Amazon-Routing']);
  assert.equal(tagChoices('APPROVED', vocabulary).isNew, false, 'another letter case is the same tag, not a new one');
  assert.equal(tagChoices('topic:amazon-routing', vocabulary).isNew, false);
  assert.deepEqual(tagChoices('topic:repricer', vocabulary).typed, { key: 'topic', value: 'repricer' });
  assert.equal(tagChoices('anything', []).isNew, true, 'with no tags yet, whatever is typed can be created');
});

test('header toggles are the most recent tags this session does not already carry — never a shipped list', () => {
  const vocabulary = [{ value: 'approved' }, { value: 'follow-up' }, { key: 'topic', value: 'walmart' }, { value: 'dropped' }, { value: 'later' }];
  const carried = [{ value: 'Approved', provenance: 'user-asserted' }, { value: 'follow-up', provenance: 'model-claimed' }];
  assert.deepEqual(recentToggles(vocabulary, carried, 3).map(tag => tag.value), ['follow-up', 'walmart', 'dropped'],
    'an agent\'s tag of the same name does not stop the owner applying his own');
  assert.deepEqual(recentToggles([], carried, 3), [], 'nothing is offered before the owner has tagged anything');
  assert.deepEqual(recentToggles(vocabulary, [], 0), []);
});
