import { el } from '../core.js';

const ICON_SEND = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5M5 12l7-7 7 7"/></svg>';
const ICON_STOP = '<svg viewBox="0 0 24 24"><rect x="7" y="7" width="10" height="10" rx="1.5" fill="currentColor"/></svg>';

export function atMentionToken(value, selectionStart) {
  const position = Number.isSafeInteger(selectionStart) ? selectionStart : String(value || '').length;
  const before = String(value || '').slice(0, position);
  const match = before.match(/(^|\s)@([\w./-]*)$/);
  return match ? { query: match[2], start: position - match[2].length - 1, end: position } : null;
}

export function matchingCommands(commands, value) {
  const source = String(value || '');
  if (!source.startsWith('/')) return [];
  const word = source.slice(1).split(/\s+/)[0].toLowerCase();
  return commands.filter(command => command.cmd.slice(1).startsWith(word));
}

export function insertMention(value, token, insertion) {
  if (!token) return String(value || '');
  return String(value || '').slice(0, token.start) + insertion + ' ' + String(value || '').slice(token.end);
}

export function createComposer({ sendIntent, loadFiles, fileContext } = {}) {
  const root = el('div');
  root.id = 'composer';
  const textarea = el('textarea');
  textarea.placeholder = 'Prompt… (Enter to send, Shift+Enter for newline)';
  textarea.rows = 1;
  const controls = el('div', 'below');
  const send = el('button', '', '');
  send.id = 'sendbtn';
  send.title = 'Send (Enter)';
  send.setAttribute('aria-label', 'Send');
  send.innerHTML = ICON_SEND;
  controls.appendChild(send);
  const menu = el('div');
  menu.id = 'composermenu';
  root.append(textarea, controls, menu);

  let commands = [];
  let items = [];
  let selected = 0;
  let mode = null;
  let fileTimer = null;

  const resize = () => {
    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, 200) + 'px';
  };
  const closeMenu = () => {
    menu.style.display = 'none';
    mode = null;
    items = [];
  };
  const paintMenu = () => {
    menu.replaceChildren();
    if (!items.length) { closeMenu(); return; }
    selected = Math.min(selected, items.length - 1);
    items.forEach((item, index) => {
      const row = el('div', 'mi' + (index === selected ? ' sel' : ''));
      row.append(el('span', 'cmd', item.cmd), el('span', 'hint', item.hint || ''));
      row.onclick = () => { selected = index; pickMenu(); };
      menu.appendChild(row);
    });
    menu.style.display = 'block';
  };
  const updateMenu = () => {
    const value = textarea.value;
    if (value.startsWith('/')) {
      mode = 'cmd';
      items = matchingCommands(commands, value);
      selected = 0;
      paintMenu();
      return;
    }
    const token = atMentionToken(value, textarea.selectionStart);
    if (token && typeof loadFiles === 'function') {
      mode = 'file';
      clearTimeout(fileTimer);
      fileTimer = setTimeout(async () => {
        try {
          const files = await loadFiles(token.query, fileContext?.());
          if (mode !== 'file') return;
          items = (files || []).map(file => ({ cmd: file, insert: file }));
          selected = 0;
          paintMenu();
        } catch { closeMenu(); }
      }, 150);
      return;
    }
    closeMenu();
  };
  const pickMenu = () => {
    const item = items[selected];
    if (!item) return;
    if (mode === 'file') {
      const token = atMentionToken(textarea.value, textarea.selectionStart);
      if (!token) { closeMenu(); return; }
      textarea.value = insertMention(textarea.value, token, item.insert);
      textarea.selectionStart = textarea.selectionEnd = token.start + item.insert.length + 1;
      closeMenu();
      resize();
      textarea.focus();
      return;
    }
    const rest = textarea.value.replace(/^\/\S*\s*/, '').trim();
    textarea.value = '';
    resize();
    closeMenu();
    item.run(rest);
  };

  textarea.addEventListener('input', () => { resize(); updateMenu(); });
  textarea.addEventListener('keydown', event => {
    if (menu.style.display === 'block') {
      if (event.key === 'ArrowDown') { event.preventDefault(); selected = Math.min(selected + 1, items.length - 1); paintMenu(); return; }
      if (event.key === 'ArrowUp') { event.preventDefault(); selected = Math.max(selected - 1, 0); paintMenu(); return; }
      if ((event.key === 'Enter' && !event.shiftKey) || event.key === 'Tab') { event.preventDefault(); pickMenu(); return; }
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); closeMenu(); return; }
    }
    if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); sendIntent?.(); }
  });
  send.addEventListener('click', () => sendIntent?.());

  return {
    root,
    textarea,
    controls,
    send,
    setCommands(next) { commands = Array.isArray(next) ? [...next] : []; updateMenu(); },
    draft() { return textarea.value.trim(); },
    clear() { textarea.value = ''; resize(); closeMenu(); },
    restore(value) { textarea.value = String(value || ''); resize(); },
    insertText(value) {
      const start = textarea.selectionStart ?? textarea.value.length;
      const end = textarea.selectionEnd ?? start;
      textarea.setRangeText(String(value || ''), start, end, 'end');
      resize();
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    },
    // replaceRange is the one write path a collaborator (dictation) may use:
    // one setRangeText, so one native undo step, and one input event so every
    // listener sees the same edit the user would have made.
    replaceRange(start, end, value, { moveCaret = true } = {}) {
      const length = textarea.value.length;
      const from = Math.max(0, Math.min(start, length));
      const to = Math.max(from, Math.min(end, length));
      const keepStart = textarea.selectionStart, keepEnd = textarea.selectionEnd;
      const text = String(value || '');
      textarea.setRangeText(text, from, to, 'end');
      if (!moveCaret) {
        // A preserved caret after the edited range moves with the edit.
        const delta = text.length - (to - from);
        const shift = position => (position >= to ? position + delta : (position > from ? from : position));
        textarea.setSelectionRange(shift(keepStart), shift(keepEnd));
      }
      resize();
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    },
    caret() { return textarea.selectionStart ?? textarea.value.length; },
    value() { return textarea.value; },
    // metrics lets an overlay mirror the textarea without touching it.
    metrics() {
      const style = typeof getComputedStyle === 'function' ? getComputedStyle(textarea) : {};
      return { font: style.font || '', lineHeight: style.lineHeight || '', letterSpacing: style.letterSpacing || '',
        paddingLeft: style.paddingLeft || '0px', paddingTop: style.paddingTop || '0px',
        offsetTop: textarea.offsetTop, offsetLeft: textarea.offsetLeft, width: textarea.clientWidth,
        height: textarea.clientHeight, scrollTop: textarea.scrollTop };
    },
    focus() { textarea.focus(); },
    setRunning(running) {
      send.innerHTML = running ? ICON_STOP : ICON_SEND;
      send.title = running ? 'Stop' : 'Send (Enter)';
      send.setAttribute('aria-label', running ? 'Stop' : 'Send');
      send.classList.toggle('stop', running);
    },
    dispose() { clearTimeout(fileTimer); closeMenu(); },
  };
}
