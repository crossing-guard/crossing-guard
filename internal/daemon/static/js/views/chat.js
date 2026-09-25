import { $, el, cpHeaders, api, fmtTime, normMemId, mdToHtml, debounce, fillSelect, mkSelectKV, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from "../ui.js";
import { S } from "../state.js";
import { renderTranscript } from "./sessions.js";
import { taskProjectionStore } from "../task/task-projection-store.js";
import { createRuntimeTask, interruptRuntimeTask } from "../task/task-api.js";
import { progressWords } from "../task/session-status.js";
import { createComposer } from "../task/composer.js";
import { createAttachments } from "../task/attachments.js";
import { createDictation } from "../task/dictation.js";
import { hasRenderableText, isVendorTurnEvidence } from "../task/task-event-semantics.js";
import { loadChatCapabilities, findChatCapability, chooseChatCapability, capabilityPairs, modePairs, modelPairs, runtimeChatDefaults } from "../chat-capabilities.js";

/* ---------- chat ----------
   Keep-alive is now the router's job (app.js: chat is a 'pin' view). renderChat
   builds fresh each time it is called — which the router only does on first
   visit — and the running stream survives navigation because the router stashes
   this DOM into a fragment and reattaches it, closures intact.

   chatState is PER-INVOCATION, not module-level. Inline mode builds one composer
   per session, so a module-level object let a stream still running in session A
   write its session id into the state session B's composer reads — B's next turn
   could land in A's session. Each composer owns its own state; the running
   stream's closure keeps writing to its own instance. */
/* inline = mounted under a session transcript. The session pane already rendered
   that transcript and already owns the scroller, so inline mode REUSES both
   (`existingLog`) instead of building a second #chatlog — rendering the events
   twice, and nesting a viewport-height scroller inside the pane's, was the
   Track 2.6 mount bug. New turns append to the transcript the user is reading. */
async function renderChat(container, inline, existingLog) {
  const main = container || $('#main');
  if (!inline) main.innerHTML = '';
  // Consume preload before the first await. The app-owned synchronous mount event
  // clears the shared handoff immediately after dispatch.
  const preload = S.chatPreload;
  S.chatPreload = null;
  const capabilityLoading = el('div', 'sub', 'Loading registered chat runtimes…');
  main.appendChild(capabilityLoading);
  let capabilities;
  try {
    capabilities = await loadChatCapabilities();
  } catch (error) {
    if (capabilityLoading.isConnected) capabilityLoading.replaceWith(el('div', 'banner', 'Chat runtimes unavailable: ' + (error.message || error)));
    return;
  }
  if (!capabilityLoading.isConnected) return;
  capabilityLoading.remove();
  /* origin = the live session this composer is bound to, or null for an ad-hoc
     chat that has not started one yet. Two ids, kept distinct:
       resumeId  — vendor resume handle (codex: the trailing thread uuid)
       harvestId — the full harvest record id (/api/handoff/generate, openSession)
     The server currently accepts either form — codexRuntime.MatchID suffix-matches
     (harvest/codex.go) — so this is defensive precision, not a bug fix: without it
     the console silently depends on a lenient matcher it never declares. */
  const chatState = { sessionId: null, runtime: '', origin: null, liveTurns: 0 };
  const wrap = el('div'); wrap.id = 'chatwrap';
  const defs = getDefaults();
  const scopedCwd = !inline && typeof S.chatCwd === 'string' ? S.chatCwd : '';
  chatState.runtime = chooseChatCapability(capabilities, preload?.runtime || defs.runtime).runtime;
  // inline = the live mode of a session: the session's own header already names
  // it, so a second "Fallback chat" heading would rename the user's session.
  if (!inline) {
    wrap.appendChild(el('h2', '', 'Fallback chat'));
    wrap.appendChild(el('div', 'sub', 'Drives a registered, already-installed coding harness. Crossing Guard does not host its model or take custody of its provider credentials.'));
    if (scopedCwd) {
      const scope = el('div', 'chat-scope');
      scope.append(el('span', 'chat-scope-label', 'New chat in'), el('code', '', scopedCwd));
      scope.title = scopedCwd;
      wrap.appendChild(scope);
    }
  }

  // Machine/account config (endpoint, token, binary, extra args) lives in
  // SETTINGS, entered once and persisted — not in a per-message drawer where a
  // password field was retyped on every chat (§11). The composer keeps only
  // `cwd`, which is genuinely per-session. Read at SEND time, so changing
  // Settings takes effect without rebuilding the composer.
  const acct = () => runtimeChatDefaults(getDefaults(), chatState.runtime);
  const cwd = el('input'); cwd.placeholder = 'working directory';
  cwd.className = 'ctl ctl-cwd';
  cwd.value = scopedCwd || (defs.cwd || '');
  if (!inline) S.chatCwd = null; // one fresh-chat destination; never leaks into the next chat
  cwd.title = 'Working directory for this session';
  // Show the END of the path. A left-truncated absolute path reads
  // "/Users/example/Doc…" for every project on the machine — identical, and
  // therefore useless. The project name is the part that identifies it.
  const showTail = () => { cwd.scrollLeft = cwd.scrollWidth; cwd.title = cwd.value || 'working directory'; };
  cwd.addEventListener('blur', showTail);
  requestAnimationFrame(showTail);

  // Inline: adopt the session's transcript element; standalone: build our own.
  const log = existingLog || el('div');
  if (!existingLog) { log.id = 'chatlog'; wrap.appendChild(log); }
  if (inline) wrap.classList.add('inline'); // drops the 100vh column sizing (CSS)
  // The scroller is the pane in inline mode, not the log — measuring the log
  // there would make stick()/scrolled() track a container that never scrolls.
  const scroller = existingLog ? (existingLog.closest('.detailscroll') || existingLog) : log;

  // --- composer: input on top, ONE compact control line below (claude-code style) ---
  const composerController = createComposer({
    sendIntent: () => { void doSend(); },
    loadFiles: async query => api('/api/files?q=' + encodeURIComponent(query) + '&cwd=' + encodeURIComponent(cwd.value || '')),
  });
  const { root: composer, controls: below } = composerController;
  // The mode warning lives OUTSIDE the composer's border. Inside it, it read as
  // text someone had typed into the prompt.

  const currentCapability = () => findChatCapability(capabilities, chatState.runtime);
  const attachmentController = createAttachments({
    root: composer, controls: below, capability: currentCapability(),
    storageKey: 'cg_task_inputs:' + (preload?.harvestId || preload?.sessionId || (inline ? 'inline' : 'new')),
  });
  const dictationController = createDictation({ root: composer, controls: below, composer: composerController,
    cwd: () => cwd.value });
  // Auto-send never doubles as Stop: while a turn runs the dictated text stays
  // in the draft for the user to send afterwards.
  composer.addEventListener('cg:dictation-autosend', () => { if (!running) void doSend(); });
  const selRuntime = mkSelectKV(capabilityPairs(capabilities), chatState.runtime);
  selRuntime.className = 'ctl'; selRuntime.title = 'Runtime';
  const selMode = mkSelectKV(modePairs(currentCapability()), '');
  selMode.className = 'ctl'; selMode.title = 'Mode — what the process may do';
  const selModel = mkSelectKV(modelPairs(currentCapability()), '');
  selModel.className = 'ctl'; selModel.title = 'Model';
  const customModel = el('input', 'ctl hidden'); customModel.placeholder = currentCapability().modelHint;
  selModel.onchange = () => customModel.classList.toggle('hidden', selModel.value !== 'custom');

  // Switching runtime refills mode/model, but a round trip (claude → codex →
  // claude) must not silently discard what the user had picked, so each
  // runtime's selection is remembered.
  let curRuntime = chatState.runtime;
  const selMemory = {};
  function applyRuntime(target) {
    clearShareWarn();
    selMemory[curRuntime] = { mode: selMode.value, model: selModel.value };
    chatState.runtime = target; curRuntime = target;
    const capability = currentCapability();
    fillSelect(selMode, modePairs(capability), '');
    fillSelect(selModel, modelPairs(capability), '');
    customModel.placeholder = capability.modelHint;
    syncModeNote(); // the mode list just changed; the warning must follow it
    const remembered = selMemory[target];
    if (remembered) { selMode.value = remembered.mode || ''; selModel.value = remembered.model || ''; }
    customModel.classList.toggle('hidden', selModel.value !== 'custom');
    attachmentController.setCapability(capability);
  }

  /* The cross-runtime gate. A runtime cannot resume another runtime's session,
     so switching away from the live session's runtime must DECLARE that in place
     rather than silently starting an unrelated session — the old handler called
     newSession(), which cleared the transcript the user was reading (INV-22).
     Keyed on live state, not on how the session got here: an ad-hoc chat that has
     produced a real session id needs the same gate as one opened from the rail. */
  selRuntime.onchange = () => {
    const target = selRuntime.value;
    const live = chatState.origin;
    if (!live) { applyRuntime(target); newSession(); return; } // nothing to lose yet
    applyRuntime(target);
    if (target === live.runtime) { clearGate(); return; }      // switched back
    showGate(live, target);
  };
  // A mode that cannot act says so HERE, under the controls, and only while it
  // is selected — a warning that is always on screen stops being read.
  const modeNote = el('div', 'modenote hidden');
  const syncModeNote = () => {
    const mode = currentCapability()?.modes.find(item => item.id === selMode.value);
    const warned = mode?.risk === 'elevated' || mode?.risk === 'dangerous';
    modeNote.textContent = warned ? mode.description : '';
    modeNote.classList.toggle('hidden', !warned);
  };
  selMode.addEventListener('change', syncModeNote);

  const gearBtn = el('button', 'iconbtn', '⚙');
  gearBtn.title = 'Settings — endpoint, token, binary';
  gearBtn.onclick = () => document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'settings' }));
  const newBtn = el('button', 'iconbtn', '＋'); newBtn.title = 'New session';

  const status = el('div', 'status');
  const sessBadge = el('span', '', 'new session');
  const costBadge = el('span', '', '');
  status.append(sessBadge, costBadge);

  below.prepend(selRuntime, selMode, selModel, customModel, cwd, gearBtn, newBtn, status);
  syncModeNote();
  // The gate lives in the COMPOSER block, never in the scroll pane — inserting it
  // above the transcript would move the reader's scroll anchor.
  const gate = el('div', 'gate hidden');
  const authGate = el('div', 'auth-gate hidden');
  // Ownership warning (in-turn-progress plan, Part A): the daemon refused
  // because another process is using this session. Inline, not a modal — the
  // sentence is the daemon's own, and "Send anyway" is the person's override
  // for this one send. Enter keeps its meaning.
  const shareWarn = el('div', 'share-warn hidden');
  shareWarn.setAttribute('role', 'alert');
  composer.prepend(gate, authGate, shareWarn);
  let allowSharedSession = false;
  function clearShareWarn() { shareWarn.classList.add('hidden'); shareWarn.innerHTML = ''; }
  function showShareWarn(message) {
    shareWarn.innerHTML = '';
    shareWarn.classList.remove('hidden');
    // The daemon's sentence, as prose: capitalised, one full stop per clause.
    const prose = message.charAt(0).toUpperCase() + message.slice(1).replace('; ', '. ');
    shareWarn.appendChild(el('span', '', prose.endsWith('.') ? prose : prose + '.'));
    const again = el('button', 'btn', 'Send anyway');
    again.onclick = () => { allowSharedSession = true; clearShareWarn(); doSend(); };
    const cancel = el('button', 'btn', 'Cancel');
    cancel.onclick = clearShareWarn;
    shareWarn.append(again, cancel);
  }
  wrap.appendChild(composer);
  wrap.appendChild(modeNote);
  main.appendChild(wrap);

  const gateOpen = () => !gate.classList.contains('hidden');
  function clearGate() { gate.classList.add('hidden'); gate.innerHTML = ''; }
  const capitalize = s => (s || '').charAt(0).toUpperCase() + (s || '').slice(1);

  function clearAuthGate() { authGate.classList.add('hidden'); authGate.innerHTML = ''; }
  async function refreshVendorAuth() {
    const status = await api('/api/chat/auth?runtime=' + encodeURIComponent(chatState.runtime));
    if (status.ready) { clearAuthGate(); return true; }
    showAuthRecovery(status);
    return false;
  }
  function showAuthRecovery(status, message='') {
    authGate.innerHTML = '';
    authGate.classList.remove('hidden');
    const runtimeLabel = capitalize(status.runtime || chatState.runtime);
    authGate.appendChild(el('div', 'auth-gate-h', runtimeLabel + ' needs sign-in'));
    authGate.appendChild(el('div', 'sub', message ||
      (status.can_sign_in === false
        ? 'Authenticate with the harness or configured provider outside Crossing Guard, then check again.'
        : 'Crossing Guard will open the official ' + runtimeLabel + ' browser sign-in. The vendor keeps the credential.')));
    const row = el('div', 'row');
    const signIn = el('button', 'btn primary', status.flow === 'running' ? 'Sign-in running…' : 'Sign in');
    signIn.disabled = status.flow === 'running' || status.can_sign_in === false;
    signIn.onclick = async () => {
      signIn.disabled = true; signIn.textContent = 'Opening sign-in…';
      try {
        const started = await api('/api/chat/auth/start', {
          method: 'POST', body: JSON.stringify({ runtime: chatState.runtime }),
        });
        showAuthRecovery(started, 'Complete the vendor sign-in, then choose Check again.');
      } catch (err) {
        showAuthRecovery(status, 'Could not open sign-in: ' + (err.message || err));
      }
    };
    const retry = el('button', 'btn', 'Check again');
    retry.onclick = async () => {
      retry.disabled = true; retry.textContent = 'Checking…';
      try { await refreshVendorAuth(); }
      catch (err) { showAuthRecovery(status, 'Status unavailable: ' + (err.message || err)); }
    };
    row.append(signIn, retry); authGate.appendChild(row);
  }

  /* Two honest exits, never one. Forcing every runtime switch through a handoff
     strands the user who just wants a clean session in the other runtime — and
     the destructive option is the one that must say what it discards (INV-13). */
  function showGate(live, target) {
    gate.innerHTML = '';
    gate.classList.remove('hidden');
    gate.appendChild(el('div', 'gate-h', `⚠ ${capitalize(target)} can't resume this ${capitalize(live.runtime)} session.`));
    gate.appendChild(el('div', 'sub', 'Continuing generates a handoff extract you review before it is written.'));
    const row = el('div', 'row');
    const review = el('button', 'btn primary', 'Review handoff →');
    review.onclick = () => openHandoff();
    const fresh = el('button', 'btn', `Start a fresh ${capitalize(target)} session`);
    fresh.onclick = () => { clearGate(); chatState.origin = null; newSession(); };
    row.append(review, fresh);
    gate.appendChild(row);
    // Enter changes meaning while the gate is up; say so rather than silently
    // reassigning it.
    gate.appendChild(el('div', 'sub', 'Enter → review handoff · "fresh session" does not carry the transcript above'));
  }

  /* Routed over the document bus rather than importing sessions.js here:
     sessions.js ⇄ chat.js is already a cycle resolving only via hoisting, and a
     third edge would deepen it (plan §4). */
  function openHandoff() {
    const live = chatState.origin;
    if (!live) return;
    document.dispatchEvent(new CustomEvent('cg:handoff', { detail: {
      runtime: live.runtime, id: live.harvestId, liveTurns: chatState.liveTurns,
      target: chatState.runtime, // where the user is continuing TO
    } }));
  }

  /* Which session a note/memory is ABOUT. Always the origin when there is one:
     while the cross-runtime gate is up, chatState.runtime is already the TARGET
     while sessionId is still the origin's, so composing the two would file the
     record against `codex/<claude-session-id>` — a session that does not exist. */
  function sessionRef() {
    const o = chatState.origin;
    return o ? o.runtime + '/' + o.resumeId : chatState.runtime + '/' + chatState.sessionId;
  }

  // resolve the compact selectors into request fields
  function modelFields() {
    const v = selModel.value;
    if (v === 'custom') return { model: customModel.value.trim() };
    return { model: v };
  }

  function needsVendorAuth() {
    const capability = currentCapability();
    if (!capability?.canSignIn) return false;
    if ((capability.supportsBaseURL && acct().base_url)
      || (capability.supportsAuthToken && acct().auth_token)) return false;
    const option = capability.models.find(item => item.id === selModel.value);
    return option?.vendorAuthRequired ?? capability.vendorAuthDefault;
  }

  /* Inline, `log` is the SESSION's transcript — clearing it would delete the
     record the user is reading to make room for an unrelated chat. A new session
     is a new destination, so hand off to the standalone chat instead of emptying
     someone else's transcript in place. */
  function newSession() {
    clearShareWarn();
    if (inline) { document.dispatchEvent(new CustomEvent('cg:continue', { detail: { fresh: true } })); return; }
    chatState.sessionId = null;
    chatState.origin = null; chatState.liveTurns = 0; // a new session is bound to nothing
    sessBadge.textContent = 'new session'; costBadge.textContent = '';
    log.innerHTML = '';
  }
  newBtn.onclick = newSession;

  const scrolled = () => scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 60;
  const pillUpdate = inline ? () => {} : attachBottomPill(log, 'pill-chat');
  const stick = wasAtBottom => { if (wasAtBottom) scroller.scrollTop = scroller.scrollHeight; else pillUpdate(); };

  // message builders
  function addUser(text, attached = []) {
    const m = el('div', 'msg user');
    const bubble = el('div', 'bubble', text);
    if (attached.length) {
      const summary = el('div', 'user-attachment-summary');
      attached.forEach(input => summary.appendChild(el('span', 'user-attachment-chip',
        (input.kind === 'image' ? '▧ ' : '≡ ') + input.name)));
      bubble.appendChild(summary);
    }
    m.appendChild(bubble);
    log.appendChild(m); scroller.scrollTop = scroller.scrollHeight;
    return m;
  }
  function newAgentBubble() {
    const m = el('div', 'msg agent streaming');
    m.appendChild(mkMark(false));
    const md = el('div', 'md');
    m.appendChild(md);
    log.appendChild(m);
    return { m, md, raw: '' };
  }
  function addChip(cls, label, body) {
    const det = document.createElement('details'); det.className = cls;
    const sum = el('summary');
    if (cls === 'toolchip') {
      sum.append('⚙ ');
      const tn = el('span', 'tname', label); sum.appendChild(tn);
      sum.append(' ' + (body || '').slice(0, 60));
    } else {
      sum.textContent = label;
    }
    det.appendChild(sum);
    det.appendChild(el('div', 'tbody', body || ''));
    log.appendChild(det);
    return det;
  }

  let running = false, activeTaskID = null;
  async function interruptActive() {
    if (!running || !activeTaskID) return false;
    await interruptRuntimeTask(activeTaskID);
    return true;
  }
  const announceActive = () => document.dispatchEvent(new CustomEvent('cg:task-active', {
    detail: { root: wrap, interrupt: () => {
      if (!running || !activeTaskID) return false;
      interruptActive().catch(error => log.appendChild(el('div', 'sysline err', '✖ ' + String(error))));
      return true;
    } },
  }));
  wrap.addEventListener('focusin', announceActive);
  wrap.addEventListener('pointerdown', announceActive);
  announceActive();

  async function doSend() {
    if (running) {
      interruptActive().catch(error => log.appendChild(el('div', 'sysline err', '✖ ' + String(error))));
      return;
    } // Send button doubles as explicit task Stop
    // The ownership override is consumed here, once, on every exit path: it
    // applies to this send attempt and to nothing after it, by construction.
    const allowShared = allowSharedSession;
    allowSharedSession = false;
    clearShareWarn();
    // While the cross-runtime gate is up there is no send path — the only ways
    // forward are the two the gate offers. This is what makes the gate a gate.
    if (gateOpen()) { openHandoff(); return; }
    if (dictationController.isLive()) {
      log.appendChild(el('div', 'sysline err', '✖ Finish dictating first.'));
      return;
    }
    if (attachmentController.hasPending()) {
      log.appendChild(el('div', 'sysline err', '✖ Wait for attachments to finish inspection before sending.'));
      return;
    }
    const prompt = composerController.draft();
    if (!prompt) return;
    const fields = modelFields();
    if (needsVendorAuth()) {
      try {
        if (!await refreshVendorAuth()) return; // prompt remains in the composer
      } catch (err) {
        // A status probe is advisory when it cannot establish a verdict. Preserve
        // the existing send path; a real auth failure becomes auth_required below.
        log.appendChild(el('div', 'sysline', 'Auth preflight unavailable — attempting the turn.'));
      }
    }
    const attached = attachmentController.snapshot();
    const inputReferences = attachmentController.references();
    composerController.clear();
    const userMessage = addUser(prompt, attached);
    let turnCounted = false; // counted on the first event the vendor actually returns
    running = true; composerController.setRunning(true);
    let cur = null;          // current streaming agent bubble
    let settledRaw = '';     // the last completed text block, to recognise a late echo
    let echo = '';           // streamed text seen after that completion
    // The transcript file will carry this same turn seconds from now. Tell the
    // session pane what was already drawn, by the vendor's own record
    // identity, so the harvested copy is not drawn twice.
    const drawnLive = detail => document.dispatchEvent(new CustomEvent('cg:drawn-live', { detail }));
    drawnLive({ prompt });
    let curThink = null;     // current streaming thinking chip
    let lastTool = null;     // last tool chip (to attach its result)
    const finishCur = () => { if (cur) { cur.m.classList.remove('streaming'); cur = null; } };

    // working indicator (contract §3.4: no dead air) — pinned to the bottom of
    // the turn, shows current activity + elapsed time. Codex emits only
    // completed items, so without this the gaps between items look dead.
    const workRow = el('div', 'loader');
    const wlabel = el('span', '', 'working');
    workRow.append(mkMark(true), wlabel);
    const t0 = Date.now();
    let act = 'working';
    const tick = () => { wlabel.textContent = act + ' · ' + Math.round((Date.now() - t0) / 1000) + 's'; };
    const setAct = a => { act = a; tick(); };
    const wtimer = setInterval(tick, 1000);
    log.appendChild(workRow); scroller.scrollTop = scroller.scrollHeight;
    const applyEvent = ev => {
          /* Count the turn only once the vendor has actually answered. Counting
             at send time inflated the handoff's "N turn(s) not yet harvested"
             line whenever a turn was aborted or never spawned — and that line is
             an INV-21 claim, so an overcount is a false statement, not a rounding
             error. */
          if (!turnCounted && isVendorTurnEvidence(ev)) {
            turnCounted = true; chatState.liveTurns++;
          }
          const atBottom = scrolled();
          // activity label per event; streaming bubbles carry their own pulse
          switch (ev.type) {
            case 'spawn': setAct('starting…'); break;
            case 'delta': workRow.classList.add('hidden'); break;
            // Same rule as the pane (turn_progress.go rule 5): a COMPLETED
            // thinking block means the reply is being written; a delta means
            // the thought is still in flight.
            case 'thinking_delta': setAct(progressWords('thinking')); break;
            case 'thinking': setAct(progressWords('writing')); break;
            case 'tool': setAct(progressWords('tool', ev.name)); workRow.classList.remove('hidden'); break;
            case 'tool_result': setAct(progressWords('thinking')); workRow.classList.remove('hidden'); break;
            case 'text': setAct(progressWords('writing')); workRow.classList.remove('hidden'); break;
          }
          switch (ev.type) {
            case 'session':
              chatState.sessionId = ev.id;
              // An ad-hoc chat becomes a real session here. It needs the same
              // gate as one opened from the rail, so bind origin now rather than
              // only on preload — otherwise switching runtime after the first
              // turn still silently wipes a live transcript. For a session we
              // started ourselves the catalog id is still unknown. A vendor resume
              // handle must never be promoted into an exact harvested-row identity.
              if (!chatState.origin && ev.id) {
                chatState.origin = { runtime: chatState.runtime, resumeId: ev.id, harvestId: '', cwd: cwd.value || '' };
              }
              sessBadge.textContent = '⌁ ' + (ev.id || '').slice(0, 8) + (ev.model ? ' · ' + ev.model : '');
              break;
            case 'delta':
              // The vendor can emit a block's completed text before its last
              // streamed chunk. A chunk the completed text already ends with is
              // that echo, not a new reply, and must not open a bubble.
              if (!cur && settledRaw) {
                echo += ev.text;
                if (settledRaw.endsWith(echo)) break;
                cur = newAgentBubble(); cur.raw = echo; echo = '';
              } else {
                if (!cur) cur = newAgentBubble();
                cur.raw += ev.text;
              }
              cur.md.innerHTML = mdToHtml(cur.raw);
              break;
            case 'text':
              // final block — replace streamed content (or create if no deltas came)
              if (!cur) cur = newAgentBubble();
              cur.raw = ev.text;
              cur.md.innerHTML = mdToHtml(cur.raw);
              settledRaw = ev.text; echo = '';
              finishCur();
              if (ev.anchor) drawnLive({ anchor: ev.anchor });
              break;
            case 'thinking_delta':
              if (hasRenderableText(ev.text)) {
                if (!curThink) curThink = addChip('thinkchip', 'thinking…', '');
                curThink.querySelector('.tbody').textContent += ev.text;
              }
              break;
            case 'thinking':
              if (ev.anchor) drawnLive({ anchor: ev.anchor });
              if (curThink) {
                if (hasRenderableText(ev.text)) curThink.querySelector('.tbody').textContent = ev.text;
                curThink = null;
              } else if (hasRenderableText(ev.text)) addChip('thinkchip', 'thinking', ev.text);
              break;
            case 'tool':
              finishCur();
              lastTool = addChip('toolchip', ev.name, ev.text);
              if (ev.anchor) drawnLive({ anchor: ev.anchor });
              break;
            case 'tool_result':
              if (ev.anchor) drawnLive({ anchor: ev.anchor });
              if (lastTool) {
                lastTool.querySelector('.tbody').textContent += '\n── result ──\n' + ev.text;
                if (ev.is_error) lastTool.classList.add('toolchip-error');
                lastTool = null;
              }
              break;
            case 'spawn': {
              const d = el('div', 'sysline', '$ ' + ev.text);
              log.appendChild(d);
              break;
            }
            case 'stderr':
              log.appendChild(el('div', 'sysline', ev.text));
              break;
            case 'inputs':
              if (!attached.length) {
                const names = (ev.items || []).map(item => item.name).filter(Boolean);
                if (names.length) log.appendChild(el('div', 'sysline', 'Attached: ' + names.join(', ')));
              }
              break;
            case 'input_error':
              userMessage.remove(); // the runtime never received it; keep it only in the composer
              composerController.restore(prompt);
              if (ev.field === 'cwd') { cwd.focus(); cwd.setSelectionRange?.(0, cwd.value.length); }
              log.appendChild(el('div', 'sysline err', '✖ ' + ev.text));
              break;
            case 'auth_required':
              finishCur();
              showAuthRecovery({ runtime: ev.runtime || chatState.runtime, flow: 'idle',
                can_sign_in: currentCapability()?.canSignIn === true }, ev.text);
              log.appendChild(el('div', 'sysline err', '✖ ' + ev.text + ' This turn was not replayed.'));
              break;
            case 'error':
              finishCur();
              log.appendChild(el('div', 'sysline err', '✖ ' + ev.text));
              break;
            case 'result':
              finishCur();
              costBadge.textContent = [
                ev.cost != null ? '$' + Number(ev.cost).toFixed(4) : '',
                ev.ms ? (ev.ms / 1000).toFixed(1) + 's' : '',
              ].filter(Boolean).join(' · ');
              break;
          }
          if (workRow.isConnected) log.appendChild(workRow); // keep pinned last
          stick(atBottom);
    };
    try {
      const idempotencyKey = globalThis.crypto?.randomUUID?.()
        || ('send-' + Date.now() + '-' + Math.random().toString(36).slice(2));
      let task;
      try {
        task = await createRuntimeTask({
          runtime: chatState.runtime, prompt, session_id: chatState.sessionId || '',
          catalog_session_id: chatState.origin?.harvestId || '',
          ...fields, mode: selMode.value,
          base_url: currentCapability().supportsBaseURL ? (acct().base_url || '') : '',
          auth_token: currentCapability().supportsAuthToken ? (acct().auth_token || '') : '',
          binary: acct().binary || '', extra_args: acct().extra_args || '', cwd: cwd.value,
          allow_shared_session: allowShared,
          ...inputReferences,
        }, idempotencyKey);
      } catch (error) {
        userMessage.remove(); composerController.restore(prompt);
        if (error?.code === 'session_in_use') {
          // The daemon refused because another process is using the session.
          // Show its sentence inline with the one-click override; the turn
          // teardown below still runs (finally), so the composer is usable.
          showShareWarn(String(error.detail || error.message || ''));
          return;
        }
        throw error;
      }
      activeTaskID = task.id;
      attachmentController.claimed();
      wrap.dataset.renderedTaskId = task.id;
      announceActive();
      await new Promise((resolve, reject) => {
        let lastSequence = 0, unsubscribe = null, settled = false;
        const finish = error => {
          if (settled) return;
          settled = true;
          if (unsubscribe) unsubscribe();
          if (error) reject(error); else resolve();
        };
        const drain = () => {
          try {
            for (const event of taskProjectionStore.events(task.id, lastSequence)) {
              lastSequence = event.sequence;
              applyEvent(event.payload || {});
            }
            const projected = taskProjectionStore.task(task.id);
            if (projected && taskProjectionStore.terminal(task.id)
              && lastSequence >= (projected.last_sequence || 0)) finish();
          } catch (error) { finish(error); }
        };
        unsubscribe = taskProjectionStore.subscribe(drain);
        if (settled) unsubscribe();
      });
      if (taskProjectionStore.task(task.id)?.lifecycle === 'interrupted') {
        log.appendChild(el('div', 'sysline', '■ stopped'));
      }
    } catch (err) {
      log.appendChild(el('div', 'sysline err', '✖ ' + String(err)));
    } finally {
      // Every exit — completed, failed, refused — puts the composer back.
      clearInterval(wtimer); workRow.remove();
      finishCur();
      running = false;
      activeTaskID = null;
      composerController.setRunning(false);
      composerController.focus();
    }
  }
  // --- composer menus: '/' command palette + '@' file mentions (items 4+5) ---
  const PALETTE = [
    { cmd: '/new', hint: 'start a new session', run: () => newSession() },
    // Goes through the same gated handler as the selector — gating only the
    // <select> would leave this as a second, ungated door.
    { cmd: '/continue', hint: 'continue in another registered runtime', run: a => {
        if (findChatCapability(capabilities, a)?.canStart) { selRuntime.value = a; selRuntime.onchange(); }
        else log.appendChild(el('div', 'sysline', 'usage: /continue ' + capabilities.filter(c => c.canStart).map(c => c.runtime).join('|'))); } },
    { cmd: '/search', hint: 'search all sessions: /search <query>', run: a => {
        document.querySelector('nav button[data-view="sessions"]').click();
        setTimeout(() => { const s = $('#search'); s.value = a; s.dispatchEvent(new Event('input')); s.focus(); }, 250); } },
    { cmd: '/session', hint: 'find a session: /session <fragment>', run: a => PALETTE[2].run(a) },
    { cmd: '/usage', hint: 'open the usage dashboard', run: () => document.dispatchEvent(new CustomEvent('cg:center', { detail: { view: 'usage' } })) },
    { cmd: '/note', hint: 'pin a note to this chat session: /note <text>', run: async a => {
        if (!a) return log.appendChild(el('div', 'sysline', 'usage: /note <text>'));
        if (!chatState.sessionId) return log.appendChild(el('div', 'sysline', 'no session yet — send a turn first'));
        await api('/api/notes', { method: 'POST', body: JSON.stringify({ target: sessionRef(), text: a }) });
        log.appendChild(el('div', 'sysline', 'Note added')); } },
    { cmd: '/memory', hint: 'save a memory claim: /memory <claim>', run: async a => {
        if (!a) return log.appendChild(el('div', 'sysline', 'usage: /memory <claim>'));
        await api('/api/memory', { method: 'POST', body: JSON.stringify({ claim: a, classification: 'user-asserted', scope: 'chat', sources: ['chat ' + sessionRef()] }) });
        log.appendChild(el('div', 'sysline', '☰ memory saved (draft)')); } },
    { cmd: '/model', hint: 'focus the model selector', run: () => selModel.focus() },
    { cmd: '/mode', hint: 'focus the mode selector', run: () => selMode.focus() },
  ];
  composerController.setCommands(PALETTE);

  // --- resume-from-session preload (set by the session viewer) ---
  if (preload) {
    const p = preload;
    chatState.runtime = p.runtime; curRuntime = p.runtime;
    chatState.sessionId = p.sessionId;
    // sessionId is the RESUME handle (codex: trailing thread uuid); harvestId is
    // the full record id. The handoff endpoint and openSession need the latter.
    chatState.origin = {
      runtime: p.runtime, resumeId: p.sessionId, harvestId: p.harvestId || p.sessionId,
      cwd: p.cwd || '', title: p.title || '',
    };
    selRuntime.value = p.runtime;
    fillSelect(selMode, modePairs(currentCapability()), '');
    fillSelect(selModel, modelPairs(currentCapability()), '');
    syncModeNote();
    // Reflect the session you are continuing (§11). Its real model was shown only
    // in a badge, so the selector said "Default" while the session ran opus — two
    // statements that disagreed. Offer the EXACT harvested model, not its family:
    // continuing with `opus` when the session ran `opus-4-8` is a different run.
    // Preserve the session's provider/model by omitting a model override. The
    // harvested model remains visible in the session badge; selecting a different
    // model is an explicit user action.
    // The mode a session originally ran with is NOT recorded anywhere we can
    // read, so the selector must not imply it is. Say so rather than showing
    // "Default" as if it were this session's setting.
    selMode.title = 'Applies to NEW turns you send from here. The mode this session ' +
      'originally ran with is not recorded, so it cannot be shown.';
    if (p.cwd) {
      cwd.value = p.cwd;
      // The session header already states this path in full. Repeating it in the
      // control row cost a slot and showed a truncated duplicate; the value is
      // still sent, it just stops taking space it does not earn.
      cwd.classList.add('hidden');
    }
    // Inline the transcript is already on screen (we adopted its element); only
    // the standalone chat has to paint the history itself.
    if (!existingLog) renderTranscript(log, p.events || []);
    log.appendChild(el('div', 'sysline',
      '⤷ resumed from history — next message continues this ' + p.runtime + ' session' + (p.cwd ? ' in ' + p.cwd : '') +
      (p.model ? ' · model ' + p.model : '') + ' · original mode not recorded'));
    sessBadge.textContent = '⌁ ' + (p.sessionId || '').slice(0, 8) + ' (resumed)';
    requestAnimationFrame(() => { scroller.scrollTop = scroller.scrollHeight; });
    setTimeout(() => { scroller.scrollTop = scroller.scrollHeight; }, 80);
  }
  // Inline, the composer sits under a session you may have opened only to READ.
  // Pre-focusing it puts the caret in a live composer, so a stray Enter sends a
  // real, costly vendor turn. Focus only the ad-hoc chat, which the user opened
  // in order to type.
  if (!inline) composerController.focus();
}

export { renderChat };
