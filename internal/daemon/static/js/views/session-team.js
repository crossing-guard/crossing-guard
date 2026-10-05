// The session's "sends:" line (team plan §5.1, R18): on a linked device, what this
// session sends the team server — in the evidence footer, never the transcript — and the
// per-session content consent (D-12). Facts from GET /api/team only.
import { el, fmtTime } from '../core.js';
import { provider } from '../infopanel.js';
import { loadTeamStatus, setSessionContent, loadSentContent, deleteSentContent } from '../team-link.js';
import { sentLine } from './team-memory-text.js';

// sessionContentKey mirrors store.ContentSessionKey: "<runtime>/<native id>".
export function sessionContentKey(runtime, id) {
  const slash = id.indexOf('/');
  const native = slash > 0 ? id.slice(slash + 1) : id;
  const rt = runtime || (slash > 0 ? id.slice(0, slash) : '') || 'unknown';
  return rt + '/' + (native || 'unknown');
}

// contentLine states the session's content fact in words, from the status document.
export function contentLine(status, key) {
  const content = status.content || {};
  const mine = (content.opted_in || []).find((row) => row.session === key);
  if (mine) return { on: true, text: 'content: on since ' + fmtTime(mine.since) + ' (you opted this session in; captured tool inputs, redacted)' };
  if (content.mandate) {
    const repos = content.mandate.repositories || [];
    return { on: true, mandated: true, text: 'content: mandated by adopted organization bundle ' + content.mandate.bundle_id +
      (repos.length ? ' for repositories ' + repos.join(', ') + ' (sessions outside them are not sent)' : '') };
  }
  return { on: false, text: 'content: off — this session\'s bodies stay on this device' };
}

// deleteSentControl renders "Delete what was sent" whenever the server holds content for
// the session from this device — opted in or mandated. Deleting also turns sharing off
// for the session.
async function deleteSentControl(line, selection, server, redraw) {
  const runtime = selection.runtime || '';
  const session = selection.thread_id || selection.id;
  let sent;
  try { sent = await loadSentContent(runtime, session); } catch (_) { return; }
  if (!line.isConnected || !sent.chunks) return;
  line.appendChild(el('span', 'evidence-fresh', sentLine(sent.chunks)));
  const button = el('button', '', 'Delete what was sent');
  button.onclick = async () => {
    if (!confirm('Delete this session\'s content from ' + server + '? ' + sentLine(sent.chunks) + ' will be erased there, and sharing for this session is turned off. Server backups may keep a copy until their own retention ends.')) return;
    try { await deleteSentContent(runtime, session); redraw(await loadTeamStatus()); }
    catch (error) { line.appendChild(el('span', 'problem', 'Not deleted: ' + (error.message || error))); }
  };
  line.appendChild(button);
}

provider({ id: 'session.team.sends', order: 5, zone: 'pinned', title: 'Team sends', required: false,
  match: (ctx) => ctx.surface === 'session' && !!ctx.selection,
  render: async (ctx, box) => {
    let status;
    try { status = await loadTeamStatus(); } catch (_) { return; }
    if (!box.isConnected || status.state !== 'linked') return;
    const key = sessionContentKey(ctx.selection.runtime || '', ctx.selection.thread_id || ctx.selection.id || '');
    const line = el('div', 'evidence-footer');
    const draw = (s) => {
      line.textContent = '';
      const fact = contentLine(s, key);
      line.appendChild(el('span', 'evidence-fresh', 'team ' + (s.organization?.name || s.server) + ' · sends metadata · decisions · ' + fact.text));
      deleteSentControl(line, ctx.selection, s.server, draw);
      if (fact.mandated) return;
      const toggle = el('button', '', fact.on ? 'Stop sharing content' : 'Share content');
      toggle.onclick = async () => {
        if (!fact.on && !confirm('Send this session\'s captured tool inputs to ' + s.server + '? They are checked against the secret patterns and matches redacted first. Forward only: nothing already captured is sent. Turning it off later stops what has not been sent; what the server holds stays until deletion.')) return;
        try { await setSessionContent(ctx.selection.runtime || '', ctx.selection.thread_id || ctx.selection.id, !fact.on); draw(await loadTeamStatus()); }
        catch (error) { line.appendChild(el('span', 'problem', 'Not changed: ' + (error.message || error))); }
      };
      line.appendChild(toggle);
    };
    draw(status);
    box.appendChild(line);
  } });
