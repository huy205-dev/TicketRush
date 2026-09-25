// Opening-sale scenario (SPEC.md 13.3): ramp from 0 to 2,000 buyers in 10s,
// keep them for 60s. 70% of buyers want VIP seats.
//
// BUYER_MODE picks how SPEC.md's "each VU: dev-login, view seats, ..." is read:
//   iteration (default, the reference for backend comparisons)
//                 every iteration is a new buyer who logs in, so the VU keeps
//                 placing orders until the event is sold out;
//   vu            each VU is one buyer: log in once, stop after one order.
//                 Arrivals are capped by the ramp, so it cannot saturate the
//                 server (see docs/adr/006).
//
// Each buyer:
//   1. takes an access token prepared before the test by make_tokens.js
//      (TOKENS_FILE), so logging in is not part of the measured load; VU v
//      owns tokens v-1, v-1+VUS, v-1+2*VUS... and moves to the next one
//      after each order it creates (a user may hold one order per event);
//   2. load the seat map and pick 1-4 random AVAILABLE seats, preferring the
//      buyer's zone and falling back to any zone when it is sold out;
//   3. POST /v1/orders; on 409 SEATS_UNAVAILABLE drop the taken seats from
//      the local copy and retry, at most 3 retries per iteration;
//   4. in vu mode, after a successful hold the buyer is done and idles,
//      because a user may hold only one order per event.
// When nothing is available anywhere the buyer sleeps 1s and looks again.
//
// Reference measurement (3 runs, fresh database each run, median and range):
//   make bench-hold BACKEND=redis
// Single run against an already running booking service:
//   make db-reset seed && make run-booking   (another terminal)
//   make load-hold
//
// Env: BASE_URL (default http://localhost:8080), EVENT_ID (default 1),
//      VUS (default 2000), RAMP (default 10s), HOLD (default 60s),
//      BUYER_MODE (iteration | vu, default iteration),
//      TOKENS_FILE (required, JSON array written by make_tokens.js).
//
// Latency is also recorded per outcome: hold_created_ms (201),
// hold_seats_unavailable_ms (409 SEATS_UNAVAILABLE) and hold_other_ms.
//      Change VUS/RAMP/HOLD only for smoke runs; results use the defaults.
import http from 'k6/http';
import { sleep } from 'k6';
import { SharedArray } from 'k6/data';
import { Counter, Trend } from 'k6/metrics';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const EVENT_ID = Number(__ENV.EVENT_ID || 1);
const VUS = Number(__ENV.VUS || 2000);
const RAMP = __ENV.RAMP || '10s';
const HOLD = __ENV.HOLD || '60s';
const VIP_SHARE = 0.7;
const MAX_RETRIES = 3;
const BUYER_MODE = __ENV.BUYER_MODE || 'iteration';
if (BUYER_MODE !== 'vu' && BUYER_MODE !== 'iteration') {
  throw new Error(`BUYER_MODE must be vu or iteration, got ${BUYER_MODE}`);
}
if (!__ENV.TOKENS_FILE) {
  throw new Error('TOKENS_FILE is required: run loadtest/make_tokens.js first (make load-hold does)');
}
// Loaded once and shared read-only by all VUs.
const tokens = new SharedArray('tokens', () => JSON.parse(open(__ENV.TOKENS_FILE)));
if (tokens.length < VUS) {
  throw new Error(`need at least ${VUS} tokens, ${__ENV.TOKENS_FILE} has ${tokens.length}`);
}

export const options = {
  scenarios: {
    opening_sale: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: RAMP, target: VUS },
        { duration: HOLD, target: VUS },
      ],
      gracefulRampDown: '5s',
    },
  },
  thresholds: {
    'http_req_duration{name:hold}': ['p(99)<200'],
    'http_req_failed{name:hold}': ['rate<0.01'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

// 409 is a normal business answer during a sale, not a failure.
http.setResponseCallback(http.expectedStatuses(200, 201, 409));

const holdCreated = new Counter('hold_created');
const holdSeatsTaken = new Counter('hold_seats_unavailable');
const holdOtherError = new Counter('hold_other_error');
const soldOutWaits = new Counter('sold_out_waits');
const tokensExhausted = new Counter('tokens_exhausted');
const createdMs = new Trend('hold_created_ms', true);
const seatsTakenMs = new Trend('hold_seats_unavailable_ms', true);
const otherMs = new Trend('hold_other_ms', true);

// Module scope is per VU: this is the buyer's state.
let tokenIndex = -1; // set on the first iteration
let done = false;

export function setup() {
  const res = http.get(`${BASE_URL}/v1/events/${EVENT_ID}`);
  if (res.status !== 200) {
    throw new Error(`event ${EVENT_ID} not found (${res.status}); run make db-reset seed`);
  }
}

function availableSeats() {
  const res = http.get(`${BASE_URL}/v1/events/${EVENT_ID}/seats`, { tags: { name: 'seatmap' } });
  if (res.status !== 200) {
    return null;
  }
  const byZone = {};
  for (const s of res.json('seats')) {
    if (s.status === 'AVAILABLE') {
      (byZone[s.zone] = byZone[s.zone] || []).push(s.seat_id);
    }
  }
  return byZone;
}

function pickSeats(byZone, preferVip) {
  const zones = Object.keys(byZone).filter((z) => byZone[z].length > 0);
  if (zones.length === 0) {
    return null;
  }
  let zone;
  if (preferVip && byZone.VIP && byZone.VIP.length > 0) {
    zone = 'VIP';
  } else {
    const others = zones.filter((z) => z !== 'VIP');
    const pool = preferVip || others.length === 0 ? zones : others;
    zone = pool[Math.floor(Math.random() * pool.length)];
  }
  const free = byZone[zone];
  const want = Math.min(1 + Math.floor(Math.random() * 4), free.length);
  const picked = new Set();
  while (picked.size < want) {
    picked.add(free[Math.floor(Math.random() * free.length)]);
  }
  return Array.from(picked);
}

function forget(byZone, seatIds) {
  const gone = new Set(seatIds);
  for (const z of Object.keys(byZone)) {
    byZone[z] = byZone[z].filter((id) => !gone.has(id));
  }
}

export default function () {
  if (done) {
    sleep(1);
    return;
  }
  if (tokenIndex < 0) {
    tokenIndex = exec.vu.idInTest - 1;
  }
  if (tokenIndex >= tokens.length) {
    tokensExhausted.add(1);
    done = true;
    return;
  }
  const token = tokens[tokenIndex];

  const byZone = availableSeats();
  if (byZone === null) {
    sleep(1);
    return;
  }
  const preferVip = (exec.vu.idInTest % 10) < VIP_SHARE * 10;

  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    const seats = pickSeats(byZone, preferVip);
    if (seats === null) {
      soldOutWaits.add(1);
      sleep(1);
      return;
    }
    const res = http.post(
      `${BASE_URL}/v1/orders`,
      JSON.stringify({ event_id: EVENT_ID, seat_ids: seats }),
      {
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
          'Idempotency-Key': crypto.randomUUID(),
        },
        tags: { name: 'hold' },
      },
    );
    if (res.status === 201) {
      createdMs.add(res.timings.duration);
      holdCreated.add(1);
      // This user now holds an order; the next buyer uses a fresh token.
      tokenIndex += VUS;
      done = BUYER_MODE === 'vu';
      return;
    }
    if (res.status === 409 && res.json('error.code') === 'SEATS_UNAVAILABLE') {
      seatsTakenMs.add(res.timings.duration);
      holdSeatsTaken.add(1);
      forget(byZone, res.json('error.details.seat_ids') || seats);
      continue;
    }
    otherMs.add(res.timings.duration);
    holdOtherError.add(1);
    return;
  }
}
