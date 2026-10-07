// Pure view models for Model routes on Settings → Models (team rest-of-release
// plan §5.5, §14 Q10/Q11). This is the one page that shows what a route resolves
// to — its model id and its endpoint. Runtime display names and model labels
// arrive as data; nothing fetches, nothing touches the DOM.

export const FAMILY_MANAGED = 'runtime-model';
export const FAMILY_REVIEW = 'inference';

// The owner's words for the two families (Q10).
export const FAMILY_WORDS = Object.freeze({
  [FAMILY_MANAGED]: 'Agents that watch or help in sessions',
  [FAMILY_REVIEW]: 'The reviewer (a model on this machine)',
});

export function familyWords(family) {
  return FAMILY_WORDS[family] || String(family || '');
}

function effortWords(effort) {
  if (!effort) return '';
  return 'effort ' + (effort.kind === 'inherit' ? 'Default' : String(effort.value || ''));
}

// routeRunsOn says what a route resolves to, one fact per entry: each is drawn as
// its own line, never joined. Shown on Settings → Models only.
export function routeRunsOn(route = {}, runtimeNames = {}, modelLabels = {}) {
  const fields = route.fields || {};
  if (route.family === FAMILY_REVIEW) return [fields.model || 'no model', fields.endpoint || ''].filter(Boolean);
  const runtime = runtimeNames[fields.runtime] || fields.runtime || 'no runtime';
  const model = fields.model ? (modelLabels[fields.runtime + '\u0000' + fields.model] || fields.model) : 'default model';
  return [runtime, model, effortWords(fields.thinking_effort)].filter(Boolean);
}

function dateWords(value) {
  const at = Date.parse(String(value || ''));
  return Number.isFinite(at) ? new Date(at).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' }) : '';
}

// migratedWords marks a route the daemon made from a place's earlier typed
// settings (Q11), or ''.
export function migratedWords(route = {}) {
  if (!route.migrated_at) return '';
  const on = dateWords(route.migrated_at);
  return 'From earlier settings' + (on ? ', ' + on : '');
}

function placeWords(place) {
  const where = place.lane === 'review' ? 'Every repository' : String(place.repository || place.project_root || 'a place');
  const notes = [place.state === 'enabled' ? '' : 'off', place.fallback ? 'fallback' : ''].filter(Boolean);
  return where + (notes.length ? ' (' + notes.join(', ') + ')' : '');
}

// usedBy groups the places that use a route by agent: one row per agent with
// its places. `agentNames` maps a profile id to the agent's name.
export function usedBy(route = {}, agentNames = {}) {
  const groups = new Map();
  for (const place of route.places || []) {
    const id = String(place.profile_id || '');
    const group = groups.get(id) || { agent: String(agentNames[id] || id || 'An agent'), places: [] };
    group.places.push(placeWords(place));
    groups.set(id, group);
  }
  return [...groups.values()].sort((a, b) => a.agent.localeCompare(b.agent));
}

// routeRow is one route on the Model routes list.
export function routeRow(route = {}, { runtimeNames = {}, modelLabels = {}, agentNames = {} } = {}) {
  const used = usedBy(route, agentNames);
  return {
    id: String(route.route_id || ''), name: String(route.name || ''), family: String(route.family || ''),
    familyWords: familyWords(route.family), runsOn: routeRunsOn(route, runtimeNames, modelLabels),
    locality: String(route.locality || (route.local ? 'on this machine' : 'leaves this machine')),
    migrated: migratedWords(route), usedBy: used, placeCount: (route.places || []).length, route,
    search: [route.name, route.fields?.model, route.fields?.endpoint, ...used.map(group => group.agent)].join(' ').toLowerCase(),
  };
}

export function routeRows(listed = {}, names = {}) {
  return (listed.routes || []).map(route => routeRow(route, names)).sort((a, b) => a.name.localeCompare(b.name));
}

// routesInUse are the routes at least one place uses, most used first: which
// agents, and where.
export function routesInUse(rows = []) {
  return rows.filter(row => row.placeCount > 0).map(row => ({
    name: row.name, runsOn: row.runsOn, agents: row.usedBy.map(group => group.agent),
    places: row.placeCount === 1 ? row.usedBy[0].places[0] : row.placeCount + ' places', placeCount: row.placeCount,
  })).sort((a, b) => b.placeCount - a.placeCount || a.name.localeCompare(b.name));
}

// unroutedPlaces are places still waiting for a route (the migration could not
// give them one) and places whose route is missing, from the roster.
export function unroutedPlaces(roster = {}) {
  const out = [];
  for (const agent of roster.agents || []) {
    for (const place of agent.places || []) {
      if (place.route_problem !== 'route_missing' && place.route_problem !== 'migration_failed') continue;
      const where = place.lane === 'review' ? 'Every repository' : String(place.repository || 'a place');
      out.push({ agentID: String(agent.profile_id || ''), agent: String(agent.name || agent.profile_id || 'An agent'), where,
        words: place.route_problem === 'route_missing'
          ? 'Its model route is missing. ' + (place.lane === 'review' ? 'Asks go to you.' : 'It starts no run.')
          : 'It has no route yet. It runs as it did before.' });
    }
  }
  return out;
}

// routeDraft is the request a create, an edit or a rename previews and selects.
export function routeDraft({ routeID = '', name = '', family = FAMILY_MANAGED, managed = {}, endpoint = '', model = '' } = {}) {
  const fields = family === FAMILY_REVIEW
    ? { endpoint: String(endpoint).trim(), model: String(model).trim() }
    : { runtime: String(managed.runtime || ''), model: String(managed.model || ''),
      ...(managed.thinking_effort ? { thinking_effort: managed.thinking_effort } : {}) };
  return { ...(routeID ? { route_id: routeID } : {}), name: String(name).trim(), family, fields };
}

// renameDraft keeps everything a route is and changes its name.
export function renameDraft(route = {}, name = '') {
  return { route_id: String(route.route_id || ''), name: String(name).trim(), family: String(route.family || ''), fields: route.fields || {} };
}

// previewView is what a sheet says before it writes: what the route would be,
// what refuses it, and the rules that apply.
export function previewView(preview = {}, names = {}) {
  const problems = (preview.problems || []).map(problem => ({ message: String(problem.message || ''), places: (problem.places || []).map(String) }));
  const rules = [];
  for (const admission of preview.admission || []) {
    for (const rule of admission.fired || []) {
      const where = admission.label ? ' — ' + admission.label : '';
      if (!rules.some(item => item.rule === rule && item.where === where)) {
        rules.push({ rule: String(rule), where, refuses: !admission.allowed && admission.rule === rule,
          action: admission.rule === rule ? String(admission.action || '') : 'observe' });
      }
    }
  }
  const refused = (preview.admission || []).filter(admission => !admission.allowed);
  return {
    runsOn: routeRunsOn(preview.route, names.runtimeNames, names.modelLabels),
    locality: String(preview.route?.locality || ''), follows: usedBy(preview.route, names.agentNames),
    followCount: (preview.route?.places || []).length, changed: Boolean(preview.changed), exists: Boolean(preview.exists),
    problems, rules, refusedByRule: refused.length > 0,
    canSave: problems.length === 0,
  };
}

// saveLabel names how many places follow an edit.
export function saveLabel(view) {
  if (!view.exists) return 'Create';
  if (!view.followCount || !view.canSave) return 'Save';
  return 'Save for ' + view.followCount + (view.followCount === 1 ? ' place' : ' places');
}

// refusalView turns a typed refusal into what a sheet shows: the message, how
// to recover, and the places it names.
export function refusalView(error) {
  return { message: [String(error?.message || error || 'Not saved'), String(error?.recovery || '')].filter(Boolean).join(' '),
    places: (error?.places || []).map(String), code: String(error?.code || '') };
}
