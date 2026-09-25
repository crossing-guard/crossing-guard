import { $, el, cpHeaders, api, fmtTime, normMemId, escapeHtml, mdInline, mdToHtml, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from "../ui.js";
import { openSession } from "./sessions.js";

/* ---------- usage dashboard ---------- */
async function renderUsage(container) {
  const main = container || $('#main');
  main.innerHTML = '<h2>Usage</h2><div class="sub">Observed vendor telemetry aggregated from local session files — nothing estimated, nothing phoned home.</div>';
  const wrap = el('div');
  main.appendChild(wrap);
  await withState(wrap, mkLoader('Aggregating usage across all sessions…'), () => api('/api/usage'), r => {
    const t = r.totals;
    const allIn = t.input_tokens + t.cache_read + t.cache_create;

    // stat tiles
    const tiles = el('div', 'tiles');
    const tile = (v, k, s) => { const d = el('div', 'tile'); d.append(el('div', 'v', v), el('div', 'k', k)); if (s) d.append(el('div', 's', s)); return d; };
    tiles.append(
      tile(fmtTok(allIn), 'input tokens', fmtTok(t.input_tokens) + ' fresh · ' + fmtTok(t.cache_create) + ' cache-write'),
      tile(fmtTok(t.output_tokens), 'output tokens'),
      tile(Math.round(t.cache_hit_rate * 100) + '%', 'cache hit rate', 'cache reads / all input'),
      tile(t.turns.toLocaleString(), 'turns', r.sessions + ' sessions with telemetry' + (r.no_data ? ' · ' + r.no_data + ' without' : '')),
    );
    // per-runtime tiles
    for (const [rt, u] of Object.entries(r.by_runtime)) {
      tiles.append(tile(fmtTok(u.input_tokens + u.cache_read + u.cache_create), rt + ' input',
        Math.round(u.cache_hit_rate * 100) + '% cache hit · ' + u.turns.toLocaleString() + ' turns'));
    }
    wrap.appendChild(tiles);

    // daily charts, last 30 days
    const days = Object.keys(r.days).sort();
    const last30 = days.slice(-30);
    const inputChart = el('div', 'chartcard');
    inputChart.innerHTML = '<h3>Input tokens per day</h3><div class="sub">stacked: cache read (pale) + fresh input (blue) + cache write (violet)</div>';
    const outChart = el('div', 'chartcard');
    outChart.innerHTML = '<h3>Output tokens per day</h3><div class="sub">what the models actually generated</div>';
    const inBars = el('div', 'bars'), outBars = el('div', 'bars');
    const maxIn = Math.max(1, ...last30.map(d => r.days[d].cache_read + r.days[d].input + r.days[d].cache_create));
    const maxOut = Math.max(1, ...last30.map(d => r.days[d].output));
    for (const d of last30) {
      const b = r.days[d];
      const totalIn = b.cache_read + b.input + b.cache_create;
      const day = el('div', 'day');
      day.title = d + '\n' + fmtTok(b.cache_read) + ' cache read\n' + fmtTok(b.input) + ' fresh input\n' + fmtTok(b.cache_create) + ' cache write\n' + b.turns + ' turns';
      const mk = (cls, v) => { const s = el('div', 'seg ' + cls); s.style.height = (v / maxIn * 100) + '%'; return s; };
      // order top→bottom: cache write, fresh, cache read
      day.append(mk('cc', b.cache_create), mk('in', b.input), mk('cr', b.cache_read));
      day.style.height = Math.max(2, totalIn / maxIn * 100) + '%';
      inBars.appendChild(day);
      const od = el('div', 'day');
      od.title = d + '\n' + fmtTok(b.output) + ' output · ' + b.turns + ' turns';
      const os = el('div', 'seg out'); os.style.height = '100%';
      od.appendChild(os);
      od.style.height = Math.max(2, b.output / maxOut * 100) + '%';
      outBars.appendChild(od);
    }
    const axis = days => { const a = el('div', 'axis'); a.append(el('span', '', last30[0] || ''), el('span', '', last30[last30.length - 1] || '')); return a; };
    inputChart.append(inBars, axis(), (() => { const l = el('div', 'legend');
      l.innerHTML = '<span><span class="sw seg cr"></span>cache read</span><span><span class="sw seg in"></span>fresh input</span><span><span class="sw seg cc"></span>cache write</span>';
      return l; })());
    outChart.append(outBars, axis());
    wrap.append(inputChart, outChart);

    // counterfactual — labeled illustrative, never presented as a bill
    const cfFull = allIn / 1e6 * 15 + t.output_tokens / 1e6 * 75;
    const cfReal = t.cache_read / 1e6 * 1.5 + t.input_tokens / 1e6 * 15 + t.cache_create / 1e6 * 18.75 + t.output_tokens / 1e6 * 75;
    wrap.appendChild(el('div', 'banner',
      'Illustrative only: at opus-class API list rates this traffic would be ~$' + Math.round(cfFull).toLocaleString() +
      ' uncached vs ~$' + Math.round(cfReal).toLocaleString() + ' with caching. Subscription usage — not a bill; shows what the cache is doing for you.'));

    // top sessions tables
    const topTable = (title, rows, valKey, valFmt) => {
      const card = el('div', 'chartcard');
      card.innerHTML = '<h3>' + title + '</h3>';
      const tb = el('table');
      tb.innerHTML = '<tr><th>Session</th><th>Runtime</th><th>' + valKey + '</th><th>Cache hit</th><th>When</th></tr>';
      for (const s of rows) {
        const tr = el('tr');
        const td1 = el('td', '', s.title || s.id); td1.style.cursor = 'pointer'; td1.style.color = 'var(--accent2)';
        td1.onclick = () => { document.querySelector('nav button[data-view="sessions"]').click(); setTimeout(() => openSession(s, null), 300); };
        tr.appendChild(td1);
        const td2 = el('td'); td2.appendChild(el('span', 'chip ' + s.runtime, s.runtime)); tr.appendChild(td2);
        tr.appendChild(el('td', '', valFmt(s)));
        tr.appendChild(el('td', '', s.hit_rate ? Math.round(s.hit_rate * 100) + '%' : '—'));
        tr.appendChild(el('td', '', fmtTime(s.modified)));
        tb.appendChild(tr);
      }
      card.appendChild(tb);
      return card;
    };
    wrap.appendChild(topTable('Largest context (last turn) — handoff candidates', r.top_context, 'Context', s => fmtTok(s.context)));
    wrap.appendChild(topTable('Heaviest sessions (total tokens)', r.top_total, 'Total', s => fmtTok(s.total)));
  });
}

export { renderUsage };
