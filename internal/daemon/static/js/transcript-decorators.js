// Transcript row-decorator seam — orchestration-agents-gui-design §4.
//
// The transcript row builder in views/sessions.js is one switch shared by three
// surfaces (session detail, live task overlay, chat). Editing case blocks in
// place risks a three-surface regression per per-turn feature, forever. This
// registry is the one framework change that pays that debt: after a row is
// built, appendTranscriptEvents calls applyTranscriptDecorators exactly once,
// and every registered decorator gets a chance to annotate the row. The next
// per-turn feature is additive — register a decorator, never touch the switch.
//
// A decorator is `fn(ev, rowEl, ctx)` where:
//   ev    — the canonical transcript event the row was built from
//   rowEl — the DOM element the switch appended for this event
//   ctx   — { kind, projectRoot, resolveReference } supplied by the renderer
// Decorators must be defensive (a throw is contained, never breaks the
// transcript) and must only ADD to the row — they never replace its content.

const decorators = [];

// registerTranscriptDecorator adds a decorator with a stable order. Lower
// orders run first; ties keep registration order (stable sort).
export function registerTranscriptDecorator(fn, order = 0) {
  if (typeof fn !== 'function') throw new Error('transcript decorator must be a function');
  decorators.push({ fn, order });
  decorators.sort((a, b) => a.order - b.order);
}

// applyTranscriptDecorators runs every decorator once for a freshly built row.
// A row is decorated at most once: the merged tool_result path in the renderer
// re-touches an existing chip, and running the chain again would duplicate
// chips/badges on the same element.
export function applyTranscriptDecorators(ev, rowEl, ctx = {}) {
  if (!ev || !rowEl) return;
  if (rowEl.dataset) {
    if (rowEl.dataset.cgDecorated === '1') return;
    rowEl.dataset.cgDecorated = '1';
  }
  for (const entry of decorators) {
    try { entry.fn(ev, rowEl, ctx); } catch { /* a decorator must never break the transcript */ }
  }
}

// registeredTranscriptDecorators reports the current chain (diagnostics/tests).
export function registeredTranscriptDecorators() {
  return decorators.map(entry => entry.fn);
}

// clearTranscriptDecorators empties the registry. Test seam only — production
// code registers at module import and never unregisters.
export function clearTranscriptDecorators() {
  decorators.length = 0;
}
