// "Open this card's session in its desktop app" on a board. The link, its
// label, the off switch and the hand-off are session/native-open.js's; this
// module only decides where a board shows them. A card is a draggable button
// whose click opens the session, so nothing clickable may sit inside it
// (PO-17): the control lives on the board, over the card's corner, and the
// same action is an item in the card's menu.

import { nativeOpenControl, nativeOpenModel, openNative } from '../session/native-open.js';

// What each card's row said about its link. A card that stays through a
// refresh is repainted from the fresh row, so this follows the row.
const cardSessions = new WeakMap();

export function rememberCardLink(card, session) {
  cardSessions.set(card, { native_open: session?.native_open });
}

// cardOpenItem is the card menu's item, or null for a card without a link.
// Opening leaves the board as it was, so the menu hands focus back to the card.
export function cardOpenItem(card) {
  const session = cardSessions.get(card);
  const model = nativeOpenModel(session);
  return model ? { label: model.label, refocus: true, run: () => openNative(session) } : null;
}

const shownControl = board => board.querySelector('.board-card-open');

export function hideCardOpen(board) { shownControl(board)?.remove(); }

function placeOver(board, control, card) {
  const cardRect = card.getBoundingClientRect();
  const boardRect = board.getBoundingClientRect();
  control.style.top = (cardRect.top - boardRect.top + board.scrollTop + 4) + 'px';
  control.style.left = (cardRect.right - boardRect.left + board.scrollLeft - (control.offsetWidth || 0) - 4) + 'px';
}

// showCardOpen puts the one control over `card`'s top-right corner. It is a
// child of the board, never of the card; it is a small mark, so the title
// under it stays the card's; it is not a tab stop (the board keeps one, and
// the keyboard path is the card's menu) and not a drag source.
export function showCardOpen(board, card) {
  const shown = shownControl(board);
  if (shown?.forCard === card) return shown;
  shown?.remove();
  const session = cardSessions.get(card);
  const control = nativeOpenControl(session);
  if (!control) return null;
  control.classList.add('board-card-open');
  control.textContent = '\u2197';
  control.setAttribute('aria-label', nativeOpenModel(session).label);
  control.tabIndex = -1;
  control.draggable = false;
  control.forCard = card;
  board.appendChild(control);
  placeOver(board, control, card);
  return control;
}

// settleCardOpen keeps the control true after the board changed under it:
// gone when its card left or is no longer drawn, else back over its card.
// `isShown` is the board's own test of whether a card is drawn.
export function settleCardOpen(board, isShown = () => true) {
  const shown = board && shownControl(board);
  if (!shown) return;
  const card = shown.forCard;
  if (!card || card.isConnected === false || !card.closest('.board') || !isShown(card)) { shown.remove(); return; }
  placeOver(board, shown, card);
}

// mountCardOpen shows the control for the card under a mouse pointer and takes
// it away when the pointer leaves both, the board scrolls, or a drag starts.
// A touch has no hover, and a control appearing under the finger would take
// the tap meant for the card: touch uses the card's menu.
export function mountCardOpen(board) {
  const inside = (node, selector) => node?.closest?.(selector) || null;
  board.addEventListener('pointerover', event => {
    if (event.pointerType && event.pointerType !== 'mouse') return;
    const shown = shownControl(board);
    if (shown && !shown.forCard.isConnected) shown.remove();
    if (inside(event.target, '.board-card-open')) return;
    const card = inside(event.target, '.board-card');
    if (card) showCardOpen(board, card);
  });
  board.addEventListener('pointerout', event => {
    const shown = shownControl(board);
    if (!shown) return;
    const to = event.relatedTarget;
    if (inside(to, '.board-card-open') === shown || inside(to, '.board-card') === shown.forCard) return;
    shown.remove();
  });
  board.addEventListener('dragstart', () => hideCardOpen(board));
  board.addEventListener('scroll', () => hideCardOpen(board), true);
}
