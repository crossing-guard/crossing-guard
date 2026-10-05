// Settings › Overview (settings-restructure plan §3.2): what the daemon says
// needs the owner, worst first, each item one click from the page that fixes
// it. The page weighs nothing itself; settingsAttention words what the reads say.
import { el, api } from "../core.js";
import { loadChatCapabilities } from "../chat-capabilities.js";
import { loadRoster } from "../orchestration/agents/roster-api.js";
import { loadTeamStatus, loadTeamLayers } from "../team-link.js";
import { loadSpeechCapabilities } from "../speech/speech-api.js";
import { agentState } from "../orchestration/agents/roster-model.js";
import { settingsAttention, attentionCounts } from "./settings-model.js";

// loadAttention reads what the daemon knows and lists what needs the owner. A
// read that fails is left out and named, so one broken source cannot blank the rest.
export async function loadAttention() {
  const sources = [['team', loadTeamStatus], ['teamLayers', loadTeamLayers], ['roster', loadRoster], ['runtimeStatus', () => api('/api/runtime-status')],
    ['speech', loadSpeechCapabilities], ['capabilities', loadChatCapabilities]];
  const settled = await Promise.allSettled(sources.map(([, load]) => load()));
  const reads = {}, unread = [];
  settled.forEach((result, index) => {
    if (result.status === 'fulfilled') reads[sources[index][0]] = result.value;
    else unread.push(sources[index][0]);
  });
  const runtimeNames = Object.fromEntries((reads.capabilities || []).map(item => [item.runtime, item.displayName]));
  const items = settingsAttention(reads, runtimeNames);
  return { reads, unread, items, counts: attentionCounts(items) };
}

const SEVERITY_MARK = Object.freeze({ bad: '!', warn: '!', info: 'i' });
const SOURCE_WORDS = Object.freeze({ team: 'the team link', teamLayers: 'what the team shares', roster: 'agents', runtimeStatus: 'runtimes', speech: 'dictation' });

function itemRow(item, ctx) {
  const row = el('div', 'settings-attention settings-attention-' + item.severity);
  row.appendChild(el('span', 'settings-attention-mark', SEVERITY_MARK[item.severity]));
  const text = el('div', 'settings-fact-text');
  text.appendChild(el('strong', '', item.title));
  for (const fact of item.facts) text.appendChild(el('div', 'sub', fact));
  const go = el('button', 'btn', 'Open');
  go.type = 'button';
  go.onclick = () => ctx.go(item.page, item.target);
  row.append(text, go);
  return row;
}

// areaTiles is one fact per area, each taken from a read.
function areaTiles(reads) {
  const tiles = [];
  const runtimes = reads.runtimeStatus?.runtimes;
  if (runtimes) {
    const connected = runtimes.filter(item => item.hook_configured && item.hook_binary_present).length;
    tiles.push(['runtimes', 'Runtimes', connected + ' of ' + runtimes.length + ' connected']);
  }
  if (reads.team) tiles.push(['team', 'Team', reads.team.pending ? 'pending' : String(reads.team.state || '')]);
  if (reads.roster) {
    const agents = reads.roster.agents || [];
    tiles.push(['agents', 'Agents', agents.filter(agent => ['on', 'attn'].includes(agentState(agent))
      && (agent.places || []).some(place => place.state === 'enabled')).length + ' of ' + agents.length + ' on']);
  }
  if (reads.speech) tiles.push(['dictation', 'Dictation', String(reads.speech.state || '')]);
  return tiles;
}

export function renderOverviewPage(main, ctx) {
  const { items, reads, unread } = ctx.attention;
  const counted = items.filter(item => item.severity !== 'info');
  const noted = items.filter(item => item.severity === 'info');
  const list = el('section', 'settings-card settings-attention-list');
  // A read that failed says nothing either way, so "nothing needs you" is only
  // said when every source answered.
  const missing = unread.filter(name => name !== 'capabilities').map(name => SOURCE_WORDS[name] || name);
  if (counted.length) counted.forEach(item => list.appendChild(itemRow(item, ctx)));
  else if (!missing.length) list.appendChild(el('div', 'sub', 'Nothing needs you.'));
  if (counted.length || !missing.length) main.appendChild(list);
  if (missing.length) main.appendChild(el('div', 'banner', 'Could not be read: ' + missing.join(', ') + '.'));
  if (noted.length) {
    main.appendChild(el('h3', 'settings-section', 'To check'));
    const notes = el('section', 'settings-card settings-attention-list');
    noted.forEach(item => notes.appendChild(itemRow(item, ctx)));
    main.appendChild(notes);
  }
  const tiles = el('div', 'settings-tiles');
  for (const [page, label, value] of areaTiles(reads)) {
    const tile = el('button', 'settings-tile');
    tile.type = 'button';
    tile.append(el('span', 'sub', label), el('strong', '', value));
    tile.onclick = () => ctx.go(page, '');
    tiles.appendChild(tile);
  }
  main.appendChild(tiles);
}
