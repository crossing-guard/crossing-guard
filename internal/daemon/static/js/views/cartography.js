// L3 cartography — the dossier the north star asks for: where a file sits, how
// big and new and coupled it is, who depends on it (console-and-info-panel §7 L3,
// the five axes of "place" in §5).
//
// This provider renders whatever descriptor the analyzer port yields and NEVER
// knows which language produced it. There is no branch on `go` or any other
// language anywhere in this file — the language is a value it prints, exactly
// like LOC. That is the framework property; adding a language changes nothing
// here.
//
// Every value carries its provenance, because a role that was INFERRED must not
// read like a line count that was MEASURED (§6).
import { el, api } from '../core.js';
import { provider } from '../infopanel.js';
import * as refs from '../refs.js';

const isCodeRef = ctx => ctx.surface === 'ref' && !!ctx.selection?.path &&
  ctx.selection.kind !== 'id' && !String(ctx.selection.path).endsWith('.md');

provider({
  id: 'file.cartography',
  order: 5,
  zone: 'primary',
  title: 'Cartography',
  match: isCodeRef,
  render: async (ctx, body) => {
    const t = ctx.selection;
    let rep;
    try {
      rep = await api('/api/codemap/descriptor?cwd=' + encodeURIComponent(t.root || refs.projectRoot() || '') +
        '&path=' + encodeURIComponent(t.path));
    } catch (e) {
      body.appendChild(el('div', 'sub', 'cartography unavailable — ' + (e.message || e)));
      return;
    }
    const d = rep.descriptor;
    if (!d) {
      // No adapter claimed this file. That is a normal, statable condition —
      // not an empty panel that looks like the file has no properties.
      body.appendChild(el('div', 'sub', rep.note || 'no descriptor for this file'));
      body.appendChild(el('div', 'sub',
        'languages bound: ' + ((rep.languages || []).join(', ') || 'none')));
      return;
    }
    const P = d.provenance || {};

    // Axis 1 — physical.
    section(body, 'place', [
      ['path', d.identity.path],
      ['package', d.identity.namespace || '—'],
      ['language', d.identity.language || '—'],
    ], P, { path: 'identity.path', package: 'identity.namespace', language: 'identity.language' });

    // Axis 2 — architectural role. Shown WITH what assigned it: a role is a
    // judgement, and an unattributed judgement reads as a fact.
    const roleRow = [['role', d.role.layer || 'unknown']];
    if (d.role.rule) roleRow.push(['rule', d.role.rule]);
    section(body, 'role', roleRow, P, { role: 'role.layer' });
    if (d.role.why) body.appendChild(el('div', 'cartowhy', d.role.why));

    // Axis 3 — call graph / coupling.
    section(body, 'coupling · package ' + (d.identity.namespace || '?'), [
      ['callers (Ca)', String(d.structure.fan_in)],
      ['depends on (Ce)', String(d.structure.fan_out)],
      ['instability I', fmt(d.metrics.instability)],
      ['abstractness A', fmt(d.metrics.abstractness)],
      ['distance D', fmt(d.metrics.distance)],
    ], P, { 'callers (Ca)': 'structure.fan_in', 'depends on (Ce)': 'structure.fan_out',
            'instability I': 'metrics.instability', 'abstractness A': 'metrics.abstractness',
            'distance D': 'metrics.distance' });
    // State what D means rather than letting a bare number imply a verdict.
    // A high D on a stable concrete unit (a persistence floor) is expected, not
    // a defect — presenting it as one would be the guess-as-measurement failure.
    body.appendChild(el('div', 'cartowhy',
      'A, I and D are defined over the PACKAGE, not this file — a file with no type ' +
      'declarations has no abstractness while its package certainly does. D is distance ' +
      'from Martin’s main sequence (|A + I − 1|); read it against the layer, since a ' +
      'concrete, widely-imported floor sits far from the sequence by design.'));

    // Axis 4 — size and evolution.
    section(body, 'size · change', [
      ['lines', String(d.metrics.loc)],
      ['branches', d.metrics.cyclomatic ? String(d.metrics.cyclomatic) : '—'],
      ['commits', String(d.evolution.commits)],
      ['new?', d.evolution.is_new ? 'yes' : 'no'],
    ], P, { lines: 'metrics.loc', branches: 'metrics.cyclomatic', commits: 'evolution.commits',
            'new?': 'evolution.is_new' });

    // Axis 5 — intent — is intentionally NOT rendered. The server stopped
    // populating d.intent (the only implementation quoted an arbitrary doc
    // mention as if it were a statement of intent — see the tombstone in
    // internal/daemon/codemap.go). Re-add the render here when intent returns
    // from a real declaration; leaving a permanently-false branch in the render
    // path only misleads the next reader into thinking it is live.

    // The public surface, capped — this is a reader, not a symbol browser.
    if (d.symbol?.exports?.length) {
      body.appendChild(el('div', 'infolbl', `exports · ${d.symbol.exports.length}`));
      body.appendChild(el('div', 'cartoexports', d.symbol.exports.slice(0, 24).join(', ') +
        (d.symbol.exports.length > 24 ? ' …' : '')));
    }

    // Callers/callees as a LIST first (§4: no mermaid, no WASM — a list covers
    // most of the need and stays inside the embed budget).
    if (d.structure.imports?.length) {
      body.appendChild(el('div', 'infolbl', `depends on · ${d.structure.imports.length}`));
      for (const imp of d.structure.imports.slice(0, 12)) {
        body.appendChild(el('div', 'cartodep', '↓ ' + imp));
      }
    }

    // What could NOT be determined, and why. Never silent.
    for (const n of d.notes || []) body.appendChild(el('div', 'sub', '⚠ ' + n));
    const foot = el('div', 'sub',
      'config: ' + (rep.config || 'none') + ' · languages: ' + (rep.languages || []).join(', ') +
      ' · at source ' + (d.source_hash || '?'));
    body.appendChild(foot);
  },
});

// section renders key/value rows, each tagged with the provenance of its value.
//
// A field with NO provenance was not computed, and its zero value is not a
// measurement. Printing "0.00" for an abstractness the descriptor explicitly
// declined to compute is the exact guess-as-fact failure the provenance map
// exists to prevent — so an untagged field renders "—", never a number.
function section(host, label, rows, provenance, fieldMap) {
  host.appendChild(el('div', 'infolbl', label));
  for (const [k, v] of rows) {
    const field = fieldMap?.[k];
    const p = field && provenance[field];
    const r = el('div', 'refrow');
    r.appendChild(el('span', 'refrow-k', k));
    r.appendChild(el('span', 'refrow-v', (field && !p) ? '—' : v));
    if (p) r.appendChild(el('span', 'prov prov-' + p, p));
    host.appendChild(r);
  }
}

const fmt = n => (typeof n === 'number' && isFinite(n)) ? n.toFixed(2) : '—';
