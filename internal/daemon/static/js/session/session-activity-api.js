import { cpHeaders } from '../core.js';

const ROOT = '/api/v1/session-activity';

async function checkedFetch(operation, path, options = {}) {
  const response = await fetch(ROOT + path, options);
  if (response.ok) return response;
  const detail = (await response.text()).trim();
  throw new Error(operation + ' failed: ' + (detail || ('HTTP ' + response.status)));
}

export async function fetchSessionActivitySnapshot(signal) {
  const response = await checkedFetch('Session activity snapshot', '', {
    headers: cpHeaders(), signal,
  });
  return response.json();
}

export function openSessionActivityStream(afterGeneration, signal) {
  return checkedFetch('Session activity stream', '/stream?after=' + encodeURIComponent(afterGeneration), {
    headers: cpHeaders(), signal,
  });
}
