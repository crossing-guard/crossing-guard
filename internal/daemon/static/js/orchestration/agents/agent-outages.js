// Provider outages on the Agents index (provider-outage plan Slice D): one
// banner per tripped route — since when, the provider's own words, the runs
// parked on it — and the preview → confirm reroute along each run's declared
// fallback chain. Stale helper replies become drafts, never late sends. The
// banner leads with the model route's name and shows no model id (team
// rest-of-release plan §14 Q9); the runtime and model stay the key the reroute
// request names, which is not shown.
import { el, button, row, spacer, problem, errorText } from './agent-ui.js';
import { confirmDialog } from './agent-dialog.js';
import { rerouteSummary } from './roster-model.js';
import { outageWords, outageReported } from './route-model.js';
import { modelsLink } from './route-picker.js';
import { rerouteParkedPreview, rerouteParkedApply } from '../agents-api.js';

export function renderOutages(host, outages, page) {
  for (const outage of outages || []) host.appendChild(outageBanner(outage, page));
}

function outageBanner(outage, page) {
  const since = outage.tripped_at ? new Date(outage.tripped_at * 1000).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }) : null;
  const node = el('div', 'agents-outage');
  const status = el('div');
  const reroute = button('Reroute parked runs\u2026', '', () => previewReroute(outage, page, reroute, status));
  node.append(row(el('b', '', outageWords(outage, since)), spacer(), modelsLink(), reroute));
  const reported = outageReported(outage);
  if (reported) {
    const more = node.appendChild(document.createElement('details'));
    more.className = 'agents-sub agents-outage-reported';
    more.append(el('summary', '', reported.summary), el('div', '', reported.text));
  }
  node.appendChild(status);
  return node;
}

async function previewReroute(outage, page, reroute, status) {
  status.replaceChildren();
  reroute.disabled = true;
  let plan;
  try {
    plan = await rerouteParkedPreview(outage.runtime, outage.model || '');
  } catch (error) {
    status.replaceChildren(problem(errorText(error, 'Reroute preview failed')));
    return;
  } finally {
    reroute.disabled = false;
  }
  confirmDialog('Reroute parked runs', rerouteSummary(plan).words, 'Reroute', async () => {
    const applied = await rerouteParkedApply(outage.runtime, outage.model || '', plan.preview_token);
    await page.navigate({ view: 'index', notice: 'Rerouted ' + Number(applied.rerouted || 0) + '; '
      + Number(applied.stale_drafts || 0) + ' held as drafts; ' + Number(applied.no_route || 0) + ' still parked without a fallback.' });
  });
}
