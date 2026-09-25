// Dictation in the composer. Owns the mic control, the push-to-talk key, the
// mirror overlay that paints provisional words, the insertion offset, the
// strip/hint/review/disclosure surfaces, and the one final replaceRange into
// the composer. The textarea never holds provisional text; the daemon owns
// audio, decoding, and every end state.
import { el } from '../core.js';
import * as speechAPI from '../speech/speech-api.js';
import { resample, toPCM16, isSilent } from './wav.js';

const ICON_MIC = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><rect x="9" y="3" width="6" height="11" rx="3"/><path d="M5 11a7 7 0 0 0 14 0M12 18v3"/></svg>';

// --- pure rules, exported for tests ---------------------------------------

// adjustOffset moves a tracked insertion offset across one textarea edit,
// derived by diffing the previous and next values. It returns null when the
// edit swallowed the offset.
export function adjustOffset(offset, previous, next) {
  if (offset === null || offset === undefined) return null;
  let prefix = 0;
  const limit = Math.min(previous.length, next.length);
  while (prefix < limit && previous[prefix] === next[prefix]) prefix++;
  let suffix = 0;
  while (suffix < limit - prefix && previous[previous.length - 1 - suffix] === next[next.length - 1 - suffix]) suffix++;
  const removedEnd = previous.length - suffix;
  const delta = next.length - previous.length;
  if (prefix >= offset) return offset;            // edit entirely after the point
  if (removedEnd <= offset) return offset + delta; // edit entirely before the point
  return null;                                     // edit spans the point
}

export function parseBinding(binding) {
  const parts = String(binding || '').split('+').map(part => part.trim().toLowerCase()).filter(Boolean);
  const key = parts.pop() || 'space';
  return { alt: parts.includes('alt'), ctrl: parts.includes('ctrl') || parts.includes('control'),
    meta: parts.includes('meta') || parts.includes('cmd'), shift: parts.includes('shift'), key };
}

export function matchesBinding(event, binding) {
  const code = String(event.code || '').toLowerCase();
  const key = String(event.key || '').toLowerCase();
  const wanted = binding.key === 'space' ? 'space' : binding.key;
  const keyMatches = wanted === 'space' ? (code === 'space' || key === ' ') : (code === 'key' + wanted || key === wanted);
  return keyMatches && !!event.altKey === binding.alt && !!event.ctrlKey === binding.ctrl && !!event.metaKey === binding.meta && !!event.shiftKey === binding.shift;
}

export function holdOrTap(pressedMs, thresholdMs) { return pressedMs >= thresholdMs ? 'hold' : 'tap'; }

export function autoSendAllowed(ui, draftWasEmpty, text) {
  if (!ui?.auto_send || !draftWasEmpty) return false;
  return String(text || '').trim().split(/\s+/).filter(Boolean).length >= (ui.auto_send_min_words || 0);
}

export function splitTail(committed, tail, dimTailWords) {
  const words = String(tail || '').split(/\s+/).filter(Boolean);
  const cut = Math.max(0, words.length - (dimTailWords || 0));
  return { stable: [committed, ...words.slice(0, cut)].filter(Boolean).join(' '), dim: words.slice(cut).join(' ') };
}

export function formatClock(ms) {
  const total = Math.max(0, Math.floor(ms / 1000));
  return Math.floor(total / 60) + ':' + String(total % 60).padStart(2, '0');
}

// --- controller ------------------------------------------------------------

export function createDictation({ root, controls, composer, cwd, api = speechAPI, doc = document,
  media = navigator.mediaDevices, AudioContextCtor = window.AudioContext, workletURL = 'js/task/audio-worklet.js' } = {}) {
  const mic = el('button', 'iconbtn dictation-mic');
  mic.type = 'button'; mic.innerHTML = ICON_MIC; mic.title = 'Dictate';
  mic.setAttribute('aria-label', 'Dictate');
  const attach = controls.querySelector('.attachment-add');
  if (attach) attach.after(mic); else controls.insertBefore(mic, controls.firstChild);
  const strip = el('div', 'dictation-strip hidden');
  const hint = el('div', 'dictation-hint hidden');
  const review = el('div', 'dictation-review hidden');
  const disclose = el('div', 'dictation-disclose hidden');
  const overlay = el('div', 'dictation-overlay');
  overlay.setAttribute('aria-hidden', 'true');
  root.insertBefore(strip, controls); root.insertBefore(hint, controls);
  root.insertBefore(review, controls); root.insertBefore(disclose, controls);
  root.appendChild(overlay);
  const textarea = composer.textarea;

  let caps = null;
  let state = 'idle';           // idle | requesting | listening | finishing | inserted
  let dictationId = '';
  let insertionOffset = null;
  let draftWasEmpty = false;
  let committed = '', tail = '';
  let lastValue = textarea.value;
  let inserted = null;          // { start, text }
  let pressStart = 0, mode = 'hold';
  let audio = null;             // { context, stream, node, seq, offset, pending, silentMs, startedAt, timer, level }
  let streamAbort = null;
  let failures = [];
  let pausedUntil = 0;
  let tintTimer = null, clockTimer = null;
  const listeners = new AbortController();
  const placeholder = textarea.placeholder;
  // The placeholder would show through the overlay under the first words.
  const setPlaceholder = live => { textarea.placeholder = live ? '' : placeholder; };

  const show = (node, on) => node.classList.toggle('hidden', !on);
  const setHint = (text, error = false, action = null) => {
    hint.replaceChildren();
    if (!text) { show(hint, false); return; }
    hint.appendChild(doc.createTextNode(text + ' '));
    if (action) {
      const link = el('a', '', action.label); link.href = '#';
      link.onclick = event => { event.preventDefault(); action.run(); };
      hint.appendChild(link);
    }
    hint.classList.toggle('err', error); show(hint, true);
  };
  const ready = () => caps?.state === 'ready';

  const paintMic = () => {
    const paused = Date.now() < pausedUntil;
    mic.classList.toggle('off', !ready() || paused);
    mic.classList.toggle('live', state === 'listening');
    if (!ready()) mic.title = caps?.reason ? 'Dictation is not set up. ' + caps.reason : 'Dictation is not set up. Open Settings → Speech.';
    else if (state === 'listening') mic.title = 'Stop dictating';
    else mic.title = 'Dictate — hold ' + caps.ui.push_to_talk + ', or click to toggle';
  };

  const paintOverlay = () => {
    overlay.replaceChildren();
    const live = state === 'listening' || state === 'finishing';
    const tinted = state === 'inserted' && inserted;
    if (!live && !tinted) { overlay.style.display = 'none'; return; }
    const m = composer.metrics();
    Object.assign(overlay.style, { display: 'block', font: m.font, lineHeight: m.lineHeight, letterSpacing: m.letterSpacing,
      top: m.offsetTop + 'px', left: m.offsetLeft + 'px', width: m.width + 'px', height: m.height + 'px',
      paddingLeft: m.paddingLeft, paddingTop: m.paddingTop, boxSizing: 'border-box' });
    overlay.scrollTop = m.scrollTop;
    const value = textarea.value;
    if (tinted) {
      overlay.append(doc.createTextNode(value.slice(0, inserted.start)));
      overlay.appendChild(el('span', 'dictated', value.slice(inserted.start, inserted.start + inserted.text.length)));
      overlay.append(doc.createTextNode(value.slice(inserted.start + inserted.text.length)));
      return;
    }
    const at = insertionOffset === null ? composer.caret() : insertionOffset;
    overlay.append(doc.createTextNode(value.slice(0, at)));
    const { stable, dim } = splitTail(committed, tail, caps.ui.dim_tail_words);
    const needsSpace = at > 0 && !/\s$/.test(value.slice(0, at)) && (stable || dim);
    if (stable) overlay.appendChild(el('span', 'provisional', (needsSpace ? ' ' : '') + stable));
    if (dim) overlay.appendChild(el('span', 'provisional-tail', ((stable || needsSpace) ? ' ' : '') + dim));
    if (state === 'listening') {
      const level = el('span', 'level'); level.style.height = (0.4 + 0.8 * (audio?.level || 0)) + 'em';
      overlay.appendChild(level);
    }
    overlay.append(doc.createTextNode(value.slice(at)));
  };

  const paintStrip = () => {
    strip.replaceChildren();
    if (state === 'listening') {
      const elapsed = audio ? Date.now() - audio.startedAt : 0;
      const meter = el('span', 'meter');
      for (let i = 0; i < 8; i++) { const bar = el('i'); bar.style.height = (2 + 12 * Math.max(0, Math.min(1, (audio?.levels?.[i] || 0)))) + 'px'; meter.appendChild(bar); }
      const cap = caps.limits.max_seconds, silence = caps.limits.silence_stop_seconds;
      strip.append(el('span', 'dot'), meter, el('span', 'time', formatClock(elapsed) + ' · up to ' + formatClock(cap * 1000) + ' · stops after ' + silence + ' s of silence'), el('span', 'spacer'));
      const cancel = el('button', 'quiet', 'Cancel'); cancel.type = 'button'; cancel.onclick = () => void cancel_();
      strip.appendChild(cancel);
    } else if (state === 'finishing') {
      const elapsed = audio ? audio.durationMs : 0;
      strip.append(el('span', 'dot calm'), el('span', '', formatClock(elapsed) + ' · finishing…'), el('span', 'spacer'));
      const cancel = el('button', 'quiet', 'Cancel'); cancel.type = 'button'; cancel.onclick = () => void cancel_();
      strip.appendChild(cancel);
    } else if (state === 'inserted' && inserted) {
      strip.append(el('span', 'dot calm'), el('span', '', 'Inserted ' + formatClock(inserted.durationMs) + (inserted.atCursor ? ' at the cursor' : '')), el('span', 'spacer'));
      const undo = el('button', 'quiet', 'Undo'); undo.type = 'button'; undo.onclick = undoInsert;
      strip.appendChild(undo);
    }
    show(strip, state !== 'idle' && state !== 'requesting');
  };

  const paint = () => { paintMic(); paintStrip(); paintOverlay(); };

  const noteFailure = () => {
    const now = Date.now();
    if (!ready()) return;
    const windowMs = caps.limits.failure_pause_seconds * 1000;
    failures = failures.filter(at => now - at < windowMs); failures.push(now);
    if (failures.length >= caps.limits.failure_pause_count) {
      pausedUntil = failures[0] + windowMs; failures = [];
      setHint('Dictation paused for a moment.');
      setTimeout(paint, pausedUntil - now + 10);
    }
  };

  async function loadCapabilities() {
    try { caps = await api.loadSpeechCapabilities(); }
    catch (error) { caps = { state: 'unavailable', reason: error?.message || String(error) }; }
    paint();
  }

  // --- capture -------------------------------------------------------------

  async function startCapture(created) {
    const stream = await media.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
    const context = new AudioContextCtor();
    await context.audioWorklet.addModule(workletURL);
    const source = context.createMediaStreamSource(stream);
    const node = new AudioWorkletNode(context, 'dictation-capture', { numberOfInputs: 1, numberOfOutputs: 0, channelCount: 1 });
    audio = { context, stream, node, seq: 0, offset: 0, pending: [], pendingLength: 0, silentMs: 0, startedAt: Date.now(),
      level: 0, levels: new Array(8).fill(0), durationMs: 0, sending: Promise.resolve() };
    const targetRate = created.sample_rate;
    node.port.onmessage = message => {
      if (state !== 'listening' || !audio) return;
      const frames = message.data;
      const resampled = resample(frames, context.sampleRate, targetRate);
      const silent = isSilent(resampled, caps.limits);
      const frameMs = resampled.length * 1000 / targetRate;
      audio.silentMs = silent ? audio.silentMs + frameMs : 0;
      audio.durationMs += frameMs;
      const { peak } = { peak: silent ? 0 : Math.min(1, Math.max(...Array.from(resampled.slice(0, 256)).map(Math.abs)) * 2) };
      audio.level = peak; audio.levels.push(peak); audio.levels.shift();
      audio.pending.push(resampled); audio.pendingLength += resampled.length;
      if (audio.silentMs >= caps.limits.silence_stop_seconds * 1000) { void stop('silence'); return; }
      if (audio.durationMs >= caps.limits.max_seconds * 1000) { void stop('cap'); }
    };
    source.connect(node);
    const interval = caps.streaming.frame_interval_ms;
    audio.timer = setInterval(() => {
      if (!root.isConnected) { dispose(); return; }
      void flushFrame(); paintStrip(); paintOverlay();
    }, interval);
  }

  async function flushFrame() {
    if (!audio || !audio.pendingLength || (state !== 'listening' && state !== 'finishing')) return;
    const merged = new Float32Array(audio.pendingLength);
    let at = 0;
    for (const chunk of audio.pending) { merged.set(chunk, at); at += chunk.length; }
    audio.pending = []; audio.pendingLength = 0;
    const bytes = toPCM16(merged);
    const seq = audio.seq++, offset = audio.offset; audio.offset += bytes.length;
    audio.sending = audio.sending.then(() => api.sendFrame(dictationId, seq, offset, bytes)).catch(error => {
      if (state === 'listening') fail('Dictation failed.', error);
    });
    return audio.sending;
  }

  function stopCapture() {
    if (!audio) return;
    clearInterval(audio.timer);
    try { audio.node.port.onmessage = null; audio.node.disconnect(); } catch { /* already gone */ }
    try { audio.stream.getTracks().forEach(track => track.stop()); } catch { /* already gone */ }
    void audio.context.close().catch(() => {});
  }

  // --- lifecycle -----------------------------------------------------------

  async function start(trigger) {
    if (!ready() || state !== 'idle' || Date.now() < pausedUntil) return;
    setHint('');
    review.replaceChildren(); show(review, false);
    if (caps.backend?.requires_disclosure) { showDisclosure(trigger); return; }
    await begin('', trigger);
  }

  function showDisclosure(trigger) {
    disclose.replaceChildren();
    disclose.appendChild(el('div', 'h', 'Send this recording to ' + (caps.backend?.display_name || 'the transcription provider') + '?'));
    disclose.appendChild(doc.createTextNode(caps.disclosure?.text || ''));
    const row = el('div', 'row');
    const go = el('button', 'primary', 'Start'); go.type = 'button';
    go.onclick = async () => {
      show(disclose, false);
      try {
        const confirmed = await api.confirmDisclosure(caps.backend.backend, caps.disclosure?.version);
        await begin(confirmed.token, trigger);
      } catch (error) { fail('Could not confirm.', error); }
    };
    const cancelBtn = el('button', '', 'Cancel'); cancelBtn.type = 'button'; cancelBtn.onclick = () => show(disclose, false);
    row.append(go, cancelBtn); disclose.appendChild(row); show(disclose, true);
  }

  async function begin(token, trigger) {
    // A hold gesture cannot survive the disclosure card: the key was released to
    // click Start, so a confirmed dictation always runs in toggle mode.
    state = 'requesting'; mode = trigger === 'key' && !token ? 'hold' : 'toggle';
    insertionOffset = composer.caret(); draftWasEmpty = composer.draft() === '';
    committed = ''; tail = ''; inserted = null; lastValue = textarea.value;
    try {
      const created = await api.createDictation({ cwd: typeof cwd === 'function' ? cwd() : '', disclosure_token: token || undefined });
      dictationId = created.id;
      await startCapture(created);
    } catch (error) {
      state = 'idle'; stopCapture(); audio = null;
      if (error?.name === 'NotAllowedError' || error?.name === 'SecurityError') setHint('Microphone is blocked for this site. Allow it in the address bar, then try again.', true);
      else if (error?.name === 'NotFoundError') setHint('No microphone found.', true);
      else if (error?.code === 'busy') setHint('Finish the current dictation first.');
      else fail('Dictation failed.', error);
      if (dictationId) { void api.cancelDictation(dictationId).catch(() => {}); dictationId = ''; }
      paint(); return;
    }
    state = 'listening';
    setPlaceholder(true);
    streamAbort = new AbortController();
    void api.openDictationStream(dictationId, onEvent, streamAbort.signal).catch(() => {});
    paint();
  }

  function onEvent(event) {
    if (!event || event.kind !== 'partial') return;
    if (state !== 'listening' && state !== 'finishing') return;
    committed = event.committed || ''; tail = event.tail || '';
    paintOverlay();
  }

  async function stop(reason) {
    if (state !== 'listening') return;
    state = 'finishing';
    stopCapture();
    await flushFrame().catch(() => {});
    if (audio) await audio.sending.catch(() => {});
    paint();
    let ended;
    try { ended = await api.finishDictation(dictationId, reason); }
    catch (error) { fail('Dictation failed.', error); return; }
    settle(ended);
  }

  function settle(ended) {
    streamAbort?.abort(); streamAbort = null;
    setPlaceholder(false);
    const duration = ended.duration_ms || audio?.durationMs || 0;
    const lastSeen = [committed, tail].filter(Boolean).join(' ').trim();
    audio = null; dictationId = '';
    switch (ended.state) {
      case 'final': insert(ended.text, duration); return;
      case 'low_confidence': state = 'idle'; showReview(ended.text, duration); paint(); return;
      case 'no_speech': state = 'idle'; setHint('Nothing heard in ' + formatClock(duration) + '.', false, { label: 'Try again', run: () => void start('click') }); paint(); return;
      case 'cancelled': state = 'idle'; paint(); return;
      case 'timeout': state = 'idle'; if (lastSeen) insert(lastSeen, duration); setHint('Took too long.', false, { label: 'Try again', run: () => void start('click') }); noteFailure(); paint(); return;
      case 'provider_error': state = 'idle'; if (lastSeen) insert(lastSeen, duration); setHint((ended.message || 'Connection lost') + '.', true, { label: 'Try again', run: () => void start('click') }); noteFailure(); paint(); return;
      default: state = 'idle'; if (lastSeen) insert(lastSeen, duration); setHint('Dictation failed.', true, { label: 'Try again', run: () => void start('click') }); noteFailure(); paint();
    }
  }

  function insert(text, durationMs) {
    const value = textarea.value;
    const atCursor = insertionOffset === null;
    let at = atCursor ? composer.caret() : insertionOffset;
    at = Math.max(0, Math.min(at, value.length));
    const before = value.slice(0, at);
    const needsSpace = before.length > 0 && !/\s$/.test(before);
    const finalText = (needsSpace ? ' ' : '') + text;
    const caretWasAtPoint = composer.caret() === at;
    composer.replaceRange(at, at, finalText, { moveCaret: caretWasAtPoint });
    inserted = { start: at, text: finalText, durationMs, atCursor };
    lastValue = textarea.value;
    committed = ''; tail = ''; insertionOffset = null;
    state = 'inserted';
    clearTimeout(tintTimer);
    tintTimer = setTimeout(clearInserted, caps.ui.tint_seconds * 1000);
    if (autoSendAllowed(caps.ui, draftWasEmpty, text)) root.dispatchEvent(new CustomEvent('cg:dictation-autosend', { bubbles: true }));
    paint();
    composer.focus();
  }

  function clearInserted() {
    if (state === 'inserted') { state = 'idle'; inserted = null; paint(); }
  }

  function undoInsert() {
    if (!inserted) return;
    const current = textarea.value.slice(inserted.start, inserted.start + inserted.text.length);
    if (current === inserted.text) composer.replaceRange(inserted.start, inserted.start + inserted.text.length, '');
    inserted = null; state = 'idle'; lastValue = textarea.value; paint();
  }

  function showReview(text, durationMs) {
    review.replaceChildren();
    review.appendChild(el('div', 'h', 'This may not be right'));
    review.appendChild(doc.createTextNode(text));
    const row = el('div', 'row');
    const use = el('button', 'primary', 'Insert'); use.type = 'button';
    use.onclick = () => { show(review, false); insert(text, durationMs); };
    const discard = el('button', '', 'Discard'); discard.type = 'button'; discard.onclick = () => { show(review, false); paint(); };
    const again = el('button', '', 'Record again'); again.type = 'button'; again.onclick = () => { show(review, false); void start('click'); };
    row.append(use, discard, again); review.appendChild(row); show(review, true);
  }

  async function cancel_() {
    if (state !== 'listening' && state !== 'finishing' && state !== 'requesting') return;
    const id = dictationId;
    stopCapture(); streamAbort?.abort(); streamAbort = null;
    state = 'idle'; audio = null; dictationId = ''; committed = ''; tail = '';
    setPlaceholder(false);
    if (id) await api.cancelDictation(id).catch(() => {});
    paint();
  }

  function fail(message, error) {
    stopCapture(); streamAbort?.abort(); streamAbort = null;
    const id = dictationId; dictationId = ''; audio = null; state = 'idle';
    setPlaceholder(false);
    if (id) void api.cancelDictation(id).catch(() => {});
    setHint(message, true, { label: 'Try again', run: () => void start('click') });
    if (error?.code === 'unavailable') setHint('Dictation is not set up on this machine.', false);
    noteFailure(); paint();
  }

  // --- input tracking and keys ----------------------------------------------

  textarea.addEventListener('input', () => {
    const next = textarea.value;
    if (state === 'listening' || state === 'finishing') insertionOffset = adjustOffset(insertionOffset, lastValue, next);
    if (state === 'inserted' && inserted) {
      const stillThere = next.slice(inserted.start, inserted.start + inserted.text.length) === inserted.text;
      if (!stillThere || next !== lastValue) clearInserted();
    }
    lastValue = next;
    paintOverlay();
  }, { signal: listeners.signal });
  textarea.addEventListener('scroll', paintOverlay, { signal: listeners.signal });

  const binding = () => parseBinding(caps.ui.push_to_talk);
  doc.addEventListener('keydown', event => {
    if (!root.isConnected) { dispose(); return; }
    if (event.key === 'Escape' && (state === 'listening' || state === 'finishing')) { event.preventDefault(); void cancel_(); return; }
    if (!ready() || doc.activeElement !== textarea || !matchesBinding(event, binding())) return;
    event.preventDefault();
    if (event.repeat) return;
    if (state === 'listening' && mode === 'toggle') { void stop('release'); return; }
    if (state === 'idle') { pressStart = Date.now(); void start('key'); }
  }, { signal: listeners.signal });
  doc.addEventListener('keyup', event => {
    if (!ready()) return;
    if (!matchesBinding(event, binding()) && !(binding().key === 'space' && event.code === 'Space')) return;
    if (state !== 'listening' && state !== 'requesting') return;
    if (mode !== 'hold') return;
    const held = holdOrTap(Date.now() - pressStart, caps.ui.hold_threshold_ms);
    if (held === 'hold') void stop('release'); else mode = 'toggle';
  }, { signal: listeners.signal });
  mic.addEventListener('click', () => {
    if (!ready()) { setHint('Dictation is not set up on this machine.', false, { label: 'Open Settings', run: () => doc.dispatchEvent(new CustomEvent('cg:open-settings')) }); return; }
    if (state === 'listening') { mode = 'toggle'; void stop('release'); return; }
    void start('click');
  }, { signal: listeners.signal });

  void loadCapabilities();
  doc.addEventListener('cg:speech-capabilities-changed', () => void loadCapabilities(), { signal: listeners.signal });
  paint();

  // dispose stops everything a torn-down composer could otherwise keep alive:
  // the microphone, the frame loop, the stream, and the daemon-side dictation.
  function dispose() {
    const id = dictationId;
    listeners.abort(); stopCapture(); streamAbort?.abort(); streamAbort = null;
    clearTimeout(tintTimer); clearInterval(clockTimer); overlay.remove();
    state = 'idle'; audio = null; dictationId = '';
    if (id) void api.cancelDictation(id).catch(() => {});
  }

  return {
    mic,
    isLive() { return state === 'listening' || state === 'finishing' || state === 'requesting'; },
    state() { return state; },
    refresh: loadCapabilities,
    dispose,
  };
}
