import { orchestrationApi } from './orchestration-api-error.js';

// Review-lane calls share the one orchestration error unwrap with a review
// fallback code (orchestration-api-error.js owns the envelope parsing).
function reviewApi(path, options) {
  return orchestrationApi(path, options, { code: 'review_error' });
}

function loadReviewSettings() {
  return reviewApi('/api/orchestration/reviews/settings');
}

function saveReviewBinding(input) {
  return reviewApi('/api/orchestration/reviews/binding', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...input, confirmed: true }),
  });
}

function disableReviewBinding(expectedStateToken) {
  return reviewApi('/api/orchestration/reviews/binding/disable', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ expected_state_token: expectedStateToken, confirmed: true }),
  });
}

function loadSessionReviews(runtime, sessionID, limit = 25) {
  const query = new URLSearchParams({ runtime, id: sessionID, limit: String(limit) });
  return reviewApi('/api/orchestration/reviews/session?' + query);
}

export { loadReviewSettings, saveReviewBinding, disableReviewBinding, loadSessionReviews };
