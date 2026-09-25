// Shared mechanical pager for fact panels. Callers retain API and state ownership.
import { el } from '../core.js';

export function addPager(box, label, page, state, key, load, reset={}) {
  if (!page) return;
  const row = el('div', 'sub');
  const move = offset => {
    state[key] = offset;
    for (const [resetKey, value] of Object.entries(reset)) state[resetKey] = value;
    void load();
  };
  if (page.offset > 0) {
    const previous = el('button', '', `Previous ${label}`);
    previous.addEventListener('click', () => move(Math.max(0, page.offset - page.limit)));
    row.appendChild(previous);
  }
  if (page.next_offset !== undefined) {
    const next = el('button', '', `Next ${label}`);
    next.addEventListener('click', () => move(page.next_offset));
    row.appendChild(next);
  }
  if (row.childNodes.length) box.appendChild(row);
}
