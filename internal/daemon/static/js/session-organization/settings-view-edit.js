// A view's edit form (session-views-rebuild plan §3.4). One write model:
// nothing is written until Save, and Cancel puts everything back. The form
// edits a draft; the entry module sends it. The daemon alone reads a filter:
// the match line under the field is its answer, never a guess made here.
import { el, api } from '../core.js';
import { organizedRailUrl, FILTER_TYPING_PAUSE_MS } from './view-group-source.js';
import { groupByChoices, recoverySuffix } from './view-actions.js';
import { queryNotesNode } from './query-bar.js';
import { NEW_DRAFT, SORTS, draftOf, showAs, saveState, baseState, baseView, draftChanged, rebase, asNewDraft,
  groupChoiceSets, isMemoryView } from './settings-view-model.js';
import { boardEditor, loadPlacementReads, showRuleCounts } from './settings-view-board.js';
import { button, linkButton, card, field, problem, selectBox, textInput, textArea, spacer, sequence, whileBusy } from '../orchestration/agents/agent-ui.js';

const TERMS = [
  ['tag:approved', 'sessions with that tag, whoever applied it'],
  ['tag:phase=plan', 'a tag under a key · tag:topic=order* globs'],
  ['mine:follow-up', 'only tags you applied'],
  ['-tag:vcs=commit', 'exclude'],
  ['repo:app · runtime: · branch:', 'where the session ran'],
  ['title:"sprint retro" · note:', 'words in its title or in your note'],
  ['touched:>14d · tagged:>5d', 'age of last activity / of your tag'],
  ['calls:>5 · lines:<20', 'how much a session did: model calls / transcript lines'],
  ['status:running', 'live sessions only — such a view shows no count'],
  ['plain words', 'transcript text, inside what the terms left'],
];

export function renderEdit(page, body, key) {
  const form = { page, body, key, draft: page.draft(key), stale: '', refused: '', match: null, matched: null,
    bar: el('div', 'views-savebar'), termsOpen: key === NEW_DRAFT, ticket: sequence(), timer: 0 };
  form.stale = baseState(form.draft, page.stored(key));
  form.blocked = page.blocked();
  page.watchAdopt(() => adopted(form));
  page.watchVocabulary(() => refillGroups(form));
  paint(form, '');
  // What each rule places now is read for the saved view and written into
  // the form where it stands.
  const stored = page.stored(key);
  if (stored?.board && (stored.board.placement || []).length) {
    loadPlacementReads([stored]).then(() => { if (body.isConnected) showRuleCounts(body, form.draft.view); }).catch(() => {});
  }
}

// The tag keys can arrive after the form is drawn: the group list is refilled
// then, unless the owner is in it.
function refillGroups(form) {
  const old = form.body.querySelector('[data-focus="group"]');
  if (old && document.activeElement !== old) old.replaceWith(groupSelect(form, form.ctx));
}

// paint draws the whole form. It runs when the form opens and after a change
// to its structure (how the view is shown, a column or rule added, moved or
// removed), never while a field is being typed in.
function paint(form, focus) {
  const { page, draft, key } = form;
  const blocked = page.blocked();
  const memory = isMemoryView(draft.view);
  // An edit answers the last refusal: its sentence and its outlined rule go.
  const changed = () => { form.refused = ''; refreshBar(form); markRefusedRule(form); };
  const ctx = { blocked, changed, redraw: target => paint(form, target) };
  form.ctx = ctx;
  form.blocked = blocked;
  const back = key === NEW_DRAFT || !page.stored(key) ? { screen: 'index' } : { screen: 'view', id: key };
  const parts = [
    linkButton(back.screen === 'index' ? '‹ Session views' : '‹ ' + (baseView(draft)?.name || 'View'), () => page.navigate(back)),
    el('h2', 'agents-title', key === NEW_DRAFT ? 'New view' : 'Edit view'),
    card('', nameField(form, ctx), filterField(form, ctx, memory), termsTable(form)),
    displayCard(form, ctx, memory),
  ];
  if (draft.shownAs === 'board' && !memory) parts.push(card('Board', ...boardEditor(draft, ctx)));
  form.body.replaceChildren(...parts, form.bar);
  refreshBar(form);
  markRefusedRule(form);
  if (focus) focusIn(form.body, focus);
}

function focusIn(body, target) {
  const node = body.querySelector('[data-focus="' + target + '"]');
  if (!node) return;
  (node.matches('input, select, textarea, button') ? node : node.querySelector('input, select'))?.focus();
}

function nameField(form, ctx) {
  const input = textInput(form.draft.view.name, '');
  input.maxLength = 120;
  input.disabled = ctx.blocked;
  input.dataset.focus = 'name';
  input.oninput = () => { form.draft.view.name = input.value; ctx.changed(); };
  return field('Name', input);
}

function filterField(form, ctx, memory) {
  const input = textArea(form.draft.view.query, '', 2);
  input.className = 'views-mono';
  input.disabled = ctx.blocked;
  input.dataset.focus = 'filter';
  input.spellcheck = false;
  form.match = el('div', 'views-match');
  input.oninput = () => { form.draft.view.query = input.value; ctx.changed(); readMatch(form, memory, FILTER_TYPING_PAUSE_MS); };
  const node = field('Filter', input);
  node.appendChild(form.match);
  if (form.matched && form.matched.query === form.draft.view.query) showMatch(form);
  else readMatch(form, memory, 0);
  return node;
}

// readMatch asks the daemon what the filter being typed matches, after a
// pause. A memory view lists no sessions, so it has no line.
function readMatch(form, memory, pause) {
  clearTimeout(form.timer);
  const query = form.draft.view.query || '';
  const current = form.ticket();
  form.matched = null;
  if (memory || !query.trim()) { form.match.replaceChildren(); return; }
  form.timer = setTimeout(async () => {
    let matched;
    try {
      const read = await api(organizedRailUrl({ query, groupBy: 'none' }, 1));
      matched = { query, total: Number.isInteger(read.match_total) ? read.match_total : null, notes: read.query_notes || [] };
    } catch (error) {
      matched = { query, refused: String(error?.message || error) };
    }
    if (!current() || !form.body.isConnected) return;
    form.matched = matched;
    showMatch(form);
  }, pause);
}

function showMatch(form) {
  const { matched, match } = form;
  if (matched.refused) { match.replaceChildren(el('div', 'views-match-bad', matched.refused)); return; }
  const words = matched.total === null ? 'Not counted'
    : 'Matches ' + matched.total + (matched.total === 1 ? ' session' : ' sessions') + ' now';
  match.replaceChildren(el('div', matched.total === null ? 'agents-sub' : 'views-match-ok', words));
  const notes = queryNotesNode(matched.notes);
  if (notes) match.appendChild(notes);
}

function termsTable(form) {
  const node = el('details', 'views-terms');
  node.open = form.termsOpen;
  node.ontoggle = () => { form.termsOpen = node.open; };
  node.appendChild(el('summary', '', 'Filter terms'));
  const table = el('table');
  for (const [term, meaning] of TERMS) {
    const tr = el('tr');
    const td = el('td');
    td.appendChild(el('code', '', term));
    tr.append(td, el('td', '', meaning));
    table.appendChild(tr);
  }
  node.append(table, el('div', 'agents-sub', 'Every term is required; repeating a field means either. The full table lives in the how-to (organize-sessions).'));
  return node;
}

// How the view is shown: a list with its grouping, or a board. A memory view
// lists records that have no board, so it offers the sort alone.
function displayCard(form, ctx, memory) {
  const { draft } = form;
  const sort = selectBox(SORTS, draft.view.sort || 'newest', '');
  sort.disabled = ctx.blocked;
  sort.onchange = () => { draft.view.sort = sort.value; ctx.changed(); };
  const board = draft.shownAs === 'board' && !memory;
  const fields = el('div', 'views-two');
  if (!memory) fields.appendChild(shownAsField(form, ctx));
  if (!board) fields.appendChild(field('Group by', groupSelect(form, ctx)));
  fields.appendChild(field(board ? 'Sort cards' : 'Sort', sort));
  return card('Display', fields);
}

function shownAsField(form, ctx) {
  const seg = el('div', 'views-seg');
  for (const [kind, label] of [['list', 'List'], ['board', 'Board']]) {
    const node = button(label, form.draft.shownAs === kind ? 'active' : '', () => { showAs(form.draft, kind); ctx.redraw('shown-' + kind); });
    node.dataset.focus = 'shown-' + kind;
    node.setAttribute('aria-pressed', String(form.draft.shownAs === kind));
    node.disabled = ctx.blocked;
    seg.appendChild(node);
  }
  const wrap = el('div', 'agents-field');
  wrap.append(el('span', 'agents-field-label', 'Shown as'), seg);
  return wrap;
}

// The group list: the mechanical groupings, then the tag keys the owner
// applies, then every other key in use. The stored and the chosen value are
// always offered in their own spelling (groupByChoices).
function groupSelect(form, ctx) {
  const { draft, page } = form;
  const choices = groupByChoices(page.vocabulary.known, baseView(draft)?.group_by, draft.listGroupBy);
  const sets = groupChoiceSets(choices, page.vocabulary.tags);
  const select = el('select');
  select.disabled = ctx.blocked;
  select.dataset.focus = 'group';
  const add = (parent, list) => {
    for (const [value, label] of list) {
      const option = el('option', '', label);
      option.value = value;
      parent.appendChild(option);
    }
  };
  add(select, sets.basic);
  for (const [label, list] of [['Your tag keys', sets.yours], ['Other tag keys', sets.other]]) {
    if (!list.length) continue;
    const group = el('optgroup');
    group.label = label;
    add(group, list);
    select.appendChild(group);
  }
  select.value = draft.listGroupBy;
  select.onchange = () => { draft.listGroupBy = select.value; ctx.changed(); };
  return select;
}

// refreshBar keeps the save bar truthful as the draft is edited. It redraws
// the bar alone, so it can run while a field has focus.
function refreshBar(form) {
  const { page, draft, key, bar } = form;
  bar.replaceChildren();
  if (form.refused) bar.appendChild(problem(form.refused));
  if (form.stale) { staleChoices(form); return; }
  const state = saveState(draft, baseView(draft), page.blocked());
  const cancel = button('Cancel', '', () => discard(form));
  const save = button(key === NEW_DRAFT ? 'Create view' : 'Save', 'primary');
  save.disabled = !state.ready;
  save.onclick = () => whileBusy([save, cancel], () => submit(form));
  const line = el('div', 'views-savebar-row');
  line.append(el('span', 'views-savebar-state' + (state.ready ? ' dirty' : ''), state.words), spacer(), cancel, save);
  bar.appendChild(line);
}

// The view changed or was deleted elsewhere since the draft was made: nothing
// is sent until the owner says which edit stands.
function staleChoices(form) {
  const { page, draft, key, bar } = form;
  const gone = form.stale === 'gone';
  // One unfinished new view at a time: this edit cannot become a second.
  const taken = gone && page.drafts.has(NEW_DRAFT);
  const keep = gone
    ? button('Save as a new view', 'primary', () => {
      page.drafts.delete(key);
      page.drafts.set(NEW_DRAFT, asNewDraft(draft));
      page.navigate({ screen: 'new' }, 'name');
    })
    : button('Reload and keep my edit', 'primary', () => {
      form.draft = rebase(draft, page.stored(key));
      page.drafts.set(key, form.draft);
      form.stale = '';
      paint(form, 'name');
    });
  keep.disabled = taken;
  const line = el('div', 'views-savebar-row');
  const words = gone ? 'This view was deleted elsewhere.' : 'This view changed since you started editing.';
  line.append(el('span', 'views-savebar-state bad', taken ? words + ' Finish or cancel the new view you started before saving this as another.' : words),
    spacer(), button('Discard my edit', '', () => discard(form)), keep);
  bar.appendChild(line);
}

function discard(form) {
  const { page, key } = form;
  page.drafts.delete(key);
  page.navigate(key !== NEW_DRAFT && page.stored(key) ? { screen: 'view', id: key } : { screen: 'index' });
}

async function submit(form) {
  const { page, key } = form;
  form.refused = '';
  const result = await page.save(key);
  if (result.ok) { page.navigate(result.id ? { screen: 'view', id: result.id } : { screen: 'index' }); return; }
  if (result.stale) form.stale = result.stale;
  else if (!result.status) form.refused = 'The save did not reach the daemon: ' + result.message;
  else form.refused = result.message + recoverySuffix(result.status);
  refreshBar(form);
  markRefusedRule(form);
}

// A refused placement rule is named by its number in the daemon's sentence;
// the row it names is outlined.
function markRefusedRule(form) {
  for (const node of form.body.querySelectorAll('.views-item-bad')) node.classList.remove('views-item-bad');
  const named = /placement rule (\d+)/.exec(form.refused || '');
  if (named) form.body.querySelector('[data-focus="rule-' + (Number(named[1]) - 1) + '"]')?.classList.add('views-item-bad');
}

// adopted runs when the views were adopted while the form is shown. A draft
// the owner has not changed is remade from the stored view; a changed one is
// kept, and the bar says the view moved under it.
function adopted(form) {
  const { page, key, draft } = form;
  const stored = page.stored(key);
  const state = baseState(draft, stored);
  // The file may have gained or lost an unreadable entry: saving turns off
  // or on with it, and the form's fields with it.
  if (!state && page.blocked() !== form.blocked) paint(form, document.activeElement?.closest?.('[data-focus]')?.dataset.focus || '');
  if (!state) return;
  if (!draftChanged(draft)) {
    page.drafts.delete(key);
    if (!stored) { page.navigate({ screen: 'index' }); return; }
    form.draft = draftOf(stored);
    page.drafts.set(key, form.draft);
    paint(form, document.activeElement?.closest?.('[data-focus]')?.dataset.focus || '');
    return;
  }
  form.stale = state;
  refreshBar(form);
}
