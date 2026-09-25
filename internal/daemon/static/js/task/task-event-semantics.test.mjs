import { hasRenderableText, isVendorTurnEvidence } from './task-event-semantics.js';

let failures = 0;
const check = (name, condition) => {
  if (!condition) { failures++; console.error('FAIL', name); }
};

for (const type of ['session', 'delta', 'text', 'thinking', 'thinking_delta', 'tool', 'tool_result', 'result', 'stderr']) {
  check(`${type} is vendor turn evidence`, isVendorTurnEvidence({ type }));
}
for (const type of ['queued', 'spawn', 'error', 'input_error', 'auth_required', 'done', 'task']) {
  check(`${type} is not vendor turn evidence`, !isVendorTurnEvidence({ type }));
}
check('missing events are not evidence', !isVendorTurnEvidence(null));
check('visible text is renderable', hasRenderableText('  evidence  '));
for (const text of ['', ' \n\t ', null, undefined, 42]) {
  check(`${String(text)} is not renderable text`, !hasRenderableText(text));
}

if (failures) process.exit(1);
console.log('task-event-semantics: all pass');
