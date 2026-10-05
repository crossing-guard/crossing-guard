// The session usage strip in the evidence footer, and the split bar the Usage
// pane and the Usage page share (session usage breakdown plan §5.4). A leaf:
// it never imports session-evidence.js, so the pane module can import both.
import { el, fmtTok, fmtCost, fmtCostBasis } from '../core.js';
import { activatePane } from '../pane-host.js';

export const WORK_LABELS = { main: 'Main', subagent: 'Subagents', agent: 'Agents' };

// usageReasoningNote says how much reasoning the output includes, and on how
// many calls, when not every call stated it.
export function usageReasoningNote(u) {
  if (u.reasoning_tokens == null) return 'reasoning not stated';
  const partial = u.reasoning_stated_calls && u.reasoning_stated_calls !== u.turns
    ? ' (stated by ' + u.reasoning_stated_calls + ' of ' + u.turns + ' calls)' : '';
  return 'includes ' + fmtTok(u.reasoning_tokens) + ' reasoning' + partial;
}

// usageTotalOf is a usage object's Total: every stated token, the Usage
// page's definition (plan D-6).
export function usageTotalOf(u) {
  if (!u) return 0;
  const other = (u.other || []).reduce((sum, item) => sum + (item.count || 0), 0);
  return (u.input_tokens || 0) + (u.cache_read || 0) + (u.cache_create || 0) + (u.output_tokens || 0) + other;
}

// renderUsageSplit draws one bar with a segment per kind of work that has any;
// shares come from the values given, and each segment names its figure.
export function renderUsageSplit(values, options = {}) {
  const bar = el('div', 'usage-split' + (options.tall ? ' usage-split-tall' : ''));
  const format = options.format || fmtTok;
  const total = Object.values(values).reduce((sum, value) => sum + (value || 0), 0);
  for (const kind of ['main', 'subagent', 'agent']) {
    if (!(values[kind] > 0)) continue;
    const segment = el('span', 'usage-split-part work-' + kind);
    segment.style.width = (values[kind] / total * 100) + '%';
    segment.title = WORK_LABELS[kind] + ' ' + format(values[kind]) + ' (' + Math.round(values[kind] / total * 100) + '%)';
    bar.appendChild(segment);
  }
  return bar;
}

// usageCell is one strip value; with a kind it opens the Usage pane on it,
// the way the evidence header's values open their views.
function usageCell(value, label, title, kind) {
  const cell = el(kind ? 'button' : 'div', 'u' + (kind ? ' u-open work-' + kind : ''));
  if (kind) {
    cell.type = 'button';
    cell.addEventListener('click', () => activatePane('session.usage', { kind }));
  }
  if (title) cell.title = title;
  cell.append(el('div', 'v', value), el('div', 'k', label));
  return cell;
}

function ownCells(strip, u) {
  if (!u.turns) {
    strip.appendChild(usageCell('no calls recorded', 'main', 'this session has no recorded call of its own', 'main'));
    return;
  }
  if (u.model) strip.appendChild(usageCell(u.model, 'model'));
  strip.appendChild(usageCell(fmtTok(u.input_tokens + u.cache_read + u.cache_create), 'input', 'cumulative input incl. cache', 'main'));
  strip.appendChild(usageCell(fmtTok(u.output_tokens), 'output', usageReasoningNote(u), 'main'));
  for (const other of u.other || []) strip.appendChild(usageCell(fmtTok(other.count), other.label || other.id, other.side));
  strip.appendChild(usageCell(fmtCost(u.cost), 'cost', u.cost ? fmtCostBasis(u.cost) + ' — not a bill' : 'the runtime stated no cost'));
  if (u.cache_hit_rate > 0) strip.appendChild(usageCell(Math.round(u.cache_hit_rate * 100) + '%', 'cache hit', 'cache_read / all input — the efficiency-parity number'));
  strip.appendChild(usageCell(String(u.turns), u.turns === 1 ? 'call' : 'calls', '', 'main'));
}

// splitCells shows the session's subagents' and agents' work apart from its
// own (lineage plan §8 rule 7), each opening its rows in the Usage pane.
function splitCells(strip, u) {
  for (const [kind, part, label] of [['subagent', u.delegated, 'subagents'], ['agent', u.agents, 'agents']]) {
    if (!part) continue;
    strip.appendChild(usageCell(fmtTok(usageTotalOf(part)) + ' · ' + (part.children || 0), label,
      part.turns + ' calls · ' + fmtTok(part.output_tokens) + ' output · ' + usageReasoningNote(part), kind));
  }
  if (u.delegated || u.agents) {
    const split = renderUsageSplit({ main: usageTotalOf(u), subagent: usageTotalOf(u.delegated), agent: usageTotalOf(u.agents) });
    split.classList.add('usage-strip-split');
    strip.appendChild(split);
  }
}

// contextCell shows context occupancy; a bar only when the vendor states the window.
function contextCell(u) {
  const ctx = el('div', 'u ctxbar');
  if (u.context_window > 0) {
    const pct = Math.min(100, Math.round(u.context / u.context_window * 100));
    ctx.append(el('div', 'v', fmtTok(u.context) + ' / ' + fmtTok(u.context_window) + ' (' + pct + '%)'), el('div', 'k', 'context window'));
    const track = el('div', 'track');
    const fill = el('div', 'fill');
    fill.style.width = pct + '%';
    track.appendChild(fill); ctx.appendChild(track);
  } else {
    ctx.append(el('div', 'v', fmtTok(u.context)), el('div', 'k', 'context at last turn · window not stated by vendor'));
  }
  return ctx;
}

export function renderUsageStrip(u) {
  const strip = el('div', 'usage');
  if (u.as_of) strip.title = 'recorded as of ' + new Date(u.as_of).toLocaleString();
  ownCells(strip, u);
  splitCells(strip, u);
  if (u.turns) strip.appendChild(contextCell(u));
  return strip;
}
