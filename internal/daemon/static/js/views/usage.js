import { $, el, api, fmtTime, fmtTok, fmtCost, fmtCostBasis } from "../core.js";
import { mkLoader, withState } from "../ui.js";
import { revealPane } from "../pane-host.js";
import { openSession } from "./sessions.js";
import { renderUsageSplit, WORK_LABELS } from "./usage-strip.js";

/* ---------- usage dashboard ----------
 * Every figure is a sum of the model calls the usage recorder stored
 * (token-usage-analytics plan §3.7): GET /api/usage. A class is "stated by N of
 * M calls" when not every call stated it; a session whose source the vendor
 * removed still counts and is listed by runtime and id. */
async function renderUsage(container) {
  const main = container || $('#main');
  main.innerHTML = '<h2>Usage</h2><div class="sub">Observed vendor telemetry aggregated from local session files — nothing estimated, nothing phoned home.</div>';
  const wrap = el('div');
  main.appendChild(wrap);
  await withState(wrap, mkLoader('Aggregating usage across all sessions…'), () => api('/api/usage'), report => {
    if (report.coverage?.state === 'unavailable') {
      wrap.appendChild(el('div', 'sub', 'usage unavailable'));
      return;
    }
    wrap.append(...[usageCoverageLine(report.coverage), usageTiles(report), usageRuntimeCard(report), usageTypeCard(report),
      ...usageDayCharts(report),
      usageTopTable('Largest context (last call) — handoff candidates', report.top_context, 'Context', s => fmtTok(s.context)),
      usageTopTable('Heaviest sessions (total tokens)', report.top_total, 'Total', s => fmtTok(s.total), true)].filter(Boolean));
  });
}

// usageCoverageLine states how much of the discovered sources the figures
// cover, as data: nothing when every source is read.
function usageCoverageLine(coverage) {
  const line = el('div', 'sub usage-coverage');
  const agents = usageAgentsLine(coverage);
  if (!coverage || (coverage.state === 'current' && !agents)) return line;
  const parts = coverage.state === 'current' ? [agents]
    : [coverage.state, coverage.sources_recorded + ' of ' + coverage.sources_discovered + ' sources read'].concat(agents || []);
  if (coverage.sources_failed) parts.push(coverage.sources_failed + ' unreadable');
  if (coverage.recorder_error) parts.push('not recording');
  line.textContent = parts.join(' · ');
  return line;
}

// usageAgentsLine states, as data, when the split into agents' work is not whole.
function usageAgentsLine(coverage) {
  return coverage?.agents === 'unavailable' ? 'agents unavailable' : coverage?.agents === 'incomplete' ? 'agents incomplete' : '';
}

// percentOf renders a ratio the API states, or a dash when it is undefined.
function percentOf(ratio) {
  return ratio == null ? '—' : Math.round(ratio * 100) + '%';
}

function usageTile(value, label, detail, split) {
  const tile = el('div', 'tile');
  tile.append(el('div', 'v', value), el('div', 'k', label));
  if (detail) tile.append(el('div', 's', detail));
  if (split) tile.append(split);
  return tile;
}

// workSplit is one measure's split of the report's totals by whose work it is;
// nothing when the split is withheld.
function workSplit(report, pick) {
  if (!report.work) return null;
  const values = {};
  for (const kind of Object.keys(WORK_LABELS)) values[kind] = report.work[kind] ? pick(report.work[kind]) : 0;
  return renderUsageSplit(values);
}

const inputOf = u => u.input_tokens + u.cache_read + u.cache_create;

// statedOf reads "stated by N of M calls" only when some call did not state it.
function statedOf(stated, calls) {
  return stated === calls ? '' : 'stated by ' + stated.toLocaleString() + ' of ' + calls.toLocaleString() + ' calls';
}

function usageTiles(report) {
  const totals = report.totals;
  const tiles = el('div', 'tiles');
  const reasoning = totals.reasoning_tokens != null
    ? 'includes ' + fmtTok(totals.reasoning_tokens) + ' reasoning' +
      (statedOf(totals.reasoning_stated_calls, totals.calls) ? ' (' + statedOf(totals.reasoning_stated_calls, totals.calls) + ')' : '')
    : 'reasoning not stated';
  tiles.append(
    usageTile(fmtTok(totals.input_tokens + totals.cache_read + totals.cache_create), 'input tokens',
      fmtTok(totals.input_tokens) + ' fresh · ' + fmtTok(totals.cache_create) + ' cache-write', workSplit(report, inputOf)),
    usageTile(fmtTok(totals.output_tokens), 'output tokens', reasoning, workSplit(report, u => u.output_tokens)),
    usageTile(percentOf(totals.cache_hit_rate), 'cache hit rate', 'cache reads / all input'),
    usageTile(totals.calls.toLocaleString(), 'model calls', report.work
      ? report.sessions + ' sessions' + (report.no_data ? ' · ' + report.no_data + ' without telemetry' : '') : '',
      workSplit(report, u => u.calls)),
  );
  // Cost only as the runtimes stated it: one line per unit and basis, with how
  // many calls stated any. Runtime-stated cost is not a bill.
  const coverage = report.cost_coverage || { with_cost: 0, without_cost: 0 };
  for (const line of report.cost || []) {
    tiles.append(usageTile(fmtCost(line), 'cost · ' + fmtCostBasis(line), 'stated by ' +
      coverage.with_cost.toLocaleString() + ' of ' + (coverage.with_cost + coverage.without_cost).toLocaleString() + ' calls'));
  }
  if (!(report.cost || []).length) tiles.append(usageTile('unknown', 'cost', 'no call stated a cost'));
  for (const [runtime, u] of Object.entries(report.by_runtime)) {
    tiles.append(usageTile(fmtTok(u.input_tokens + u.cache_read + u.cache_create), runtime + ' input',
      percentOf(u.cache_hit_rate) + ' cache hit · ' + u.calls.toLocaleString() + ' calls'));
  }
  return tiles;
}

// usageDayCharts draws the last 30 UTC days: stacked input classes and output.
function usageDayCharts(report) {
  const last30 = Object.keys(report.days).sort().slice(-30);
  const inputChart = el('div', 'chartcard');
  inputChart.innerHTML = '<h3>Input tokens per day</h3><div class="sub">stacked: cache read (pale) + fresh input (blue) + cache write (violet)</div>';
  const outChart = el('div', 'chartcard');
  outChart.innerHTML = '<h3>Output tokens per day</h3><div class="sub">what the models actually generated</div>';
  const inBars = el('div', 'bars'), outBars = el('div', 'bars');
  const maxIn = Math.max(1, ...last30.map(d => report.days[d].cache_read + report.days[d].input + report.days[d].cache_create));
  const maxOut = Math.max(1, ...last30.map(d => report.days[d].output));
  for (const day of last30) {
    inBars.appendChild(usageInputBar(day, report.days[day], maxIn));
    outBars.appendChild(usageOutputBar(day, report.days[day], maxOut));
  }
  const legend = el('div', 'legend');
  legend.innerHTML = '<span><span class="sw seg cr"></span>cache read</span><span><span class="sw seg in"></span>fresh input</span><span><span class="sw seg cc"></span>cache write</span>';
  inputChart.append(inBars, usageAxis(last30), legend);
  outChart.append(outBars, usageAxis(last30));
  return [inputChart, outChart];
}

function usageInputBar(date, bucket, maxIn) {
  const bar = el('div', 'day');
  bar.title = date + '\n' + fmtTok(bucket.cache_read) + ' cache read\n' + fmtTok(bucket.input) + ' fresh input\n' +
    fmtTok(bucket.cache_create) + ' cache write\n' +
    (bucket.other || []).map(item => fmtTok(item.count) + ' ' + (item.label || item.id) + '\n').join('') + bucket.calls + ' calls';
  const segment = (cls, value) => { const s = el('div', 'seg ' + cls); s.style.height = (value / maxIn * 100) + '%'; return s; };
  // order top→bottom: cache write, fresh, cache read
  bar.append(segment('cc', bucket.cache_create), segment('in', bucket.input), segment('cr', bucket.cache_read));
  bar.style.height = Math.max(2, (bucket.cache_read + bucket.input + bucket.cache_create) / maxIn * 100) + '%';
  return bar;
}

function usageOutputBar(date, bucket, maxOut) {
  const bar = el('div', 'day');
  bar.title = date + '\n' + fmtTok(bucket.output) + ' output · ' + bucket.calls + ' calls';
  const fill = el('div', 'seg out');
  fill.style.height = '100%';
  bar.appendChild(fill);
  bar.style.height = Math.max(2, bucket.output / maxOut * 100) + '%';
  return bar;
}

function usageAxis(days) {
  const axis = el('div', 'axis');
  axis.append(el('span', '', days[0] || ''), el('span', '', days[days.length - 1] || ''));
  return axis;
}

// MEASURE_PICKS read one measure from a report total.
const MEASURE_PICKS = [['all', 'All tokens', u => u.total], ['output', 'Output', u => u.output_tokens], ['calls', 'Calls', u => u.calls]];

// usageRuntimeCard shows each runtime's total split by whose work it is.
function usageRuntimeCard(report) {
  if (!report.work_by_runtime) return null;
  const card = el('div', 'chartcard');
  card.appendChild(el('h3', '', 'By runtime'));
  const chips = el('div', 'usage-chips');
  const rows = el('div', 'usage-runtime-rows');
  const draw = pick => {
    const entries = Object.entries(report.work_by_runtime).map(([runtime, kinds]) => {
      const values = {};
      for (const kind of Object.keys(WORK_LABELS)) values[kind] = kinds[kind] ? pick(kinds[kind]) : 0;
      return [runtime, values, Object.values(values).reduce((sum, value) => sum + value, 0)];
    }).sort((a, b) => b[2] - a[2]);
    const largest = Math.max(1, ...entries.map(entry => entry[2]));
    rows.replaceChildren(...entries.map(([runtime, values, total]) => usageRuntimeRow(runtime, values, total, largest)));
  };
  for (const [id, label, pick] of MEASURE_PICKS) {
    const chip = el('button', 'chip' + (id === 'all' ? ' on' : ''), label);
    chip.type = 'button';
    chip.addEventListener('click', () => { for (const other of chips.children) other.classList.toggle('on', other === chip); draw(pick); });
    chips.appendChild(chip);
  }
  draw(MEASURE_PICKS[0][2]);
  card.append(chips, rows);
  return card;
}

function usageRuntimeRow(runtime, values, total, largest) {
  const row = el('div', 'usage-runtime-row');
  const track = el('div', 'usage-runtime-track');
  const split = renderUsageSplit(values, { tall: true });
  split.style.width = Math.max(2, total / largest * 100) + '%';
  track.appendChild(split);
  const shares = ['subagent', 'agent'].filter(kind => values[kind] > 0)
    .map(kind => WORK_LABELS[kind].toLowerCase() + ' ' + Math.round(values[kind] / total * 100) + '%');
  row.append(el('span', 'chip ' + runtime, runtime), track, el('span', 'sub', [fmtTok(total)].concat(shares).join(' · ')));
  return row;
}

// usageTypeCard lists subagent and agent types across sessions: members are
// distinct subagents or agents, sessions the root sessions they worked for.
function usageTypeCard(report) {
  if (!(report.by_type || []).length) return null;
  const card = el('div', 'chartcard');
  card.appendChild(el('h3', '', 'Subagents and agents'));
  const table = el('table', 'usage-type-table');
  table.innerHTML = '<tr><th>Type</th><th>Runtime</th><th>Members</th><th>Sessions</th><th>Calls</th><th>Input</th><th>Output</th><th>Reasoning / call</th></tr>';
  for (const row of report.by_type) {
    const tr = el('tr');
    const name = el('td', '', row.type);
    name.appendChild(el('span', 'badge work-' + row.kind, row.kind === 'agent' ? 'agent' : 'subagent'));
    const perCall = row.reasoning_stated_calls ? fmtTok(Math.round((row.reasoning_tokens || 0) / row.reasoning_stated_calls)) : '—';
    const stated = statedOf(row.reasoning_stated_calls, row.calls);
    tr.append(name, el('td', '', row.runtime), el('td', '', String(row.members)), el('td', '', String(row.roots)),
      el('td', '', row.calls.toLocaleString()), el('td', '', fmtTok(inputOf(row))), el('td', '', fmtTok(row.output_tokens)),
      el('td', '', perCall + (row.reasoning_stated_calls && stated ? ' · ' + stated : '')));
    table.appendChild(tr);
  }
  card.appendChild(table);
  if (report.by_type_omitted) card.appendChild(el('div', 'sub', report.by_type_omitted + ' more types'));
  return card;
}

// usageTopTable lists root sessions; one whose source is gone shows its
// runtime and full id and does not open (plan D-8). With split, each row shows
// its own, its subagents' and its agents' work.
function usageTopTable(title, rows, valueLabel, valueFormat, split) {
  if (!(rows || []).length) return null;
  const card = el('div', 'chartcard');
  card.innerHTML = '<h3>' + title + '</h3>';
  const table = el('table');
  table.innerHTML = '<tr><th>Session</th><th>Runtime</th><th>' + valueLabel + '</th>' +
    (split ? '<th>Split</th><th>Subagents</th><th>Agents</th>' : '') + '<th>Cache hit</th><th>When</th></tr>';
  for (const session of rows) table.appendChild(usageTopRow(session, valueFormat, split));
  card.appendChild(table);
  return card;
}

// openSessionUsage opens a session and reveals its Usage pane, un-hiding a
// hidden workspace (red-team R-5). The view switch has no completion signal,
// so the handoff waits the way every other cross-view open does; the pane
// reveal is chained on openSession itself.
function openSessionUsage(session) {
  document.querySelector('nav button[data-view="sessions"]').click();
  setTimeout(() => {
    void Promise.resolve(openSession(session, null)).then(() => revealPane('session.usage', { kind: 'all' })).catch(() => {});
  }, 300);
}

function usageTopRow(session, valueFormat, split) {
  const row = el('tr');
  const name = el('td', '', session.source_present ? (session.title || session.id) : session.id);
  if (session.source_present) {
    name.style.cursor = 'pointer';
    name.style.color = 'var(--accent2)';
    name.onclick = () => openSessionUsage(session);
  } else {
    name.title = 'the session file is no longer on disk';
  }
  const runtime = el('td');
  runtime.appendChild(el('span', 'chip ' + session.runtime, session.runtime));
  row.append(name, runtime, el('td', '', valueFormat(session)));
  if (split) {
    const bar = el('td', 'usage-top-split');
    bar.appendChild(renderUsageSplit({ main: session.main?.total, subagent: session.subagent?.total, agent: session.agent?.total }));
    row.append(bar, el('td', '', session.subagents ? String(session.subagents) : '–'),
      el('td', '', (session.agent_profiles || []).join(', ') || (session.agents ? String(session.agents) : '–')));
  }
  row.append(el('td', '', percentOf(session.hit_rate)), el('td', '', fmtTime(session.modified)));
  return row;
}

export { renderUsage, usageTile, statedOf };
