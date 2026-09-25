import { $, el, cpHeaders, api, fmtTime, normMemId, escapeHtml, mdInline, mdToHtml, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from "../ui.js";
import { S } from "../state.js";
import { provider } from "../infopanel.js";
import { hidePaneHost, showPaneHost } from "../pane-host.js";

let skillsRepo = localStorage.getItem('cp_skills_repo') || '';
let lastReport = null; // full /api/skills payload (probes live here, shared by overview + detail)

const RUNTIMES = ['claude', 'codex', 'opencode'];
const RT_CHIP = { claude: 'claude', codex: 'codex', opencode: 'cl-observed' };
const CELL_CHIP = {
  'visible': 'st-verified', 'not-synced': 'st-draft', 'disabled': 'st-stale',
  'would-reject': 'st-stale', 'hidden-by-deny': 'st-stale',
  'dropped-invalid': 'st-disputed', 'vendor-managed': 'cl-observed', 'not-installed': 'st-draft',
};

/* ---------- right reference panel providers (surface 'skills', console-design §5) ----------
   Read-only coverage/grant summary that informs the center inventory. */
provider({
  id: 'skill.coverage', order: 10, title: 'Coverage',
  match: ctx => ctx.surface === 'skills' && !!ctx.selection,
  render: (ctx, box) => {
    const sk = ctx.selection;
    for (const rt of RUNTIMES) {
      const c = sk.coverage[rt] || { state: 'unknown', grade: 'fs' };
      const row = el('div', 'row'); row.style.marginBottom = '6px';
      row.append(el('span', 'chip ' + (RT_CHIP[rt] || 'st-draft'), rt),
        el('span', 'chip ' + (CELL_CHIP[c.state] || 'st-draft'), c.state),
        el('span', 'chip st-draft', c.grade));
      box.appendChild(row);
      if (c.why) box.appendChild(el('div', 'sub', c.why));
    }
  },
});
provider({
  id: 'skill.grant', order: 20, title: 'Grant (allowed-tools)',
  match: ctx => ctx.surface === 'skills' && !!ctx.selection,
  render: (ctx, box) => {
    const sk = ctx.selection;
    if (!(sk.allowed_tools || []).length) { box.appendChild(el('div', 'sub', 'No grant — loading this skill pre-approves nothing.')); return; }
    box.appendChild(el('div', 'sub', 'Pre-approves these tools when the skill is loaded:'));
    for (const t of sk.allowed_tools) box.appendChild(el('span', 'chip st-stale', t));
  },
});

/* ---------- skills: index in the RAIL, detail/overview in the CENTER (§5) ---------- */
async function renderSkills() {
  S.selSkill = null; // fresh visit starts with no skill selected
  const main = $('#main'), side = $('#sidebody');
  main.innerHTML = '<h2>Skills</h2><div class="sub">Third portability axis (memory, rules, skills). Makes silent partial coverage visible — read-only v0; fixes render as diffs to copy, never auto-applied.</div>';
  const center = el('div'); main.appendChild(center);
  side.innerHTML = '';

  // --- rail: repo-scope / rescan pin ---
  const rescanPin = el('div', 'railpin');
  rescanPin.innerHTML = '<svg class="ico" viewBox="0 0 16 16" aria-hidden="true"><path d="M13.5 8a5.5 5.5 0 1 1-1.6-3.9M13.5 2.5V5H11"/></svg><span>Rescan…</span>';
  rescanPin.title = 'Set an optional repo dir to add .agents/.claude/.opencode repo scopes';
  rescanPin.onclick = () => showRescan();
  side.appendChild(rescanPin);

  const listBox = el('div'); side.appendChild(listBox);

  const load = () => withState(listBox, mkLoader('Scanning skill directories…'),
    () => api('/api/skills?repo=' + encodeURIComponent(skillsRepo)), r => { lastReport = r; renderList(); showOverview(); });
  await load();

  function renderList() {
    listBox.innerHTML = '';
    const skills = (lastReport && lastReport.skills) || [];
    if (!skills.length) { listBox.appendChild(el('div', 'empty', 'No skills found. Skills are SKILL.md directories — see agentskills.io.')); return; }
    for (const sk of skills) {
      const row = el('div', 'sess'); row.dataset.skill = sk.name;
      row.appendChild(el('div', 't', sk.name));
      const meta = el('div', 'm');
      for (const rt of RUNTIMES) {
        const c = sk.coverage[rt] || { state: 'unknown' };
        meta.appendChild(el('span', 'chip ' + (CELL_CHIP[c.state] || 'st-draft'), rt[0])).title = rt + ': ' + c.state;
      }
      if (sk.drift) meta.appendChild(el('span', 'chip st-disputed', 'drift'));
      row.appendChild(meta);
      row.onclick = () => showSkill(sk, row);
      listBox.appendChild(row);
    }
  }

  function showRescan() {
    S.selSkill = null; hidePaneHost();
    listBox.querySelectorAll('.sess').forEach(x => x.classList.remove('sel'));
    center.innerHTML = '';
    center.appendChild(el('h3', '', 'Repo scope'));
    center.appendChild(el('div', 'sub', 'Optional. Adds repo-local .agents / .claude / .opencode skill scopes on top of the home-dir scan.'));
    const repoIn = el('input'); repoIn.placeholder = 'repo dir (optional)'; repoIn.style.cssText = 'width:100%;max-width:520px;margin:8px 0'; repoIn.value = skillsRepo;
    const rescan = el('button', 'btn primary', 'Rescan');
    rescan.onclick = async () => {
      skillsRepo = repoIn.value.trim(); localStorage.setItem('cp_skills_repo', skillsRepo);
      await load();
    };
    center.append(repoIn, el('div', 'row', ''));
    center.lastChild.appendChild(rescan);
  }

  // Default center: the flagship coverage matrix + per-runtime probe controls.
  function showOverview() {
    if (S.selSkill) return; // a selection is already showing detail
    center.innerHTML = '';
    const r = lastReport || {};
    if (!r.skills || !r.skills.length) {
      center.appendChild(el('div', 'empty', 'No skills found in ~/.agents/skills, ~/.claude/skills, ~/.config/opencode/skills, or ~/.codex/skills' + (r.repo_dir ? ' (or repo scopes under ' + r.repo_dir + ')' : '') + '.'));
      return;
    }
    center.appendChild(el('div', 'banner',
      'Advisory: coverage is derived from files [fs] and vendor config [config] unless a live probe [probed] confirms it. Select a skill for its detail; governance cells compile to nothing until the rules engine wires skill.load.'));
    center.appendChild(el('h3', '', 'Coverage matrix'));
    const table = el('table', 'grid');
    const head = el('tr');
    head.appendChild(el('th', '', 'skill ↓ / runtime →'));
    for (const rt of RUNTIMES) {
      const th = el('th'); th.append(rt + ' ');
      if (rt !== 'claude') {
        const pb = el('button', 'btn', 'probe'); pb.style.cssText = 'font-size:10px;padding:2px 8px;';
        pb.title = rt === 'codex' ? 'Spawns codex app-server → skills/list (seconds)' : 'Runs opencode debug skill';
        pb.onclick = async () => {
          pb.disabled = true; pb.textContent = 'probing…';
          try { lastReport = await api('/api/skills/probe', { method: 'POST', body: JSON.stringify({ runtime: rt, repo: skillsRepo }) });
            renderList(); showOverview();
          } catch (err) { pb.textContent = 'probe'; pb.disabled = false; alert('Probe failed: ' + (err.message || err)); }
        };
        th.appendChild(pb);
      } else th.appendChild(el('span', 'chip st-draft', 'fs only'));
      const pi = (r.probes || {})[rt];
      if (pi) th.appendChild(el('div', 'sub', pi.ok ? '[probed] ' + fmtTime(pi.at) : '✖ ' + (pi.err || '').slice(0, 60)));
      head.appendChild(th);
    }
    table.appendChild(head);
    for (const sk of r.skills) {
      const tr = el('tr');
      const th = el('th', '', sk.name); th.style.cursor = 'pointer';
      th.onclick = () => { const row = listBox.querySelector('[data-skill="' + CSS.escape(sk.name) + '"]'); showSkill(sk, row); };
      if (sk.drift) th.appendChild(el('span', 'chip st-disputed', 'drift'));
      tr.appendChild(th);
      for (const rt of RUNTIMES) {
        const c = sk.coverage[rt] || { state: 'unknown', grade: 'fs' };
        const td = el('td');
        const chip = el('span', 'chip ' + (CELL_CHIP[c.state] || 'st-draft'), c.state); chip.title = c.why || '';
        td.append(chip, el('span', 'chip st-draft', c.grade));
        tr.appendChild(td);
      }
      table.appendChild(tr);
    }
    const wrap = el('div'); wrap.style.overflowX = 'auto'; wrap.appendChild(table); center.appendChild(wrap);
  }

  function showSkill(sk, rowEl) {
    S.selSkill = sk; showPaneHost({ surface: 'skills', selection: sk, api });
    listBox.querySelectorAll('.sess').forEach(x => x.classList.remove('sel'));
    if (rowEl) rowEl.classList.add('sel');
    center.innerHTML = '';
    const h = el('h3', '', sk.name);
    if (sk.drift) h.appendChild(el('span', 'chip st-disputed', 'drift'));
    center.appendChild(h);
    if (sk.description) {
      center.appendChild(el('div', 'sub', sk.description));
      center.appendChild(el('div', 'sub', 'description: ' + sk.description.length + ' / 1536 chars' + (sk.description.length > 1536 ? ' — TRUNCATED in Claude listings' : '')));
    }

    // coverage detail (per runtime, with why/fix)
    center.appendChild(el('h2', '', 'Coverage'));
    for (const rt of RUNTIMES) {
      const c = sk.coverage[rt] || { state: 'unknown', grade: 'fs' };
      const row = el('div', 'row'); row.style.marginBottom = '4px';
      row.append(el('span', 'chip ' + (RT_CHIP[rt] || 'st-draft'), rt),
        el('span', 'chip ' + (CELL_CHIP[c.state] || 'st-draft'), c.state),
        el('span', 'chip st-draft', c.grade));
      center.appendChild(row);
      if (c.why) center.appendChild(el('div', 'sub', c.why));
      if (c.fix) { const f = el('div', 'sub'); f.appendChild(el('code', '', 'fix: ' + c.fix)); center.appendChild(f); }
    }

    // locations
    center.appendChild(el('h2', '', 'Locations'));
    for (const e of sk.entries) {
      const line = el('div', 'sub');
      line.append(el('span', 'chip ' + (e.dir_class === 'claude' ? 'claude' : e.dir_class === 'codex' ? 'codex' : 'cl-observed'), e.dir_class + '/' + e.scope), ' ');
      line.append((e.symlink ? '⇢ ' : '') + e.path.replace(/^\/Users\/[^/]+/, '~') + (e.symlink && e.target ? ' → ' + e.target.replace(/^\/Users\/[^/]+/, '~') : ''));
      line.append(' · #' + e.hash + ' · ' + e.files + ' files');
      if (e.vendor_managed) line.appendChild(el('span', 'chip cl-observed', 'vendor-managed'));
      center.appendChild(line);
    }

    // lints
    const lints = sk.entries.flatMap(e => e.lints || []);
    center.appendChild(el('h2', '', 'Lints'));
    if (lints.length) for (const l of [...new Set(lints)]) center.appendChild(el('div', 'sub', '⚠ ' + l));
    else center.appendChild(el('span', 'chip st-verified', 'clean'));
  }
}

export { renderSkills };
