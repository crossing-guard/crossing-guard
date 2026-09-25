import { el } from '../core.js';
import {
  appendPromptFieldsets, promptsOf, promptsWereDropped, selectionsAreComplete,
} from './approval-choice.js';

export function approvalTitle(approval) {
  return approval.origin === 'runtime_tool'
    ? (approval.tool_name || 'Runtime tool')
    : (approval.rule || 'Policy approval');
}

export function approvalSubtitle(approval) {
  return approval.origin === 'runtime_tool' ? 'Runtime tool request' : 'Governance policy hold';
}

export function countdownText(deadline, now = Date.now()) {
  const milliseconds = new Date(deadline).getTime() - now;
  if (!Number.isFinite(milliseconds) || milliseconds <= 0) return 'deadline passed—request denied';
  return Math.ceil(milliseconds / 1000) + 's until fail-closed';
}

export function appendApprovalTags(container, approval) {
  const tags = el('div', 'row');
  for (const tag of approval.fired_tags || []) {
    if (tag && tag !== '(none)') tags.appendChild(el('span', 'chip cl-observed', tag));
  }
  if (tags.children.length) container.appendChild(tags);
}

function sessionIdentity(approval) {
  return approval.catalog_session_id || approval.session || approval.native_session_id || '';
}

function approvalCardHead(approval) {
  const head = el('div', 'row');
  head.appendChild(el('b', null, approvalTitle(approval)));
  head.appendChild(el('span', 'chip st-draft', approvalSubtitle(approval)));
  if (approval.runtime) head.appendChild(el('span', 'chip st-draft', approval.runtime));
  if (approval.mode) {
    head.appendChild(el('span', 'chip ' + (approval.mode === 'hard-block' ? 'st-disputed' : 'st-stale'),
      approval.mode));
  }
  if (promptsWereDropped(approval)) {
    head.appendChild(el('span', 'chip st-disputed', 'options not carried'));
  }
  const countdown = el('span', 'chip');
  countdown.dataset.deadline = approval.deadline;
  countdown.textContent = countdownText(approval.deadline);
  head.appendChild(countdown);
  return head;
}

// The raw call detail stays available but steps aside when there are real options to
// read: a question competing with its own JSON is a question people answer wrongly.
function appendCallDetail(card, approval) {
  const body = approval.summary || approval.command;
  if (!body) return;
  const detail = el('pre');
  detail.textContent = body;
  detail.style.whiteSpace = 'pre-wrap';
  if (!promptsOf(approval).length) {
    card.appendChild(detail);
    return;
  }
  const box = el('details');
  box.appendChild(el('summary', 'sub', 'Raw held call'));
  box.appendChild(detail);
  card.appendChild(box);
}

function appendApprovalIdentity(card, approval) {
  const identity = [];
  if (approval.task_id) identity.push('task ' + approval.task_id.slice(-12));
  if (sessionIdentity(approval)) identity.push('session ' + sessionIdentity(approval).slice(-12));
  if (approval.tool_call_id) identity.push('call ' + approval.tool_call_id.slice(-12));
  if (identity.length) card.appendChild(el('div', 'sub', identity.join(' · ')));
  appendApprovalTags(card, approval);
  if (approval.boundary) card.appendChild(el('div', 'sub', approval.boundary));
}

function reasonInput(approval, surface, options) {
  const inputID = 'approval-reason-' + surface + '-' + approval.id;
  const label = el('label', 'sub', approval.mode === 'confirm-and-record'
    ? 'Override reason (required)' : 'Decision reason (optional)');
  label.htmlFor = inputID;
  const reason = el('input', 'ap-reason');
  reason.id = inputID;
  reason.dataset.approvalId = approval.id;
  reason.value = options.readDraft?.(approval.id)?.reason || '';
  reason.placeholder = approval.mode === 'confirm-and-record'
    ? 'Attributed and recorded; does not un-taint the session' : 'Optional reason';
  reason.style.width = '420px';
  reason.addEventListener('input', () => options.writeReasonDraft?.(approval.id, reason.value));
  return { label, reason };
}

export function createApprovalCard(approval, surface, options) {
  if (typeof options?.decide !== 'function') throw new Error('approval card needs a decision callback');
  const card = el('div', 'banner');
  card.dataset.approvalId = approval.id;
  card.style.borderLeftColor = approval.mode === 'hard-block' ? 'var(--bad)' : 'var(--warn)';
  card.appendChild(approvalCardHead(approval));
  if (approval.message) card.appendChild(el('div', null, approval.message));
  if (promptsWereDropped(approval)) {
    card.appendChild(el('div', 'sub', 'This call is asking a question whose options exceed the '
      + 'approvals inbox limits, so they were not carried here. Allowing lets the call proceed '
      + 'without an answer, and the session is told why.'));
  }
  appendCallDetail(card, approval);
  const { label: reasonLabel, reason } = reasonInput(approval, surface, options);
  const deny = el('button', 'btn', 'Deny');
  const allow = el('button', 'btn', promptsOf(approval).length ? 'Allow with answer' : 'Allow');
  const status = el('span', 'sub');
  status.setAttribute('role', 'alert');
  const readSelections = appendPromptFieldsets(card, approval, surface, {
    initial: options.readDraft?.(approval.id)?.selections || [],
    changed: () => {
      const selections = readSelections ? readSelections() : [];
      options.writeSelectionDraft?.(approval.id, selections);
      allow.disabled = !selectionsAreComplete(approval, selections);
    },
  });
  appendApprovalIdentity(card, approval);
  if (approval.mode === 'hard-block') {
    allow.disabled = true;
    allow.title = 'Restricted and non-overridable';
  } else if (readSelections) {
    allow.disabled = !selectionsAreComplete(approval, readSelections());
    status.textContent = allow.disabled ? 'Choose an answer to enable Allow.' : '';
  }
  const decide = async decision => {
    deny.disabled = true;
    allow.disabled = true;
    reason.disabled = true;
    status.textContent = 'Submitting decision…';
    try {
      const selections = decision === 'allow' && readSelections ? readSelections() : [];
      await options.decide(approval.id, decision, reason.value.trim(), selections);
      status.textContent = 'Decision recorded.';
    } catch (error) {
      deny.disabled = false;
      allow.disabled = approval.mode === 'hard-block';
      reason.disabled = false;
      status.textContent = error?.message || String(error);
    }
  };
  deny.onclick = () => { void decide('deny'); };
  allow.onclick = () => { void decide('allow'); };
  const actions = el('div', 'row');
  actions.append(reasonLabel, reason, deny, allow, status);
  card.appendChild(actions);
  return card;
}

export function reconcileApprovalCards(container, approvals, surface, options) {
  const existing = new Map([...container.querySelectorAll(':scope > [data-approval-id]')]
    .map(card => [card.dataset.approvalId, card]));
  for (const approval of approvals) {
    let card = existing.get(approval.id);
    if (!card) card = createApprovalCard(approval, surface, options);
    container.appendChild(card);
    existing.delete(approval.id);
  }
  for (const card of existing.values()) card.remove();
}

export function updateApprovalCountdowns(root = document, now = Date.now()) {
  for (const node of root.querySelectorAll('[data-deadline]')) {
    node.textContent = countdownText(node.dataset.deadline, now);
  }
}
