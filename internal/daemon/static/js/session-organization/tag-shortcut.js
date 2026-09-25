// The tag shortcut (console.json keymap.tag_session; empty switches it off).
// Its default is an unmodified key, so it lives here with its own typing guard
// rather than in the global keymap listener, which has none: a letter typed
// into the composer or any other field must stay a letter.
import { api } from '../core.js';
import { openHeaderTagPopover } from './header-tags.js';
import { isTypingTarget, shortcutMatches } from './shortcut-match.js';

let binding = '';

api('/api/console/config').then(found => { binding = String(found?.config?.keymap?.tag_session || ''); }).catch(() => {});

document.addEventListener('keydown', event => {
  if (isTypingTarget(document.activeElement) || !shortcutMatches(binding, event)) return;
  const row = document.activeElement?.closest?.('#sidebody .sess');
  if (row) {
    event.preventDefault();
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true }));
  } else if (document.querySelector('.session-tags .tag-add')) {
    event.preventDefault();
    openHeaderTagPopover();
  }
});
