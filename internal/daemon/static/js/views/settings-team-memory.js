// Settings → Team: shared memory (team item 5; team rest-of-release plan §4.2). The
// counts are rows of From the team and From this device (settings-team-model.js).
// Sharing the records that existed before the link is one explicit act, confirmed in a
// dialog that says what is sent; the deletions list is read only when Details is opened,
// because the daemon asks the team server for it.
import { keyValue } from '../orchestration/agents/agent-ui.js';
import { actDialog } from './settings-team-dialog.js';
import { shareTeamMemory, loadTeamMemoryDeletions } from '../team-link.js';
import { count } from './team-memory-text.js';

// shareWords says what "Share N more…" sends, one fact per row.
export function shareWords(n, organization) {
  return [
    ['Sent', count(n, 'memory record', 'memory records') + ' to ' + organization],
    ['Which', 'Active records at organization scope, or at a repository identified by its remote, written before this device shared them'],
    ['Checked first', 'Each is checked against the secret patterns, and matches are redacted'],
    ['Who receives them', 'Every member’s devices'],
  ];
}

// shareMemoryDialog confirms and sends the records this device has not shared yet.
export function shareMemoryDialog(view) {
  const n = Number(view.status.memory?.share_candidates || 0);
  const organization = view.status.organization?.name || 'the team';
  actDialog(view, { title: 'Share ' + count(n, 'record', 'records') + ' with ' + organization, body: keyValue(shareWords(n, organization)),
    label: 'Share', failed: 'Not shared', act: () => shareTeamMemory() });
}

// memoryDeletions asks how far this device's memory deletions have reached; null when
// the team server cannot be asked.
export async function memoryDeletions() {
  try { return await loadTeamMemoryDeletions(); } catch { return null; }
}
