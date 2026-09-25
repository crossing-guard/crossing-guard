// Shared orchestration API error unwrap (IMPL-10 #13/14).
//
// Every orchestration route reports failures as an HTTP error whose body is
// `{"error":{"message","code","field","recovery"}}`. core.js api() surfaces the
// raw body as error.message; this module is the ONE place that JSON envelope is
// unwrapped into a normalized Error carrying { status, code, field, recovery }.
// profile-api.js, review-api.js, managed-api.js and agents-api.js all call
// through here so the unwrap cannot drift between clients.
import { api } from '../core.js';

// normalizeOrchestrationError turns a raw api() error into a normalized one
// when its message body is the typed error envelope; otherwise it returns the
// original error unchanged. Never throws.
export function normalizeOrchestrationError(error, { code = 'orchestration_error', recovery = '' } = {}) {
  let payload;
  try { payload = JSON.parse(String(error?.message || '')); } catch { return error; }
  if (!payload?.error || typeof payload.error.message !== 'string') return error;
  const normalized = new Error(payload.error.message);
  normalized.status = error.status;
  normalized.code = String(payload.error.code || code);
  normalized.field = String(payload.error.field || '');
  normalized.recovery = String(payload.error.recovery || recovery);
  return normalized;
}

// orchestrationApi is api() with the typed-error unwrap applied. `defaults`
// supplies the fallback code (and, for the profile lane, a fallback recovery
// line) so each client keeps its historical error shape.
export async function orchestrationApi(path, options, defaults) {
  try {
    return await api(path, options);
  } catch (error) {
    throw normalizeOrchestrationError(error, defaults);
  }
}
