// Opening-sale scenario (SPEC.md 13.3): ramp from 0 to 2,000 buyers in 10s,
// keep them for 60s. 70% of buyers want VIP seats.
//
// Each VU is one buyer with its own user id:
//   1. dev-login once;
//   2. load the seat map and pick 1-4 random AVAILABLE seats, preferring the
//      buyer's zone and falling back to any zone when it is sold out;
//   3. POST /v1/orders; on 409 SEATS_UNAVAILABLE drop the taken seats from
//      the local copy and retry, at most 3 retries per iteration;
//   4. after a successful hold the buyer is done and idles, because a user
//      may hold only one order per event.
// When nothing is available anywhere the buyer sleeps 1s and looks again.
//
// Run with the booking service up and a freshly seeded event:
//   make db-reset seed && make run-booking   (another terminal)
//   make load-hold
//
// Env: BASE_URL (default http://localhost:8080), EVENT_ID (default 1),
//      VUS (default 2000), RAMP (default 10s), HOLD (default 60s).
//      Change VUS/RAMP/HOLD only for smoke runs; results use the defaults.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const EVENT_ID = Number(__ENV.EVENT_ID || 1);
const VUS = Number(__ENV.VUS || 2000);
const RAMP = __ENV.RAMP || '10s';
const HOLD = __ENV.HOLD || '60s';
const VIP_SHARE = 0.7;
const MAX_RETRIES = 3;

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

// Module scope is per VU: this is the buyer's state.
let token = null;
let done = false;

export function setup() {
  const res = http.get(`${BASE_URL}/v1/events/${EVENT_ID}`);
  if (res.status !== 200) {
    throw new Error(`event ${EVENT_ID} not found (${res.status}); run make db-reset seed`);
  }
  // A per-run offset keeps user ids unique across runs without a db reset.
  return { userBase: (Date.now() % 1_000_000) * 100_000 };
}

function login(userId) {
  const res = http.post(`${BASE_URL}/v1/auth/dev-login`, JSON.stringify({ user_id: userId }), {
    headers: { 'Content-Type': 'application/json' },
    tags: { name: 'login' },
  });
  check(res, { 'login 200': (r) => r.status === 200 });
  return res.status === 200 ? res.json('access_token') : null;
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

export default function (data) {
  if (done) {
    sleep(1);
    return;
  }
  if (token === null) {
    token = login(data.userBase + exec.vu.idInTest);
    if (token === null) {
      sleep(1);
      return;
    }
  }

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
      holdCreated.add(1);
      done = true;
      return;
    }
    if (res.status === 409 && res.json('error.code') === 'SEATS_UNAVAILABLE') {
      holdSeatsTaken.add(1);
      forget(byZone, res.json('error.details.seat_ids') || seats);
      continue;
    }
    holdOtherError.add(1);
    return;
  }
}
