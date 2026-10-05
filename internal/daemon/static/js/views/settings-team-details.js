// Settings → Team: Details, collapsed (team rest-of-release plan §4.2 part 5, §14 Q34).
// Identifiers, fingerprints, addresses and cadences live here and nowhere else on the
// page, with every fact the earlier page showed that the default view no longer does.
import { el } from "../core.js";
import { teamDetails } from "./settings-team-model.js";
import { memoryDeletions } from "./settings-team-memory.js";

function meterNode(meter) {
  const node = el('div', 'settings-meter' + (meter.over ? ' over' : ''));
  const fill = el('i');
  fill.style.width = Math.min(100, Math.round(meter.pending / meter.mark * 100)) + '%';
  node.appendChild(fill);
  node.setAttribute('role', 'img');
  node.setAttribute('aria-label', meter.pending + ' waiting; high-water mark ' + meter.mark);
  return node;
}

function detailsGrid(details) {
  const grid = el('div', 'agents-kv');
  for (const row of details.rows) {
    const value = el('div', 'team-detail-value');
    for (const line of row.lines) value.appendChild(row.mono ? el('code', 'agents-mono', line) : el('div', '', line));
    if (row.meter) value.appendChild(meterNode(row.meter));
    grid.append(el('span', 'agents-kv-k', row.label), value);
  }
  if (details.document) {
    const shown = el('details', 'settings-disclosure');
    shown.append(el('summary', '', 'Show'), el('pre', 'agents-source team-source', details.document));
    grid.append(el('span', 'agents-kv-k', 'Last document sent'), shown);
  }
  return grid;
}

// teamDetailsPart draws Details closed. Opening it reads the memory deletions (the
// daemon asks the team server) and stays open across the page's repaints.
export function teamDetailsPart(view) {
  const node = el('details', 'settings-disclosure team-details');
  const body = el('div', 'team-disclosed');
  const draw = deletions => body.replaceChildren(detailsGrid(teamDetails(view.status, view.layers, deletions)));
  draw(null);
  node.append(el('summary', '', 'Details'), body);
  const linked = view.model.mode === 'linked';
  let asked = false;
  const opened = async () => {
    view.page.dataset.details = node.open ? 'open' : '';
    if (!node.open || asked || !linked) return;
    asked = true;
    const deletions = await memoryDeletions();
    if (deletions && node.isConnected) draw(deletions);
  };
  node.addEventListener('toggle', opened);
  node.open = view.page.dataset.details === 'open';
  return node;
}
