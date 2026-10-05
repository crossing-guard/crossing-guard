// The Usage pane (session usage breakdown plan §5.4): a session's own calls,
// its subagents' and its agents', from GET /api/session/usage. A pane of its
// own like Plan; as an evidence pane it gets the shared header and footer. The
// footer's usage values open it on one kind.
import { el, api, fmtTok } from '../core.js';
import { provider } from '../infopanel.js';
import { renderUsageSplit, WORK_LABELS } from './usage-strip.js';
import { MEASURES, activeSpan, depthLabel, gapSegments, groupByRole, measureOf, memberTree, splitShares, statedNote, timeScale } from './session-usage-model.js';

const SVG_NS = 'http://www.w3.org/2000/svg';
const MEASURE_LABELS = { all: 'All tokens', input: 'Input', output: 'Output', reasoning: 'Reasoning', calls: 'Calls' };
const MEASURE_UNITS = { all: 'tokens', input: 'input', output: 'output', reasoning: 'reasoning', calls: 'calls' };

// usageDefaults is daemon.json's usage section, read once.
let usageDefaultsRequest = null;
function usageDefaults() {
  usageDefaultsRequest ||= api('/api/console/config').then(body => body?.config?.usage || {}).catch(() => ({}));
  return usageDefaultsRequest;
}

const fmtMeasure = (value, measure) => value == null ? '—' : measure === 'calls' ? value.toLocaleString() : fmtTok(value);
const fmtSpan = ms => ms == null ? '–' : ms < 3600000 ? Math.round(ms / 60000) + ' min' : (ms / 3600000).toFixed(1) + ' h';
const fmtClock = value => new Date(value).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const fmtDay = value => new Date(value).toLocaleDateString([], { month: 'numeric', day: 'numeric' });
const shortID = id => id.length > 12 ? id.slice(0, 10) + '…' : id;
const percent = ratio => ratio == null ? '–' : Math.round(ratio * 100) + '%';

function kindsOf(data) {
  return { main: data.main, subagent: data.subagent, agent: data.agent };
}

// memberName is how a member reads: its stated description, else its id
// (the part that tells members apart) and its role.
function memberName(member) {
  return member.description || (shortID(member.id) + (member.role ? ' · ' + member.role : ''));
}

// usageChips is one chip group; picking a chip re-renders through onPick.
function usageChips(options, current, onPick) {
  const group = el('div', 'usage-chips');
  for (const [value, label] of options) {
    const chip = el('button', 'chip' + (value === current ? ' on' : ''), label);
    chip.type = 'button';
    chip.addEventListener('click', () => onPick(value));
    group.appendChild(chip);
  }
  return group;
}

// coverageLine states, as data, what the figures do not yet cover.
function coverageLine(data) {
  const coverage = data.coverage || {};
  const parts = [];
  if (coverage.state && coverage.state !== 'current') {
    parts.push(coverage.state + ' · ' + coverage.sources_recorded + ' of ' + coverage.sources_discovered + ' sources read');
  }
  if (coverage.agents === 'unavailable') parts.push('agents unavailable');
  if (coverage.agents === 'incomplete') parts.push('agents incomplete');
  if (data.main?.series_unavailable) parts.push('context series unavailable');
  return parts.length ? el('div', 'sub usage-coverage', parts.join(' · ')) : null;
}

function kindDetail(data, kind) {
  if (kind === 'main') return data.main.calls ? data.main.calls.toLocaleString() + ' calls · ' + (data.main.models || []).join(', ') : 'no calls recorded';
  if (kind === 'subagent') {
    const nested = data.subagents.filter(member => member.parent).length;
    return data.subagents.length + (data.subagents_omitted ? '+' + data.subagents_omitted : '') + (nested ? ' · ' + nested + ' nested' : '');
  }
  const profiles = [...new Set(data.agents.flatMap(agent => agent.profiles || []))];
  return data.agents.length + (data.agents_omitted ? '+' + data.agents_omitted : '') + (profiles.length ? ' · ' + profiles.join(', ') : '');
}

// headline is the total of the three kinds with its split beside it (D-4).
function headline(data, state) {
  const kinds = kindsOf(data);
  const values = {};
  for (const kind of Object.keys(kinds)) values[kind] = measureOf(kinds[kind], state.measure);
  const total = Object.values(values).reduce((sum, value) => sum + (value || 0), 0);
  const box = el('div', 'usage-headline');
  const value = el('div', 'evidence-value', fmtMeasure(total, state.measure));
  value.appendChild(el('small', '', MEASURE_UNITS[state.measure] + (data.as_of ? ' · as of ' + fmtClock(data.as_of) : '')));
  box.append(value, renderUsageSplit(values, { tall: true, format: v => fmtMeasure(v, state.measure) }));
  const lines = el('div', 'usage-kinds');
  for (const share of splitShares(values)) {
    const line = el('button', 'usage-kind work-' + share.kind);
    line.type = 'button';
    line.addEventListener('click', () => state.focusKind?.(share.kind));
    line.append(el('span', 'usage-kind-name', WORK_LABELS[share.kind]), el('span', 'usage-kind-detail', kindDetail(data, share.kind)),
      el('span', 'usage-kind-value', fmtMeasure(share.value, state.measure) + ' · ' + Math.round(share.share * 100) + '%'));
    lines.appendChild(line);
  }
  box.appendChild(lines);
  return box;
}

function svgNode(tag, attrs, text) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [key, value] of Object.entries(attrs || {})) node.setAttribute(key, String(value));
  if (text != null) node.textContent = text;
  return node;
}

function timelineTimes(data) {
  const times = (data.main.series || []).map(point => point.at_ms);
  for (const member of data.subagents) times.push(Date.parse(member.first_at), Date.parse(member.last_at));
  for (const agent of data.agents) times.push(...(agent.call_times || []));
  return times.filter(Number.isFinite);
}

// timelineAxis labels each segment's start, dated where the day changes, and
// marks each collapsed gap.
function timelineAxis(svg, segments, scale, height) {
  let day = '';
  let clearFrom = -Infinity;
  segments.forEach(([start], index) => {
    const date = fmtDay(start);
    const text = (date !== day ? date + ' ' : '') + fmtClock(start);
    if (scale.starts[index] < clearFrom) return; // a label never overlaps the one before it
    svg.appendChild(svgNode('text', { x: scale.starts[index], y: 10, class: 'usage-tl-label' }, text));
    clearFrom = scale.starts[index] + text.length * 5.5 + 6;
    day = date;
  });
  for (const x of scale.breaks) {
    svg.appendChild(svgNode('path', { d: `M${x - 3} 14 l6 5 l-6 5 l6 5`, class: 'usage-tl-break' }));
    svg.appendChild(svgNode('line', { x1: x, x2: x, y1: 32, y2: height - 2, class: 'usage-tl-gap' }));
  }
}

function timelineMain(svg, data, scale, layout) {
  const series = data.main.series || [];
  const peak = Math.max(1, ...series.map(point => point.context));
  const y = context => layout.mainTop + layout.mainHeight - context / peak * (layout.mainHeight - 4);
  svg.appendChild(svgNode('text', { x: 0, y: layout.mainTop + layout.mainHeight / 2, class: 'usage-tl-name' }, 'Main'));
  svg.appendChild(svgNode('line', { x1: layout.label, x2: layout.label + layout.plot, y1: layout.mainTop + layout.mainHeight,
    y2: layout.mainTop + layout.mainHeight, class: 'usage-tl-axis' }));
  let path = '';
  let previous = null;
  for (const point of series) {
    const open = previous != null && point.at_ms - previous <= layout.gapMs;
    path += (open ? 'L' : 'M') + scale.at(point.at_ms).toFixed(1) + ' ' + y(point.context).toFixed(1) + ' ';
    previous = point.at_ms;
  }
  if (path) svg.appendChild(svgNode('path', { d: path, class: 'usage-tl-main' }));
}

function timelineMembers(svg, data, scale, layout) {
  const rows = memberTree(data.subagents);
  const heaviest = Math.max(1, ...data.subagents.map(member => member.total || 0));
  rows.forEach((row, index) => {
    const y = layout.rowsTop + index * layout.row;
    const name = memberName(row.member);
    svg.appendChild(svgNode('text', { x: row.level * 10, y: y + 9, class: 'usage-tl-member' + (row.level ? ' nested' : '') },
      name.length > layout.chars ? name.slice(0, layout.chars - 1) + '…' : name));
    const x0 = scale.at(Date.parse(row.member.first_at)), x1 = Math.max(scale.at(Date.parse(row.member.last_at)), x0 + 3);
    const bar = svgNode('rect', { x: x0.toFixed(1), y: y + 2, width: (x1 - x0).toFixed(1), height: 8, rx: 2, class: 'usage-tl-bar',
      'fill-opacity': (0.35 + 0.65 * (row.member.total || 0) / heaviest).toFixed(2) });
    bar.appendChild(svgNode('title', {}, name + ' · ' + row.member.calls + ' calls · ' + fmtTok(row.member.total) + ' tokens'));
    svg.appendChild(bar);
  });
  return rows.length;
}

function timelineAgents(svg, data, scale, layout, top) {
  data.agents.forEach((agent, index) => {
    const y = top + index * layout.row;
    svg.appendChild(svgNode('text', { x: 0, y: y + 9, class: 'usage-tl-member' }, (agent.profiles || [agent.role || agent.id]).join(', ')));
    for (const at of agent.call_times || []) {
      svg.appendChild(svgNode('rect', { x: scale.at(at).toFixed(1), y: y + 1, width: 2, height: 10, class: 'usage-tl-tick' }));
    }
  });
}

// timeline plots the main lane's context per call, a bar per subagent and a
// tick per agent call, with idle gaps collapsed (D-8).
function timeline(data, gapMinutes, width) {
  const times = timelineTimes(data);
  if (!times.length) return null;
  const layout = { label: width >= 700 ? 210 : 130, mainTop: 20, mainHeight: 44, row: 12, gapMs: gapMinutes * 60000 };
  layout.plot = width - layout.label - 12;
  layout.chars = Math.floor(layout.label / 6.5);
  layout.rowsTop = layout.mainTop + layout.mainHeight + 8;
  const segments = gapSegments(times, layout.gapMs);
  const scale = timeScale(segments, layout.label, layout.plot, 12);
  const height = layout.rowsTop + (data.subagents.length + data.agents.length) * layout.row + 8;
  const svg = svgNode('svg', { viewBox: `0 0 ${width} ${height}`, class: 'usage-timeline', role: 'img' });
  timelineAxis(svg, segments, scale, height);
  timelineMain(svg, data, scale, layout);
  const drawn = timelineMembers(svg, data, scale, layout);
  timelineAgents(svg, data, scale, layout, layout.rowsTop + drawn * layout.row);
  return svg;
}

function reasoningCell(totals) {
  if (!totals.reasoning_stated_calls) return el('td', 'dim', 'not stated');
  const note = statedNote(totals.reasoning_stated_calls, totals.calls);
  const cell = el('td', '', fmtTok(totals.reasoning_tokens));
  if (note) cell.appendChild(el('span', 'usage-stated', note));
  return cell;
}

// usageRow is one table row: calls, tokens and output always; the rest as
// columns in a wide pane, or behind the row's expansion in a narrow one.
function usageRow(name, totals, options = {}) {
  const row = el('tr', 'usage-row' + (options.level ? ' usage-level-' + Math.min(options.level, 3) : ''));
  const nameCell = el('td', 'usage-name');
  nameCell.append(...[].concat(name));
  row.append(nameCell, el('td', '', (totals.calls || 0).toLocaleString()), el('td', '', fmtTok(totals.total || 0)),
    el('td', '', fmtTok(totals.output_tokens || 0)));
  const extra = [el('td', '', fmtTok((totals.input_tokens || 0) + (totals.cache_read || 0) + (totals.cache_create || 0))),
    el('td', '', percent(totals.cache_hit_rate ?? null)), reasoningCell(totals),
    el('td', '', totals.peak_context ? fmtTok(totals.peak_context) : '–'), el('td', '', fmtSpan(activeSpan(totals)))];
  for (const cell of extra) cell.classList.add('usage-wide-only');
  row.append(...extra);
  return row;
}

// detailRow repeats the wide-only figures for a narrow pane, hidden until its
// row is clicked. A row's expansion is the drill-down floor (R-13).
function detailRow(totals, model, depth) {
  const row = el('tr', 'usage-detail');
  row.hidden = true;
  const cell = el('td', 'usage-detail-cell');
  cell.colSpan = 4;
  const facts = [['input', fmtTok((totals.input_tokens || 0) + (totals.cache_read || 0) + (totals.cache_create || 0))],
    ['cache hit', percent(totals.cache_hit_rate ?? null)],
    ['reasoning', totals.reasoning_stated_calls ? fmtTok(totals.reasoning_tokens) + (statedNote(totals.reasoning_stated_calls, totals.calls) ? ' · ' + statedNote(totals.reasoning_stated_calls, totals.calls) : '') : 'not stated'],
    ['peak context', totals.peak_context ? fmtTok(totals.peak_context) : '–'], ['active', fmtSpan(activeSpan(totals))],
    ['model', model || (totals.models || []).join(', ') || '–']];
  if (depth) facts.push(['depth', depth]);
  const grid = el('div', 'usage-detail-grid');
  for (const [label, value] of facts) grid.append(el('span', 'usage-detail-k', label), el('span', 'usage-detail-v', value));
  cell.appendChild(grid);
  row.appendChild(cell);
  return row;
}

function appendRow(body, name, totals, options = {}) {
  const row = usageRow(name, totals, options);
  const detail = detailRow(totals, options.model, options.depth);
  row.addEventListener('click', event => {
    if (event.target.closest('.usage-twist')) return;
    detail.hidden = !detail.hidden;
  });
  body.append(row, detail);
  return row;
}

function groupHeader(kind, label, count) {
  const row = el('tr', 'usage-group work-' + kind);
  row.dataset.usageGroup = kind;
  const cell = el('td', '', '');
  cell.colSpan = 9;
  cell.append(el('span', 'usage-group-name', label), el('span', 'dim', ' ' + count));
  row.appendChild(cell);
  return row;
}

function memberLabel(member, extra) {
  const parts = [el('span', '', memberName(member))];
  if (member.role && member.description) parts.push(el('span', 'badge', member.role));
  if (extra) parts.push(extra);
  return parts;
}

function subagentTree(body, data, open) {
  const rows = memberTree(data.subagents);
  const kidRows = new Map();
  for (const row of rows) {
    const twist = row.children ? el('button', 'usage-twist', '▸') : null;
    const depthNote = !row.level && row.member.depth > 1 && !row.member.parent ? el('span', 'dim', ' depth ' + row.member.depth) : null;
    const label = memberLabel(row.member, depthNote);
    if (twist) { twist.type = 'button'; label.unshift(twist); }
    const tr = appendRow(body, label, row.member, { level: row.level, depth: depthLabel(row.member) });
    if (row.level) { tr.hidden = !open.has(row.member.parent); kidRows.get(row.member.parent)?.push(tr); }
    if (twist) {
      kidRows.set(row.member.id, []);
      if (open.has(row.member.id)) twist.textContent = '▾';
      twist.addEventListener('click', () => {
        const kids = kidRows.get(row.member.id) || [];
        const show = kids.some(kid => kid.hidden);
        for (const kid of kids) kid.hidden = !show;
        twist.textContent = show ? '▾' : '▸';
        if (show) open.add(row.member.id); else open.delete(row.member.id);
      });
    }
  }
}

function subagentTypes(body, data) {
  for (const group of groupByRole(data.subagents)) {
    appendRow(body, [el('span', '', group.role), el('span', 'dim', ' × ' + group.count)], group.totals, { level: 1 });
  }
}

// usageTable is who did the work: main, then subagents as a tree or by type,
// then agents (D-5).
function usageTable(data, state) {
  const table = el('table', 'usage-table');
  const head = el('tr');
  for (const [label, wide] of [['Name'], ['Calls'], ['Tokens'], ['Output'], ['Input', 1], ['Cache hit', 1], ['Reasoning', 1], ['Peak ctx', 1], ['Active', 1]]) {
    head.appendChild(el('th', wide ? 'usage-wide-only' : '', label));
  }
  const body = el('tbody');
  table.append(el('thead'), body);
  table.tHead.appendChild(head);
  appendRow(body, [el('span', 'usage-group-name work-main', WORK_LABELS.main)], data.main, { model: (data.main.models || []).join(', ') });
  if (data.subagents.length) {
    body.appendChild(groupHeader('subagent', WORK_LABELS.subagent, data.subagents.length + (data.subagents_omitted ? '+' + data.subagents_omitted : '')));
    if (state.grouping === 'type') subagentTypes(body, data); else subagentTree(body, data, state.open ||= new Set());
  }
  if (data.agents.length) {
    body.appendChild(groupHeader('agent', WORK_LABELS.agent, data.agents.length + (data.agents_omitted ? '+' + data.agents_omitted : '')));
    for (const agent of data.agents) {
      const label = [el('span', '', (agent.profiles || []).join(', ') || agent.id), el('span', 'badge', agent.role || 'agent')];
      appendRow(body, label, agent, { level: 1 });
    }
  }
  return table;
}

function emptyState(data) {
  const box = el('div', 'usage-empty');
  if (data.coverage?.state === 'unavailable') box.textContent = 'Usage unavailable';
  else box.textContent = 'No calls recorded';
  return box;
}

// drawUsagePane renders the pane from one response and the pane's state.
function drawUsagePane(box, data, state, defaults) {
  state.drawnHidden = !box.clientWidth;
  const width = Math.max(320, box.clientWidth || 520);
  box.classList.toggle('usage-wide', width >= (defaults.wide_columns_px || 720));
  const parts = [coverageLine(data)];
  const hasCalls = data.main.calls || data.subagent.calls || data.agent?.calls;
  if (!hasCalls) {
    box.replaceChildren(...parts.filter(Boolean), emptyState(data));
    return;
  }
  const redraw = () => drawUsagePane(box, data, state, defaults);
  parts.push(usageChips(MEASURES.map(value => [value, MEASURE_LABELS[value]]), state.measure, value => { state.measure = value; redraw(); }));
  parts.push(headline(data, state));
  parts.push(el('div', 'evidence-sectionhead', 'Timeline'), timeline(data, defaults.timeline_gap_minutes || 20, width));
  const tableHead = el('div', 'usage-table-head');
  tableHead.append(el('div', 'evidence-sectionhead', 'Who did the work'),
    usageChips([['tree', 'Tree'], ['type', 'Type']], state.grouping, value => { state.grouping = value; redraw(); }));
  parts.push(tableHead, usageTable(data, state));
  box.replaceChildren(...parts.filter(Boolean));
  state.width = width;
}

// focusKind opens and scrolls to one kind's rows (the footer's drill-down).
function focusKind(box, kind) {
  const target = box.querySelector(`[data-usage-group="${kind}"]`) || (kind === 'main' ? box.querySelector('.usage-table') : null);
  target?.scrollIntoView({ block: 'start', behavior: 'smooth' });
}

// widthObservers keeps each pane box's observer out of the pane's saved
// state, which a refresh copies onto a new box (code red-team C-2).
const widthObservers = new WeakMap();

// watchWidth redraws when the box's width moves, and once it is first shown
// when it was drawn hidden (a refresh draws into a hidden staging box).
function watchWidth(box, state, redraw) {
  if (typeof ResizeObserver === 'undefined') return;
  widthObservers.get(box)?.disconnect();
  const observer = new ResizeObserver(() => {
    if (!box.isConnected) { observer.disconnect(); return; }
    const width = box.clientWidth || 0;
    if (width && (state.drawnHidden || Math.abs(width - (state.width || 0)) > 40)) redraw();
  });
  observer.observe(box);
  widthObservers.set(box, observer);
}

provider({
  id: 'session.usage', order: 8, title: 'Usage',
  pane: { capability: 'evidence', icon: '<svg viewBox="0 0 24 24"><path d="M5 20v-7M12 20V5M19 20v-10"/></svg>' },
  match: ctx => ctx.surface === 'session' && !!ctx.selection,
  render: async (ctx, box) => {
    const query = new URLSearchParams({ runtime: ctx.selection.runtime || '', id: ctx.selection.id || '' });
    const [defaults, data] = await Promise.all([usageDefaults(), api('/api/session/usage?' + query)]);
    if (!box.isConnected) return;
    const state = Object.assign({ measure: MEASURES.includes(defaults.default_measure) ? defaults.default_measure : 'all',
      grouping: defaults.default_grouping === 'type' ? 'type' : 'tree' }, box.cgState || {});
    box.cgState = state;
    state.focusKind = kind => focusKind(box, kind);
    drawUsagePane(box, data, state, defaults);
    watchWidth(box, state, () => drawUsagePane(box, data, state, defaults));
    if (state.pendingKind) { focusKind(box, state.pendingKind); state.pendingKind = null; }
  },
  activate: (_ctx, box, detail) => {
    const kind = detail?.kind || 'main';
    if (box.cgState?.focusKind) box.cgState.focusKind(kind);
    else { box.cgState = box.cgState || {}; box.cgState.pendingKind = kind; }
  },
});

