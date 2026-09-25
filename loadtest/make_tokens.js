// Prepares access tokens for hold_contention.js so that logging in is not
// part of the measured load. setup() calls the real dev-login endpoint in
// parallel batches; handleSummary writes the tokens to TOKENS_OUT.
//
// Why not setup() of hold_contention.js itself: k6 copies setup()'s return
// value into every VU, so 50,000 tokens x 2,000 VUs would need tens of GB.
// hold_contention.js loads this file once into a SharedArray instead.
//
// Env: BASE_URL (default http://localhost:8080), COUNT (default 50000),
//      TOKENS_OUT (required, output JSON file).
import http from 'k6/http';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const COUNT = Number(__ENV.COUNT || 50000);
const BATCH = 500;

if (!__ENV.TOKENS_OUT) {
  throw new Error('TOKENS_OUT is required');
}

export const options = {
  vus: 1,
  iterations: 1,
  setupTimeout: '300s',
  summaryTrendStats: ['avg', 'p(99)'],
};

export function setup() {
  // A per-run offset keeps user ids unique across runs without a db reset.
  const base = (Date.now() % 1_000_000) * 1_000_000;
  const tokens = [];
  for (let start = 0; start < COUNT; start += BATCH) {
    const reqs = [];
    for (let i = start; i < Math.min(start + BATCH, COUNT); i++) {
      reqs.push({
        method: 'POST',
        url: `${BASE_URL}/v1/auth/dev-login`,
        body: JSON.stringify({ user_id: base + i }),
        params: { headers: { 'Content-Type': 'application/json' }, tags: { name: 'login' } },
      });
    }
    for (const res of http.batch(reqs)) {
      if (res.status !== 200) {
        throw new Error(`dev-login failed: ${res.status} ${res.body}`);
      }
      tokens.push(res.json('access_token'));
    }
  }
  return { tokens };
}

export default function () {}

export function handleSummary(data) {
  const tokens = data.setup_data.tokens;
  console.log(`wrote ${tokens.length} tokens to ${__ENV.TOKENS_OUT}`);
  return { [__ENV.TOKENS_OUT]: JSON.stringify(tokens) };
}
