import { el } from '../core.js';

// reviewCard renders one review invocation on the unified agent-claim wire:
// `action` carries the recommendation (allow/deny/abstain). Shared by this
// exact-session projection and the Agents page review history.
function reviewCard(item) {
  const review = item.invocation || {};
  const card = el('article', 'orchestration-review-card');
  const delegated = Boolean(review.approval_id);
  const head = el('div', 'row'); head.append(el('strong', '', delegated ? 'Delegated approval review' : 'Independent review recommendation · report only'),
    el('span', 'chip', String(review.state || 'unknown').replaceAll('_', ' ')));
  card.append(head, el('div', 'sub', String(review.runtime || 'runtime unknown') + ' · ' +
    String(review.tool || 'tool unknown') + ' · ' + reviewTime(review.admitted_at)));
  if (review.state === 'completed') {
    card.appendChild(el('div', 'orchestration-review-claim',
      'Model recommendation: ' + String(review.action || 'abstain') + ' — ' + String(review.message || 'No message.')));
    const picked = pickedLabel(review.selections_json);
    if (picked) card.appendChild(el('div', 'orchestration-review-claim', 'Answer chosen: ' + picked));
    if (review.citations?.length) card.appendChild(el('div', 'sub', 'Cited supplied facts: ' + review.citations.join(' · ')));
  } else if (review.recovery) {
    card.appendChild(el('div', 'sub', String(review.recovery)));
  }
  card.appendChild(el('div', 'sub', 'Observed governance decision: ' +
    String(item.observed_governance_decision || 'not retained or not reported')));
  card.appendChild(el('div', 'sub', 'Actual tool outcome: ' + outcomeLabel(item.actual_outcome)));
  if (delegated) card.appendChild(el('div', 'sub', 'Approval owner outcome: ' +
    String(review.approval_outcome || 'not submitted').replaceAll('_', ' ')));
  card.appendChild(el('div', 'sub', 'Profile ' + shortDigest(review.profile_bundle_digest) + ' · path ' +
    shortDigest(review.request_path_digest) + ' · ' + String(review.duration_ms || 0) + ' ms · ' +
    String(review.timing_class || 'timing unknown').replaceAll('_', ' ')));
  return card;
}

// pickedLabel reads what the reviewer CLAIMED to choose. Whether that answer became
// operative is the approval owner's outcome, rendered separately below it.
function pickedLabel(encoded) {
  if (!encoded) return '';
  let selections = null;
  try { selections = JSON.parse(encoded); } catch { return 'unreadable'; }
  if (!Array.isArray(selections) || !selections.length) return '';
  return selections
    .map(selection => String(selection.prompt_id) + ' → ' + (selection.values || []).join(', '))
    .join(' · ');
}

function outcomeLabel(outcome) {
  if (!outcome?.observed) return 'not observed (not evidence that the tool did not run)';
  return 'exact result observation: ' + String(outcome.state || 'unknown');
}

function reviewTime(seconds) {
  if (!seconds) return 'time unavailable';
  return new Date(Number(seconds) * 1000).toLocaleString();
}

function shortDigest(value) { const text = String(value || 'unavailable'); return text.length > 24 ? text.slice(0, 24) + '…' : text; }

export { reviewCard, outcomeLabel };
