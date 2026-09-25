// Fetch-streamed SSE framing shared by compatibility chat and the global task feed.
// Transport only: no retries, task reduction, DOM, or runtime-specific behavior.
export async function readSSE(body, onEvent, onMalformed = () => {}) {
  if (!body?.getReader) throw new Error('Streaming response is unavailable.');
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  const dispatch = async frame => {
    const lines = frame.split(/\r?\n/);
    const data = [];
    let id = '', event = 'message', retry = null;
    for (const line of lines) {
      if (!line || line.startsWith(':')) continue;
      const colon = line.indexOf(':');
      const field = colon < 0 ? line : line.slice(0, colon);
      let value = colon < 0 ? '' : line.slice(colon + 1);
      if (value.startsWith(' ')) value = value.slice(1);
      if (field === 'data') data.push(value);
      else if (field === 'id' && !value.includes('\u0000')) id = value;
      else if (field === 'event') event = value || 'message';
      else if (field === 'retry' && /^\d+$/.test(value)) retry = Number(value);
    }
    if (!data.length) return;
    const raw = data.join('\n');
    let value;
    try {
      value = JSON.parse(raw);
    } catch (error) {
      onMalformed({ raw, id, event, error });
      return;
    }
    await onEvent(value, { id, event, retry });
  };

  try {
    for (;;) {
      const { done, value } = await reader.read();
      buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
      for (;;) {
        const match = /\r?\n\r?\n/.exec(buffer);
        if (!match) break;
        const frame = buffer.slice(0, match.index);
        buffer = buffer.slice(match.index + match[0].length);
        await dispatch(frame);
      }
      if (done) break;
    }
    if (buffer.trim()) await dispatch(buffer);
  } finally {
    try { await reader.cancel(); } catch { /* response abort already released it */ }
  }
}

export async function readDataEvents(body, onEvent, onMalformed) {
  return readSSE(body, onEvent, onMalformed);
}
