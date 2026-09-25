// Agent turn decorators — the first two users of the transcript row-decorator
// seam (orchestration-agents-gui-design §4; plan §6 hand-back loop).
//
// EV-FIELD CONTRACT (designed as if the daemon supplied it; the daemon does not
// yet emit these on transcript events, so a client-side join fills in below):
//
//   ev.agent_review: [{ agent, verdict, run_id, claim_ref }]
//       Facts that agents reviewed the turn this event completes. Rendered as a
//       "⚖ reviewed by <agent> → <verdict>" chip on the source turn's row.
//   ev.agent_authorship: { agent, run_id, operator_edited }
//       This user-role event was composed by an agent (the vendor record says
//       "user"; the turn-anchored caused edge carries the truth). Rendered as
//       the authorship badge — NEVER an unmarked human turn (plan §6, Q4):
//         operator_edited false → "sent by ⚖ <agent>"
//         operator_edited true  → "you · via ⚖ <agent>"
//   ev.turn_anchor: opaque vendor-composed anchor string, compared only for
//       equality (plan §4 EdgeSource / TurnAnchorer contract).
//
// CLIENT-SIDE JOIN (the fallback while the daemon does not emit the fields):
// the session agents panel loads the managed projection and publishes an index
// built from runs + relationships. A relationship's source_turn_anchor /
// reply_turn_anchor keys the facts to transcript rows carrying the same
// ev.turn_anchor. When a turn-level anchor is ABSENT the fact is recorded at
// the task boundary only ({ taskId, kind, fact } in index.boundary) — surfaced
// by the agents strip, never guessed onto a transcript row by timestamp.
import { el } from '../core.js';
import { registerTranscriptDecorator } from '../transcript-decorators.js';

/* ---------- pure join logic (unit-tested) ---------- */

function agentLabelFor(run, relationship) {
  return String(run?.profile_name || run?.profile_id || relationship?.role || 'agent');
}

function verdictFor(run) {
  const detail = run?.detail;
  if (detail && typeof detail.verdict === 'string' && detail.verdict) return detail.verdict;
  if (run?.action) return String(run.action).replaceAll('_', ' ');
  return '';
}

// buildAgentTurnIndex maps the managed projection (runs + relationships) onto
// turn-anchored facts. Returns:
//   byAnchor — Map(turn_anchor → { reviews:[fact], authorship:fact|null })
//   boundary — [{ taskId, kind:'review'|'authorship', fact }] for every fact
//              whose relationship carries NO turn anchor (task boundary only).
export function buildAgentTurnIndex(projection = {}) {
  const runs = Array.isArray(projection.runs) ? projection.runs : [];
  const relationships = Array.isArray(projection.relationships) ? projection.relationships : [];
  const byRunID = new Map(runs.map(run => [run.run_id, run]));
  const byAnchor = new Map();
  const boundary = [];
  const slot = anchor => {
    if (!byAnchor.has(anchor)) byAnchor.set(anchor, { reviews: [], authorship: null });
    return byAnchor.get(anchor);
  };
  const linkedRuns = new Set();
  for (const rel of relationships) {
    const run = byRunID.get(rel.run_id);
    if (run) linkedRuns.add(run.run_id);
    const agent = agentLabelFor(run, rel);
    const review = { agent, verdict: verdictFor(run), runId: rel.run_id || '', groupId: rel.group_id || '' };
    if (rel.source_turn_anchor) slot(rel.source_turn_anchor).reviews.push(review);
    else boundary.push({ taskId: String(rel.parent_task_id || run?.source_task_id || ''), kind: 'review', fact: review });
    if (rel.reply_task_id) {
      // operator_edited is unknowable from the projection alone; leave it
      // undefined so presentation defaults to the operator-edited wording —
      // the only shipped send path today is Edit & send. The daemon-supplied
      // ev.agent_authorship.operator_edited decides when present.
      const authorship = { agent, runId: rel.run_id || '', groupId: rel.group_id || '', operatorEdited: undefined };
      if (rel.reply_turn_anchor) {
        const entry = slot(rel.reply_turn_anchor);
        if (!entry.authorship) entry.authorship = authorship;
      } else {
        boundary.push({ taskId: String(rel.reply_task_id), kind: 'authorship', fact: authorship });
      }
    }
  }
  // A completed claim whose run has no relationship row yet still deserves a
  // boundary fact — visible on the strip, never on a guessed row.
  for (const run of runs) {
    if (linkedRuns.has(run.run_id)) continue;
    if (run.state !== 'completed') continue;
    boundary.push({ taskId: String(run.source_task_id || ''), kind: 'review',
      fact: { agent: agentLabelFor(run, null), verdict: verdictFor(run), runId: run.run_id || '', groupId: run.group_id || '' } });
  }
  return { byAnchor, boundary };
}

// reviewChipText renders one review fact as chip copy.
export function reviewChipText(fact) {
  const agent = String(fact?.agent || 'agent');
  const verdict = String(fact?.verdict || '');
  return verdict ? '⚖ reviewed by ' + agent + ' → ' + verdict : '⚖ reviewed by ' + agent;
}

// authorshipBadgeText applies the owner-confirmed wording (plan §13 Q4).
// operator_edited defaults to TRUE when unknown: the client join cannot prove
// an unedited auto-send, and claiming "sent by the agent" for a turn the
// operator edited would be the dishonest direction.
export function authorshipBadgeText(fact) {
  const agent = String(fact?.agent || 'agent');
  const operatorEdited = fact?.operatorEdited === undefined ? true : Boolean(fact.operatorEdited);
  return operatorEdited ? 'you · via ⚖ ' + agent : 'sent by ⚖ ' + agent;
}

/* ---------- published index + pending-row registry ---------- */

let publishedKey = '';
let publishedIndex = null;
let pendingRows = []; // rows rendered before the index arrived: {anchor, kind, ev, row}

// publishAgentTurnIndex installs the client-side join result for one session
// and retro-decorates rows that rendered before the projection loaded. The
// session agents panel calls this after loading the managed projection.
export function publishAgentTurnIndex(sessionKey, index) {
  if (sessionKey !== publishedKey) pendingRows = [];
  publishedKey = String(sessionKey || '');
  publishedIndex = index || null;
  if (!publishedIndex) return;
  const remaining = [];
  for (const entry of pendingRows) {
    if (!entry.row.isConnected) continue;
    const facts = publishedIndex.byAnchor.get(entry.anchor);
    if (!facts) { remaining.push(entry); continue; }
    if (entry.kind === 'review' && facts.reviews.length) attachReviewChips(entry.row, facts.reviews);
    else if (entry.kind === 'authorship' && facts.authorship) attachAuthorshipBadge(entry.row, facts.authorship);
    else remaining.push(entry);
  }
  pendingRows = remaining;
}

function anchorFacts(anchor) {
  return anchor && publishedIndex ? publishedIndex.byAnchor.get(anchor) : null;
}

/* ---------- DOM attachment (typed nodes only — contract-test discipline) ---------- */

function attachReviewChips(row, reviews) {
  if (row.querySelector?.('.agent-review-chips')) return;
  const host = el('div', 'agent-review-chips');
  for (const fact of reviews) {
    const chip = el('span', 'agent-review-chip', reviewChipText(fact));
    if (fact.runId) chip.title = 'Agent claim · run ' + fact.runId;
    host.appendChild(chip);
  }
  row.appendChild(host);
}

function attachAuthorshipBadge(row, fact) {
  if (row.querySelector?.('.agent-authorship-badge')) return;
  const badge = el('div', 'agent-authorship-badge', authorshipBadgeText(fact));
  badge.title = 'This turn entered the vendor transcript as a user message; the agent relationship records who composed it.';
  row.insertBefore(badge, row.firstChild);
}

/* ---------- the two decorators ---------- */

function normalizeReviewFacts(list) {
  return list.map(item => ({ agent: item.agent, verdict: item.verdict || '', runId: item.run_id || item.runId || '' }));
}

function reviewedByDecorator(ev, row) {
  if (ev.kind === 'user') return; // review facts land on the reviewed (assistant/tool) turn
  if (Array.isArray(ev.agent_review) && ev.agent_review.length) {
    attachReviewChips(row, normalizeReviewFacts(ev.agent_review));
    return;
  }
  if (!ev.turn_anchor) return; // no anchor → boundary-only; the strip carries it
  const facts = anchorFacts(ev.turn_anchor);
  if (facts?.reviews.length) attachReviewChips(row, facts.reviews);
  else pendingRows.push({ anchor: ev.turn_anchor, kind: 'review', ev, row });
}

function authorshipDecorator(ev, row) {
  if (ev.kind !== 'user') return; // agent-authored turns ride the user role by construction
  if (ev.agent_authorship && ev.agent_authorship.agent) {
    attachAuthorshipBadge(row, { agent: ev.agent_authorship.agent,
      runId: ev.agent_authorship.run_id || '', operatorEdited: ev.agent_authorship.operator_edited });
    return;
  }
  if (!ev.turn_anchor) return; // no anchor → task boundary only; never guess by timestamp
  const facts = anchorFacts(ev.turn_anchor);
  if (facts?.authorship) attachAuthorshipBadge(row, facts.authorship);
  else pendingRows.push({ anchor: ev.turn_anchor, kind: 'authorship', ev, row });
}

registerTranscriptDecorator(reviewedByDecorator, 10);
registerTranscriptDecorator(authorshipDecorator, 20);

export { reviewedByDecorator, authorshipDecorator };
