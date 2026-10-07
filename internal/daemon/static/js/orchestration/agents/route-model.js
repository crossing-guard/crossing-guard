// Pure view models for what an agent surface may say about a model route and
// about a shared agent (team rest-of-release plan §4.1 decisions 4–5, §5.5, §5.8,
// §14 Q5/Q8/Q9/Q12/Q30). An agent surface shows a route's NAME and whether it is
// on this machine or leaves it — never a model id, an endpoint, or the name of
// what runs it. Nothing here copies those fields of a place or a route into what
// it returns (an outage's model id is read only to take it OUT of the provider's
// words), so nothing built from these view models can show them. Nothing fetches
// or touches the DOM.

/* ---------- routes ---------- */

export const ON_THIS_MACHINE = 'on this machine';
export const LEAVES_THIS_MACHINE = 'leaves this machine';

export const ROUTE_FAMILY_MANAGED = 'runtime-model';
export const ROUTE_FAMILY_REVIEW = 'inference';

export function localityWords(local) {
  return local ? ON_THIS_MACHINE : LEAVES_THIS_MACHINE;
}

// routeFamilyFor is the route family an agent's lane takes.
export function routeFamilyFor(lane) {
  return lane === 'review' ? ROUTE_FAMILY_REVIEW : ROUTE_FAMILY_MANAGED;
}

const ROUTE_MISSING = 'route_missing';
const MIGRATION_FAILED = 'migration_failed';

export const NO_ROUTE_YET = 'Not on a route yet · runs as it did before';
export const NO_ROUTE_FOR_AGENT = 'No model route for this agent yet';

// routeFacts is everything an agent surface may show of a place's route:
// { name, locality, problem, short }. `name` is the route's name, or the words
// for a place whose route is missing or that has no route yet; `locality` is ''
// when the page has nothing true to say; `problem` is '' or the typed problem;
// `short` is the one-line form for a tight spot (a table cell, a summary).
export function routeFacts(place) {
  if (!place) return { name: '', locality: '', problem: '', short: '' };
  if (place.route_problem === ROUTE_MISSING) {
    const words = place.lane === 'review' ? 'Missing — asks go to you' : 'Missing — this place starts no run';
    return { name: words, locality: '', problem: ROUTE_MISSING, short: 'Model route missing' };
  }
  if (place.route_problem === MIGRATION_FAILED || !place.route_id) {
    return { name: NO_ROUTE_YET, locality: '', problem: MIGRATION_FAILED, short: 'Not on a route yet' };
  }
  const name = String(place.route_name || 'A model route');
  return { name, locality: localityWords(Boolean(place.route_local)), problem: '', short: name };
}

// needsRoute says whether a place should offer "Choose route…".
export function needsRoute(place) {
  return Boolean(routeFacts(place).problem);
}

// fallbackFacts lists a place's fallback chain by route name, in chain order.
export function fallbackFacts(place = {}) {
  return (place.fallback_routes || []).map(entry => {
    if (entry.missing) return { id: String(entry.route_id || ''), name: 'A missing route (passed over)', locality: '', mode: String(entry.mode || '') };
    if (!entry.route_id) return { id: '', name: 'Not on a route yet', locality: '', mode: String(entry.mode || '') };
    return { id: String(entry.route_id), name: String(entry.route_name || 'A model route'),
      locality: localityWords(Boolean(entry.route_local)), mode: String(entry.mode || '') };
  });
}

// routeKey and fallbackKey compare two places' route choices by reference, so a
// difference is noticed without reading what a route resolves to.
export function routeKey(place = {}) {
  return [place.route_id || '', place.mode || ''].join('\u0000');
}

export function fallbackKey(place = {}) {
  return (place.fallback_routes || []).map(entry => (entry.route_id || '') + '\u0000' + (entry.mode || '')).join(',');
}

// routeChoices is the Model route list for one family: each route by name with
// its locality words and the read-only modes a place may pick with it. `modesFor`
// maps a route to its modes (the caller holds the capabilities); the choice
// carries only what the control shows.
export function routeChoices(routes = [], family = ROUTE_FAMILY_MANAGED, modesFor = () => []) {
  return routes.filter(route => route.family === family && route.route_id)
    .map(route => ({ id: String(route.route_id), name: String(route.name || ''), locality: localityWords(Boolean(route.local)),
      modes: modesFor(route).map(mode => ({ id: String(mode.id), label: String(mode.label || 'Read only') })) }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

/* ---------- outages ---------- */

// outageWords names one outage by the model routes it stops and what waits on
// it. It never names a model: the model is one click away on Settings → Models.
export function outageWords(outage = {}, at = null) {
  const names = (outage.route_names || []).map(String).filter(Boolean);
  const subject = names.length ? names.join(', ') : 'A model your agents use';
  const parked = Number(outage.parked || 0);
  return subject + (names.length > 1 ? ' are' : ' is') + ' not answering' + (at ? ' since ' + at : '') + '. '
    + (parked === 1 ? '1 run is parked.' : parked + ' runs are parked.');
}

// outageDetail is the provider's own words for an outage, with the model's id
// taken out wherever the provider repeated it. The provider may still name a
// runtime or an address in them, and nothing here can know every such word, so
// these words are never drawn on the banner itself (§14 Q9): outageReported puts
// them behind a disclosure.
export function outageDetail(outage = {}) {
  const detail = String(outage.detail || '');
  const model = String(outage.model || '');
  return model ? detail.split(model).join('the model') : detail;
}

// outageReported is the banner's disclosure: closed, it says only that the
// provider reported something; opened, it shows the provider's words. null when
// the provider said nothing.
export function outageReported(outage = {}) {
  const text = outageDetail(outage);
  return text ? { summary: 'What the provider reported', text } : null;
}

/* ---------- shared agents ---------- */

export function shortDate(value) {
  const at = typeof value === 'number' ? value * 1000 : Date.parse(String(value || ''));
  if (!Number.isFinite(at) || !at) return '';
  return new Date(at).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' });
}

function organizationWords(origin) {
  return String(origin?.organization_name || 'your team');
}

// originView is a shared agent as its pages say it, or null for the member's own
// agent: the mark beside its name, whether its definition is read-only (OD-21),
// and its facts — one per row.
export function originView(agent = {}, now = Date.now()) {
  const origin = agent.origin;
  if (!origin) return null;
  const organization = organizationWords(origin);
  const released = Boolean(origin.released);
  const expires = shortDate(origin.expires_at);
  const expired = Boolean(expires) && Date.parse(origin.expires_at) < now;
  const facts = [[released ? 'Was shared by' : 'Shared by', organization]];
  if (origin.scope && origin.scope !== 'organization') facts.push(['Shared for', String(origin.scope).replace(/^repository:/, 'repository ')]);
  if (origin.revision) facts.push(['Bundle revision', String(origin.revision)]);
  if (expires && !released) facts.push([expired ? 'Expired' : 'Shared until', expires]);
  return { organization, released, readOnly: Boolean(origin.read_only) || released, expired,
    mark: released ? 'no longer shared by ' + organization : 'from ' + organization, facts };
}

// collisionView is said on a member's own agent whose id an offered team agent
// uses (OD-6, Q5), or null.
export function collisionView(agent = {}) {
  if (!agent.collision) return null;
  const organization = String(agent.collision.organization_name || 'Your team');
  return { organization, mark: 'id also used by ' + organization,
    banner: organization + ' shares an agent with the id ' + String(agent.profile_id || '')
      + '. It will not be adopted — your agent uses the id. This agent is unchanged.' };
}

const HOLD_WORDS = Object.freeze({
  adoption_expired: origin => 'held — ' + organizationWords(origin) + '’s bundle expired'
    + (shortDate(origin?.expires_at) ? ' ' + shortDate(origin.expires_at) : ''),
  version_no_longer_shared: () => 'this version is no longer shared',
  adoption_ended: origin => 'held — no longer shared by ' + organizationWords(origin),
});

// holdWords says why a place of a shared agent starts no run while it stays on.
export function holdWords(reason, origin = null) {
  if (!reason) return '';
  return (HOLD_WORDS[reason] || (() => 'held — ' + String(reason).replaceAll('_', ' ')))(origin);
}

// refusalWords says why the last run start was refused.
export function refusalWords(code, origin = null) {
  if (!code) return '';
  if (code === 'admission_refused') return 'A rule on this device refused its model route';
  if (code === ROUTE_MISSING) return 'Its model route is missing';
  return holdWords(code, origin);
}

// placeStatus is the one state a place shows: { state, label, reason }. A place
// that is on and starts no run says so before it says "On".
export function placeStatus(place = {}, origin = null) {
  if (place.state !== 'enabled') return { state: 'off', label: 'Off', reason: '' };
  if (place.held_reason) return { state: 'attn', label: 'Held', reason: holdWords(place.held_reason, origin) };
  if (place.route_problem === ROUTE_MISSING) return { state: 'attn', label: 'Route missing', reason: routeFacts(place).name };
  if (place.run_refusal) return { state: 'attn', label: 'Not running', reason: refusalWords(place.run_refusal, origin) };
  return { state: 'on', label: 'On', reason: '' };
}

// stoppedWords are the attention words for an agent with a place that is on and
// starts no run; '' when every enabled place runs.
export function stoppedWords(agent = {}) {
  for (const place of agent.places || []) {
    const status = placeStatus(place, agent.origin);
    if (status.state === 'attn') return status.reason.charAt(0).toUpperCase() + status.reason.slice(1);
  }
  return '';
}

/* ---------- sharing with the team ---------- */

const DOCUMENT_CHANGE_WORDS = Object.freeze({ added: 'added', removed: 'removed', changed: 'changed', unchanged: 'unchanged' });

// shareView is what the Share with team… result shows: the two remaining acts
// (sign, upload) and the diff against the published revision.
export function shareView(built = {}, serverURL = '') {
  const documents = (built.documents || []).map(document => ({
    kind: String(document.kind || ''), name: String(document.profile_id || document.name || ''),
    profileID: String(document.profile_id || ''), change: DOCUMENT_CHANGE_WORDS[document.change] || String(document.change || ''),
    removed: document.change === 'removed', diff: document.text_diff || [],
  }));
  const rules = built.rule_changes || {};
  const published = Number(built.published_revision || 0);
  return {
    title: 'Revision ' + Number(built.revision || 0) + ' is built',
    against: published ? 'Revision ' + published : 'Nothing published yet',
    path: String(built.path || ''), signCommand: String(built.sign_command || ''), signedPath: String(built.signed_path || ''),
    uploadURL: serverURL ? String(serverURL).replace(/\/+$/, '') + '/policy' : '',
    expires: shortDate(built.expires_at), documents,
    rules: { added: rules.added || [], removed: rules.removed || [], changed: (rules.changed || []).map(change => String(change.id || change.ID || '')) },
    changes: (built.changes || []).map(change => ({ label: String(change.label || change.field || ''), from: String(change.from || ''), to: String(change.to || '') })),
  };
}
