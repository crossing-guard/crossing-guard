// Pure helpers of the tag shortcut, apart from the listener so they can be
// tested without a document.
export function isTypingTarget(node) {
  if (!node) return false;
  return /^(INPUT|TEXTAREA|SELECT)$/.test(node.tagName || '') || Boolean(node.isContentEditable);
}

// shortcutMatches reads "t" or "Meta+Shift+T": modifiers must match exactly.
export function shortcutMatches(spec, event) {
  if (!spec) return false;
  const parts = spec.split('+');
  const key = parts.pop();
  const wants = name => parts.includes(name);
  return event.key.toLowerCase() === key.toLowerCase()
    && event.metaKey === wants('Meta') && event.ctrlKey === wants('Ctrl')
    && event.altKey === wants('Alt') && event.shiftKey === wants('Shift');
}
