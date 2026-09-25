import { $, el, cpHeaders, api, fmtTime, normMemId, escapeHtml, mdInline, mdToHtml, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from "../ui.js";

let ledgerSession = localStorage.getItem('cp_ledger_session') || 'console-demo';

async function renderLedger(container) {
  const main = container || $('#main');
  main.innerHTML = '<h2>Live ledger <span class="chip st-draft">P3/P5</span></h2>'
    + '<div class="sub">The daemon-owned, tamper-evident session ledger. Drive an event '
    + 'through the SHARED engine (classify → water mark → decide), watch the mark rise, '
    + 'and verify the chain against the held anchor. This is the same path the '
    + '<code>cp</code> hook feeds live.</div>';

  // status banner (honest ceiling)
  const banner = el('div'); banner.style.cssText =
    'border:1px solid var(--border);border-radius:8px;padding:10px 12px;margin-bottom:14px;background:var(--panel2)';
  main.appendChild(banner);
  const loadStatus = async () => {
    try {
      const s = await api('/api/ledger/status');
      if (!s.configured) { banner.innerHTML = '<b>Live governance not configured</b> — '
        + 'harvest + policy still available. Start the daemon with <code>-detectors</code> '
        + 'and <code>-policy</code> to enable it.'; return; }
      banner.innerHTML = '';
      const l = el('div'); l.innerHTML = '<b>anchor:</b> ' + s.anchor
        + '<br><b>honest ceiling:</b> ' + s.honest_ceiling
        + '<br><b>sessions held:</b> ' + s.sessions_held;
      l.style.fontSize = '12px'; l.style.lineHeight = '1.6'; banner.appendChild(l);
    } catch (e) { banner.textContent = '✖ ' + e.message; }
  };
  await loadStatus();

  // --- session picker + verify ---
  const row1 = el('div', 'row');
  const sessIn = el('input'); sessIn.value = ledgerSession; sessIn.style.width = '220px';
  const verifyBtn = el('button', 'btn', 'Verify chain');
  row1.append(lblWrap('session', sessIn), verifyBtn);
  main.appendChild(row1);
  const verifyOut = el('div'); verifyOut.style.cssText = 'margin:2px 0 16px;font-size:12px'; main.appendChild(verifyOut);
  const doVerify = async () => {
    ledgerSession = sessIn.value.trim() || 'console-demo';
    localStorage.setItem('cp_ledger_session', ledgerSession);
    try {
      const r = await fetch('/api/ledger/verify?session=' + encodeURIComponent(ledgerSession),
        { headers: cpHeaders() });
      const v = await r.json();
      verifyOut.innerHTML = (v.verified ? '<span class="chip st-verified">chain valid</span> '
        : '<span class="chip st-disputed">TAMPER DETECTED</span> ') + '<span style="color:var(--dim)">' + v.detail + '</span>';
    } catch (e) { verifyOut.textContent = '✖ ' + e.message; }
  };
  verifyBtn.onclick = doVerify;

  // --- drive an event (the checkpoint console) ---
  const card = el('div'); card.style.cssText =
    'border:1px solid var(--border);border-radius:8px;padding:14px;margin-bottom:8px';
  card.appendChild(el('div', 'sub', 'Observe an event — the daemon appends it to the '
    + 'chain and decides over the WHOLE session (a compound rule can fire on a tag seen earlier).'));
  const toolIn = el('input'); toolIn.placeholder = 'tool (e.g. getOrderAddress)'; toolIn.style.width = '190px';
  const destIn = el('input'); destIn.placeholder = 'destination url (optional)'; destIn.style.width = '190px';
  const textIn = el('input'); textIn.placeholder = 'text (optional — e.g. an AKIA… key)'; textIn.style.width = '230px';
  const r2 = el('div', 'row'); r2.append(lblWrap('tool', toolIn), lblWrap('destination', destIn), lblWrap('text', textIn));
  card.appendChild(r2);
  const r3 = el('div', 'row');
  const obsBtn = el('button', 'btn primary', 'Observe →');
  const presets = el('span'); presets.style.cssText = 'display:flex;gap:6px;flex-wrap:wrap';
  [['PII read', { tool: 'getOrderAddress' }], ['vendor cost', { tool: 'getVendorProduct' }],
   ['external egress', { destination: 'https://random-saas.com/upload' }],
   ['AWS key', { text: 'AKIAIOSFODNN7EXAMPLE' }]].forEach(([lbl, ev]) => {
    const b = el('button', 'btn', lbl); b.style.fontSize = '11px';
    b.onclick = () => { toolIn.value = ev.tool || ''; destIn.value = ev.destination || ''; textIn.value = ev.text || ''; };
    presets.appendChild(b);
  });
  r3.append(obsBtn, presets); card.appendChild(r3);
  main.appendChild(card);

  const out = el('div'); main.appendChild(out);
  obsBtn.onclick = async () => {
    ledgerSession = sessIn.value.trim() || 'console-demo';
    const body = { session: ledgerSession, tool: toolIn.value.trim(),
      destination: destIn.value.trim(), text: textIn.value.trim() };
    await withState(out, mkLoader('Observing…'),
      () => api('/api/ledger/observe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }),
      obs => renderObservation(out, obs));
    doVerify();
    loadStatus();
  };
  await doVerify();
}

function renderObservation(container, obs) {
  container.innerHTML = '';
  // water mark meter
  const wm = el('div'); wm.style.cssText = 'margin:10px 0';
  const mark = obs.watermark || null;
  wm.appendChild(el('span', '', 'water mark: '));
  if (mark) { const c = el('span', 'chip ' + CLASS_CHIP(mark), mark); wm.appendChild(c); }
  else wm.appendChild(el('span', 'chip st-draft', 'none — weak negative (not a clean bill)'));
  container.appendChild(wm);

  // this event's tags
  const tagLine = el('div'); tagLine.style.cssText = 'margin:6px 0;font-size:12px';
  tagLine.appendChild(el('span', '', 'event tags: '));
  if (!obs.tags || !obs.tags.length) tagLine.appendChild(el('span', 'chip st-draft', 'none'));
  else obs.tags.forEach(t => { const c = el('span', 'chip cl-observed', t.key + '=' + t.value); c.title = t.detector + ' · ' + (t.evidence || ''); c.style.marginRight = '4px'; tagLine.appendChild(c); });
  container.appendChild(tagLine);

  // session-accumulated tags (why a compound can fire)
  const st = el('div'); st.style.cssText = 'margin:6px 0 12px;font-size:12px';
  st.appendChild(el('span', '', 'session tags (accumulated): '));
  (obs.session_tags || []).forEach(t => { const c = el('span', 'chip', t.key + '=' + t.value); c.style.cssText = 'background:var(--panel2);color:var(--dim);margin-right:4px'; st.appendChild(c); });
  container.appendChild(st);

  // the decision
  const d = obs.decision || {};
  const box = el('div'); box.style.cssText =
    'border:1px solid var(--border);border-left:3px solid ' +
    (d.decision === 'block' ? 'var(--bad)' : d.decision === 'warn' ? 'var(--warn)' : 'var(--ok)') +
    ';border-radius:6px;padding:10px 12px';
  const head = el('div'); head.style.cssText = 'font-weight:600;margin-bottom:4px';
  head.textContent = (d.decision || 'allow').toUpperCase();
  if (d.mode) head.textContent += '  ·  ' + d.mode;
  box.appendChild(head);
  if (d.rule) box.appendChild(el('div', 'sub', d.rule + ' — ' + (d.message || '')));
  if (d.reach_note) { const rn = el('div'); rn.style.cssText = 'font-size:11px;color:var(--dim);margin-bottom:6px'; rn.textContent = d.reach_note; box.appendChild(rn); }
  // resolution menu — greyed items rendered dim (capability honesty)
  if (d.menu && d.menu.length) {
    const m = el('div'); m.style.cssText = 'margin-top:6px';
    m.appendChild(el('div', 'sub', 'resolution menu (greyed = not live on this channel):'));
    d.menu.forEach(item => {
      const line = el('div'); line.style.cssText = 'font-size:12px;padding:2px 0;' + (item.live ? '' : 'opacity:0.5');
      line.textContent = (item.live ? '• ' : '× ') + item.action + ' — ' + item.why;
      m.appendChild(line);
    });
    box.appendChild(m);
  }
  container.appendChild(box);
}

export { renderLedger };
