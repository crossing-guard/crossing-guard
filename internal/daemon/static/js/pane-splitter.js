// A split node of the pane host: two children and a divider. Pointer drag,
// keyboard resize, double-click reset; the ratio commits to the host at the
// end of an interaction and paints live during it.
import { paneHostDefaults, setSplitRatio } from './pane-host.js';
import { clampRatio } from './pane-layout.js';

// keyboardStep is the ratio change per arrow press; a mechanism, not policy.
const keyboardStep = 0.05;

function paint(element, node, ratio) {
  const defaults = paneHostDefaults();
  node.ratio = clampRatio(ratio, defaults);
  element.style.setProperty('--pane-split', `${(node.ratio * 100).toFixed(2)}%`);
  element.querySelector(':scope > .pane-splitter')?.setAttribute('aria-valuenow', String(Math.round(node.ratio * 100)));
}

function installDrag(element, node, divider) {
  divider.addEventListener('pointerdown', event => {
    event.preventDefault(); divider.classList.add('dragging'); divider.setPointerCapture?.(event.pointerId);
    const move = moveEvent => {
      const rect = element.getBoundingClientRect();
      const ratio = node.dir === 'row' ? (moveEvent.clientX - rect.left) / rect.width : (moveEvent.clientY - rect.top) / rect.height;
      if (Number.isFinite(ratio)) paint(element, node, ratio);
    };
    const up = () => {
      divider.classList.remove('dragging');
      divider.removeEventListener('pointermove', move); divider.removeEventListener('pointerup', up); divider.removeEventListener('pointercancel', up);
      setSplitRatio(node.id, node.ratio);
    };
    divider.addEventListener('pointermove', move); divider.addEventListener('pointerup', up); divider.addEventListener('pointercancel', up);
  });
}

function installKeys(element, node, divider) {
  divider.addEventListener('keydown', event => {
    const keys = node.dir === 'row' ? { ArrowLeft: -keyboardStep, ArrowRight: keyboardStep } : { ArrowUp: -keyboardStep, ArrowDown: keyboardStep };
    if (event.key === 'Home') { event.preventDefault(); paint(element, node, 0.5); setSplitRatio(node.id, 0.5); return; }
    if (!(event.key in keys)) return;
    event.preventDefault(); paint(element, node, node.ratio + keys[event.key]); setSplitRatio(node.id, node.ratio);
  });
  divider.addEventListener('dblclick', () => { paint(element, node, 0.5); setSplitRatio(node.id, 0.5); });
}

export function renderSplit(host, node, first, second) {
  const element = document.createElement('div');
  element.className = `pane-split pane-split-${node.dir}`; element.dataset.splitId = node.id;
  const a = document.createElement('div'); a.className = 'pane-split-child first'; a.appendChild(first);
  const b = document.createElement('div'); b.className = 'pane-split-child second'; b.appendChild(second);
  const divider = document.createElement('div');
  divider.className = 'pane-splitter'; divider.tabIndex = 0; divider.setAttribute('role', 'separator');
  divider.setAttribute('aria-orientation', node.dir === 'row' ? 'vertical' : 'horizontal');
  const defaults = paneHostDefaults();
  divider.setAttribute('aria-valuemin', String(Math.round(defaults.clamp[0] * 100)));
  divider.setAttribute('aria-valuemax', String(Math.round(defaults.clamp[1] * 100)));
  divider.title = 'Drag or use arrow keys to resize · double-click to reset';
  element.append(a, divider, b);
  paint(element, node, node.ratio);
  installDrag(element, node, divider);
  installKeys(element, node, divider);
  return element;
}
