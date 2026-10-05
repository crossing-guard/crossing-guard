// Task lifecycle events describe the daemon's work, not a model turn. Keep the
// handoff counter tied to evidence that the vendor process actually emitted.
const VENDOR_TURN_EVENT_TYPES = new Set([
  'session',
  'delta',
  'text',
  'thinking',
  'thinking_delta',
  'tool',
  'tool_result',
  'result',
  'usage',
  'stderr',
]);

export function isVendorTurnEvidence(event) {
  return VENDOR_TURN_EVENT_TYPES.has(event?.type);
}

export function hasRenderableText(text) {
  return typeof text === 'string' && text.trim().length > 0;
}
