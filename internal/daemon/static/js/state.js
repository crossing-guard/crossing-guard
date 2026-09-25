// Shared mutable UI state — ONLY the values written in one module and read in
// another (view routing + the two hand-offs between views). Per-view
// state stays module-local. Data state is never cached here (Go owns it).
// selRef holds a reference clicked in a transcript. It must live here, not in the
// click handler: the panel re-renders on every cg:info, and it recomputes from
// S.view/S.sel — so a ref view held only locally would be silently replaced by
// the session providers on the next event (console-info-panel-impl-plan R5).
export const S = { view: 'sessions', chatPreload: null, chatCwd: null, memoryFocus: null, sel: null, selMemory: null, selSkill: null, selGovern: null, governTab: 'capture', governSession: null, governEntity: null, selRef: null };
// governSession is the SESSION the whole Governance surface is scoped to — the
// DATA-axis object the four tabs share, so picking one in Capture keeps it selected
// in Audit, filters Approvals to its holds, and highlights the Policy rules its
// folded tags could trip. Shape: { id, runtime, title, title_source, tags:[{key,value}] }.
// Distinct from selGovern (the full capture REPORT feeding the info panel) and from
// governEntity (a file clicked inside a session — a different axis node entirely).
