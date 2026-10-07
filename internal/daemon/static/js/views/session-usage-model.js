// Pure figures for the Usage pane (session usage breakdown plan §5.4): no DOM,
// so the rules are unit-tested. A usage "kind" is the route's totals object
// for main, subagent or agent work.

export const WORK_KINDS = ['main', 'subagent', 'agent'];
export const MEASURES = ['all', 'input', 'output', 'reasoning', 'calls'];

// measureOf reads one measure from a kind's totals. "all" is the Usage page's
// Total (every stated token); reasoning is null when no call stated it.
export function measureOf(kind, measure) {
  if (!kind) return 0;
  switch (measure) {
    case 'input': return (kind.input_tokens || 0) + (kind.cache_read || 0) + (kind.cache_create || 0);
    case 'output': return kind.output_tokens || 0;
    case 'reasoning': return kind.reasoning_tokens ?? null;
    case 'calls': return kind.calls || 0;
    default: return kind.total || 0;
  }
}

// splitShares turns one value per kind into shares of their sum, skipping
// kinds with nothing (a null value is unknown and counts as nothing).
export function splitShares(values) {
  const total = WORK_KINDS.reduce((sum, kind) => sum + (values[kind] || 0), 0);
  return WORK_KINDS.filter(kind => (values[kind] || 0) > 0)
    .map(kind => ({ kind, value: values[kind], share: total ? values[kind] / total : 0 }));
}

// statedNote says "stated by X of Y" only when some call did not state it.
export function statedNote(stated, calls) {
  return stated >= calls ? '' : 'stated by ' + stated.toLocaleString() + ' of ' + calls.toLocaleString();
}

// gapSegments splits sorted times into runs whose gaps are at most gapMs.
export function gapSegments(times, gapMs) {
  const sorted = [...times].filter(Number.isFinite).sort((a, b) => a - b);
  if (!sorted.length) return [];
  const segments = [[sorted[0], sorted[0]]];
  for (const time of sorted.slice(1)) {
    const last = segments[segments.length - 1];
    if (time - last[1] > gapMs) segments.push([time, time]);
    else last[1] = time;
  }
  return segments;
}

// timeScale maps a time onto [x0, x0 + width] with each segment's span in
// proportion (a minute at least) and breakPx between segments. breaks are the
// x positions of the gaps.
export function timeScale(segments, x0, width, breakPx) {
  const spanOf = ([start, end]) => Math.max(end - start, 60000);
  const total = segments.reduce((sum, segment) => sum + spanOf(segment), 0) || 1;
  // Gap marks never take more than 30% of the plot, however many gaps there are.
  if (segments.length > 1) breakPx = Math.min(breakPx, width * 0.3 / (segments.length - 1));
  const perMs = Math.max(0, width - breakPx * Math.max(segments.length - 1, 0)) / total;
  const starts = [];
  let x = x0;
  for (const segment of segments) { starts.push(x); x += spanOf(segment) * perMs + breakPx; }
  const breaks = starts.slice(1).map(start => start - breakPx / 2);
  const at = time => {
    for (let index = segments.length - 1; index >= 0; index--) {
      if (time >= segments[index][0]) return starts[index] + Math.min(time - segments[index][0], spanOf(segments[index])) * perMs;
    }
    return x0;
  };
  return { at, breaks, starts };
}

// memberTree orders members for the tree: each top-level member by first call,
// followed by its children, recursively. A member whose parent is not a member
// is top level; level is its depth in the tree.
export function memberTree(members) {
  const ids = new Set(members.map(member => member.id));
  const children = new Map();
  const tops = [];
  for (const member of members) {
    if (member.parent && ids.has(member.parent) && member.parent !== member.id) {
      if (!children.has(member.parent)) children.set(member.parent, []);
      children.get(member.parent).push(member);
    } else tops.push(member);
  }
  const byFirst = (a, b) => String(a.first_at || '').localeCompare(String(b.first_at || ''));
  const rows = [];
  const seen = new Set();
  const visit = (member, level) => {
    if (seen.has(member.id)) return;
    seen.add(member.id);
    const kids = (children.get(member.id) || []).sort(byFirst);
    rows.push({ member, level, children: kids.length });
    for (const kid of kids) visit(kid, level + 1);
  };
  for (const member of tops.sort(byFirst)) visit(member, 0);
  return rows;
}

// sumKinds adds members' totals into one kind-shaped total.
export function sumKinds(members) {
  const out = { calls: 0, input_tokens: 0, cache_read: 0, cache_create: 0, output_tokens: 0, total: 0,
    reasoning_stated_calls: 0, reasoning_tokens: null, peak_context: 0, first_at: '', last_at: '' };
  for (const member of members) {
    for (const key of ['calls', 'input_tokens', 'cache_read', 'cache_create', 'output_tokens', 'total', 'reasoning_stated_calls']) {
      out[key] += member[key] || 0;
    }
    if (member.reasoning_tokens != null) out.reasoning_tokens = (out.reasoning_tokens || 0) + member.reasoning_tokens;
    out.peak_context = Math.max(out.peak_context, member.peak_context || 0);
    if (member.first_at && (!out.first_at || member.first_at < out.first_at)) out.first_at = member.first_at;
    if (member.last_at && member.last_at > out.last_at) out.last_at = member.last_at;
  }
  const input = out.input_tokens + out.cache_read + out.cache_create;
  out.cache_hit_rate = input ? out.cache_read / input : null;
  return out;
}

// groupByRole folds members by their stated role, largest total first; an
// empty role reads "no role stated".
export function groupByRole(members) {
  const groups = new Map();
  for (const member of members) {
    const role = member.role || 'no role stated';
    if (!groups.has(role)) groups.set(role, []);
    groups.get(role).push(member);
  }
  return [...groups.entries()].map(([role, list]) => ({ role, count: list.length, totals: sumKinds(list) }))
    .sort((a, b) => b.totals.total - a.totals.total);
}

// depthLabel is a member's stated depth, or "not stated": never a guessed
// level (D-5). The route omits a depth the runtime did not state.
export function depthLabel(member) {
  return Number.isInteger(member?.depth) && member.depth > 0 ? String(member.depth) : 'not stated';
}

// activeSpan is the time between a member's first and last call, in ms.
export function activeSpan(member) {
  const first = Date.parse(member.first_at), last = Date.parse(member.last_at);
  return Number.isFinite(first) && Number.isFinite(last) ? Math.max(0, last - first) : null;
}
