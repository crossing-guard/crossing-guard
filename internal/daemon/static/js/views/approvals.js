import { $, el, shortWhen } from '../core.js';
import { S } from '../state.js';
import { railHead, railRow, railEmpty, crumb, markSelected, scopeBanner } from './governance-ui.js';
import { decideApproval } from '../approval/approval-api.js';
import {
  approvalSubtitle, approvalTitle, appendApprovalTags, countdownText,
  reconcileApprovalCards, updateApprovalCountdowns,
} from '../approval/approval-card.js';
import { approvalProjectionStore } from '../approval/approval-projection-store.js';

const OUTCOME_CHIP = { allow: 'st-verified', allowed: 'st-verified', deny: 'st-disputed',
  denied: 'st-disputed', expired: 'st-stale' };
const drafts = new Map();
let holdSel = null;
let approvalView = null;

function approvalSessionIdentity(approval) {
  return approval.catalog_session_id || approval.session || approval.native_session_id || '';
}

function acceptedApprovalResponse(approval) {
  const responses = Array.isArray(approval.responses) ? approval.responses : [];
  return responses.length ? responses[responses.length - 1] : null;
}

function approvalResponderLabel(response) {
  if (response?.responder?.kind === 'interactive' && response.responder.id === 'local-console') {
    return 'Answered through local console';
  }
  if (response?.responder?.kind && response?.responder?.id) {
    return 'Answered by ' + response.responder.kind + ' responder ' + response.responder.id;
  }
  return 'Responder not recorded (legacy)';
}

function refreshApprovalsBadge() {
  const count = approvalProjectionStore.pending().length;
  const badge = $('#apbadge');
  if (badge) { badge.textContent = count; badge.classList.toggle('hidden', count === 0); }
}

// One draft per approval, shared by both surfaces that can show it: the global
// interrupt bar and the approvals view. Typing a reason or picking an answer in one
// must not be lost when the other re-renders.
function approvalDraft(approvalID) {
  return drafts.get(approvalID) || { reason: '', selections: [] };
}

function setReasonDraft(approvalID, value) {
  drafts.set(approvalID, { ...approvalDraft(approvalID), reason: value });
  for (const input of document.querySelectorAll('input.ap-reason')) {
    if (input.dataset.approvalId === approvalID && input.value !== value) input.value = value;
  }
}

function setSelectionDraft(approvalID, selections) {
  drafts.set(approvalID, { ...approvalDraft(approvalID), selections });
}

const cardOptions = {
  readDraft: approvalDraft,
  writeReasonDraft: setReasonDraft,
  writeSelectionDraft: setSelectionDraft,
  decide: decideApproval,
};

// pickedText renders one response's answers for reading: what was chosen, per
// question, kept separate from the reason claimed for choosing it.
function pickedText(approval, response) {
  const selections = Array.isArray(response?.selections) ? response.selections : [];
  if (!selections.length) return '';
  const prompts = Array.isArray(approval.prompts) ? approval.prompts : [];
  return selections.map(selection => {
    const prompt = prompts.find(item => item.id === selection.prompt_id);
    const label = prompt?.header || prompt?.text || selection.prompt_id;
    return label + ' → ' + (selection.values || []).join(', ');
  }).join(' · ');
}

function renderInterrupt() {
  const bar = $('#interrupt'); if (!bar) return;
  const pending = approvalProjectionStore.pending();
  if (!pending.length) { bar.classList.add('hidden'); bar.replaceChildren(); return; }
  bar.classList.remove('hidden');
  let head = bar.querySelector('.int-head'); let body = bar.querySelector('.int-body');
  if (!head || !body) {
    head = el('div', 'int-head'); body = el('div', 'int-body hidden'); body.id = 'approval-interrupt-body';
    const warning = el('span', 'int-warn', '⚠');
    const label = el('span'); label.dataset.approvalCount = 'true';
    const review = el('button', 'btn', 'Review'); review.setAttribute('aria-expanded', 'false');
    review.setAttribute('aria-controls', body.id);
    review.onclick = () => {
      const open = body.classList.toggle('hidden') === false;
      review.textContent = open ? 'Hide' : 'Review'; review.setAttribute('aria-expanded', String(open));
    };
    head.append(warning, label, review); bar.append(head, body);
  }
  const label = head.querySelector('[data-approval-count]');
  label.textContent = pending.length + ' approval' + (pending.length === 1 ? '' : 's') + ' awaiting your decision';
  reconcileApprovalCards(body, pending, 'interrupt', cardOptions);
}

function renderHold(container, approval) {
  container.replaceChildren();
  const card = el('div', 'banner');
  const head = el('div', 'row');
  head.appendChild(el('b', null, approvalTitle(approval)));
  head.appendChild(el('span', 'chip st-draft', approvalSubtitle(approval)));
  if (approval.runtime) head.appendChild(el('span', 'chip st-draft', approval.runtime));
  const outcome = approval.status === 'expired' ? 'expired' : (approval.status || 'unknown');
  head.appendChild(el('span', 'chip ' + (OUTCOME_CHIP[outcome] || 'st-draft'), outcome));
  card.appendChild(head);
  if (approval.status === 'expired') card.appendChild(el('div', 'sub',
    'The deadline passed without an operative decision, so the request failed closed.'));
  if (approval.prompts_completeness === 'truncated') card.appendChild(el('div', 'sub',
    'This call asked a question whose options exceeded the inbox limits, so nobody here saw them.'));
  if (approval.late) card.appendChild(el('div', 'sub',
    'A later response was recorded for audit only; it did not affect the closed request.'));
  if (approval.message) card.appendChild(el('div', null, approval.message));
  if (approval.summary || approval.command) {
    const detail = el('pre'); detail.textContent = approval.summary || approval.command;
    detail.style.whiteSpace = 'pre-wrap'; card.appendChild(detail);
  }
  appendApprovalTags(card, approval);
  const line = (label, value) => {
    const row = el('div', 'row'); row.style.marginTop = '4px';
    row.append(el('span', 'sub', label), el('span', '', value)); card.appendChild(row);
  };
  if (approval.runtime) line('runtime', approval.runtime);
  if (approval.task_id) line('task', approval.task_id);
  if (approvalSessionIdentity(approval)) line('session', approvalSessionIdentity(approval));
  if (approval.tool_call_id) line('native call', approval.tool_call_id);
  line('held at', approval.created_at ? shortWhen(approval.created_at) : 'unknown');
  line(approval.status === 'expired' ? 'closed at deadline' : 'decided',
    approval.decided_at ? shortWhen(approval.decided_at) : 'no decision recorded');

  const response = acceptedApprovalResponse(approval);
  if (response) {
    line('responder', approvalResponderLabel(response));
    line('answer', response.decision || 'unknown');
    if (pickedText(approval, response)) line('picked', pickedText(approval, response));
    line('effect', response.disposition === 'late-advisory' ? 'Audit only—request was already closed' : 'Operative');
    line('response ID', response.id || 'not recorded');
    line('submitted', response.submitted_at ? shortWhen(response.submitted_at) : 'not recorded');
    line('accepted', response.accepted_at ? shortWhen(response.accepted_at) : 'not recorded');
  } else if (approval.status === 'expired' && !approval.late) {
    line('responder', 'Nobody answered before the deadline');
  } else {
    line('responder', approvalResponderLabel(null));
  }
  line('reason (claimed)', approval.reason || 'none given');
  if (approval.boundary) card.appendChild(el('div', 'sub', approval.boundary));
  container.appendChild(card);
}

function updateApprovalView() {
  if (!approvalView?.main?.isConnected) return;
  const pending = approvalProjectionStore.pending(); const history = approvalProjectionStore.history();
  approvalView.pendingHeading.classList.toggle('hidden', pending.length === 0);
  reconcileApprovalCards(approvalView.pendingBox, pending, 'history', cardOptions);
  const scopeID = S.governSession?.id;
  const shown = scopeID ? history.filter(item => approvalSessionIdentity(item) === scopeID) : history;
  const hidden = history.length - shown.length;
  approvalView.scope.replaceChildren();
  if (scopeID) {
    const banner = scopeBanner({ note: shown.length + ' of ' + history.length + ' holds', hidden });
    if (banner) approvalView.scope.appendChild(banner);
  }
  approvalView.side.replaceChildren(railHead(scopeID ? 'Holds for this session' : 'Past holds', shown.length));
  if (!shown.length) {
    approvalView.side.appendChild(railEmpty(scopeID ? 'No holds recorded for this scoped session.' : 'No approvals have been held yet.'));
    approvalView.detail.replaceChildren(el('div', 'empty', pending.length
      ? 'No completed decisions yet—the requests above are still open.' : 'No approval records yet.'));
    return;
  }
  const selected = shown.find(item => item.id === holdSel) || shown[0]; holdSel = selected.id;
  for (const item of shown) {
    const outcome = item.status === 'expired' ? 'expired' : (item.status || 'unknown');
    const row = railRow({ title: approvalTitle(item), meta: [el('span', 'chip ' + (OUTCOME_CHIP[outcome] || 'st-draft'), outcome), shortWhen(item.decided_at)],
      onClick: target => { markSelected(target); holdSel = item.id; approvalView.setCrumb(approvalTitle(item)); renderHold(approvalView.detail, item); } });
    if (item.id === holdSel) markSelected(row);
    approvalView.side.appendChild(row);
  }
  approvalView.setCrumb(approvalTitle(selected)); renderHold(approvalView.detail, selected);
}

async function renderApprovals(container) {
  const main = container || $('#main'); const side = $('#sidebody');
  main.replaceChildren();
  let crumbElement = crumb('Approvals'); main.appendChild(crumbElement);
  const setCrumb = value => { const next = crumb('Approvals', value); crumbElement.replaceWith(next); crumbElement = next; };
  main.append(el('h2', null, 'Approvals—the record of what was held'),
    el('div', 'sub', 'Policy hooks and supported runtime tools use one decision inbox. Live requests appear in the global banner; this view keeps their exact outcome and origin.'));
  const pendingHeading = el('h3', 'hidden', 'Awaiting your decision now');
  const pendingBox = el('div'); const scope = el('div'); const detail = el('div');
  main.append(pendingHeading, pendingBox, scope, detail);
  approvalView = { main, side, pendingHeading, pendingBox, scope, detail, setCrumb };
  updateApprovalView();
}

setInterval(() => {
  updateApprovalCountdowns();
}, 1000);

approvalProjectionStore.subscribe(() => {
  const pendingIDs = new Set(approvalProjectionStore.pending().map(approval => approval.id));
  for (const approvalID of drafts.keys()) if (!pendingIDs.has(approvalID)) drafts.delete(approvalID);
  refreshApprovalsBadge(); renderInterrupt(); updateApprovalView();
});

export { renderApprovals, refreshApprovalsBadge, renderInterrupt, acceptedApprovalResponse, approvalResponderLabel };
