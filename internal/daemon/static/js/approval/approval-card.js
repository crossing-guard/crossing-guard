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
  return Math.ceil(milliseconds / 1000) + 's remaining; request is denied on expiry';
}

export function approvalGrantOptions(approval) {
  if (Array.isArray(approval.grant_options) && approval.grant_options.length) {
    const valid = approval.grant_options.filter(option => option && typeof option.id === 'string'
      && typeof option.label === 'string' && typeof option.scope === 'string'
      && typeof option.duration === 'string');
    if (valid.length) return valid;
  }
  return [{
    id: 'request', label: approval.allow_label || 'Allow once',
    scope: approval.grant_scope || 'This request only',
    duration: approval.grant_duration || 'Until this request finishes; it is not remembered',
  }];
}

export function approvalFacts(approval, grantID = '') {
  const targets = Array.isArray(approval.targets)
    ? approval.targets.filter(target => typeof target === 'string' && target.trim()) : [];
  const options = approvalGrantOptions(approval);
  const selected = options.find(option => option.id === (grantID || approval.selected_grant_id))
    || options[0];
  return {
    action: approval.action || approvalTitle(approval),
    targets,
    reason: approval.approval_reason || 'Approval is required before this action can continue.',
    grantID: selected.id, scope: selected.scope, duration: selected.duration,
    allowLabel: selected.label,
  };
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
  return head;
}

function appendFact(facts, label, value, className = '') {
  const row = el('div', 'ap-fact');
  row.appendChild(el('div', 'ap-fact-label', label));
  const content = el('div', 'ap-fact-value ' + className);
  if (value && typeof value === 'object' && value.nodeType) content.appendChild(value);
  else content.textContent = value;
  row.appendChild(content);
  facts.appendChild(row);
}

export function appendApprovalFacts(card, approval, { includeDeadline = true, grantID = '' } = {}) {
  const values = approvalFacts(approval, grantID);
  const facts = el('div', 'ap-facts');
  appendFact(facts, 'Request', values.action);
  if (values.targets.length) {
    const row = el('div', 'ap-fact');
    row.appendChild(el('div', 'ap-fact-label', values.targets.length === 1 ? 'Target' : 'Targets'));
    const targets = el('div', 'ap-targets');
    for (const target of values.targets) targets.appendChild(el('code', 'ap-target', target));
    row.appendChild(targets);
    facts.appendChild(row);
  } else {
    appendFact(facts, 'Target', 'See the request details below');
  }
  appendFact(facts, 'Why approval is needed', values.reason);
  const scope = el('span', null, values.scope);
  const duration = el('span', null, values.duration);
  appendFact(facts, 'Applies to', scope);
  appendFact(facts, 'Lasts until', duration);
  if (includeDeadline) {
    const deadline = el('span', 'ap-deadline', countdownText(approval.deadline));
    deadline.dataset.deadline = approval.deadline;
    const row = el('div', 'ap-fact');
    row.appendChild(el('div', 'ap-fact-label', 'Respond within'));
    row.appendChild(deadline);
    facts.appendChild(row);
  }
  card.appendChild(facts);
  return {
    ...values,
    selectGrant(id) {
      const next = approvalFacts(approval, id);
      scope.textContent = next.scope;
      duration.textContent = next.duration;
      return next;
    },
  };
}

function appendGrantChoices(card, approval, selectedID, changed) {
  const options = approvalGrantOptions(approval);
  if (options.length < 2) return;
  const fieldset = el('fieldset', 'ap-grants');
  fieldset.appendChild(el('legend', 'sub', 'Approval length'));
  for (const option of options) {
    const label = el('label', 'ap-grant');
    const input = document.createElement('input');
    input.type = 'radio'; input.name = 'approval-grant-' + approval.id;
    input.value = option.id; input.checked = option.id === selectedID;
    input.addEventListener('change', () => { if (input.checked) changed(option); });
    label.appendChild(input);
    label.appendChild(el('span', null, option.label));
    fieldset.appendChild(label);
  }
  card.appendChild(fieldset);
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

function appendDroppedPromptWarning(card, approval) {
  if (!promptsWereDropped(approval)) return;
  card.appendChild(el('div', 'sub', 'This call is asking a question whose options exceed the '
    + 'approvals inbox limits, so they were not carried here. Allowing lets the call proceed '
    + 'without an answer, and the session is told why.'));
}

function setInitialAllowState(approval, allow, status, readSelections) {
  if (approval.mode === 'hard-block') {
    allow.disabled = true;
    allow.title = 'Restricted and non-overridable';
  } else if (readSelections) {
    allow.disabled = !selectionsAreComplete(approval, readSelections());
    status.textContent = allow.disabled ? 'Choose an answer to enable Allow.' : '';
  }
}

export function createApprovalCard(approval, surface, options) {
  if (typeof options?.decide !== 'function') throw new Error('approval card needs a decision callback');
  const card = el('div', 'banner');
  card.dataset.approvalId = approval.id;
  card.style.borderLeftColor = approval.mode === 'hard-block' ? 'var(--bad)' : 'var(--warn)';
  card.appendChild(approvalCardHead(approval));
  if (approval.message) card.appendChild(el('div', null, approval.message));
  let selectedGrantID = approvalFacts(approval).grantID;
  const facts = appendApprovalFacts(card, approval, { grantID: selectedGrantID });
  appendDroppedPromptWarning(card, approval);
  appendCallDetail(card, approval);
  const { label: reasonLabel, reason } = reasonInput(approval, surface, options);
  const deny = el('button', 'btn', 'Deny');
  const allowText = label => promptsOf(approval).length ? label + ' with answer' : label;
  const allow = el('button', 'btn', allowText(facts.allowLabel));
  appendGrantChoices(card, approval, selectedGrantID, option => {
    selectedGrantID = option.id;
    const selected = facts.selectGrant(option.id);
    allow.textContent = allowText(selected.allowLabel);
  });
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
  setInitialAllowState(approval, allow, status, readSelections);
  const decide = async decision => {
    deny.disabled = true;
    allow.disabled = true;
    reason.disabled = true;
    status.textContent = 'Submitting decision…';
    try {
      const selections = decision === 'allow' && readSelections ? readSelections() : [];
      await options.decide(approval.id, decision, reason.value.trim(), selections,
        decision === 'allow' ? selectedGrantID : '');
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
