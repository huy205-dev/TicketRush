#!/usr/bin/env bash
# Breaks down server-side latency of POST /v1/orders from a booking log
# (JSON lines): for 201, 409 and other statuses, the p50/p95/p99 of the whole
# request and of the time spent waiting for a pgxpool connection
# (db_acquire_ms), running PostgreSQL statements (db_query_ms) and talking to
# Redis (redis_ms). Also sums the "pgxpool stats" lines.
#
# Usage: loadtest/analyze_booking_log.sh <booking.log>   (prints JSON)
set -euo pipefail
log=${1:?usage: analyze_booking_log.sh <booking.log>}

jq -s '
  def pct(p): if length == 0 then null else sort | .[((length - 1) * p | floor)] end;
  def dist(f): [.[] | (f // 0)] | {p50: pct(0.5), p95: pct(0.95), p99: pct(0.99), mean: (if length == 0 then null else add / length end)};
  def summary: {
    n: length,
    duration_ms: dist(.duration_ms),
    db_acquire_ms: dist(.db_acquire_ms),
    db_query_ms: dist(.db_query_ms),
    redis_ms: dist(.redis_ms),
    share_waiting_for_db: (if length == 0 then null else ([.[] | .db_acquire_ms // 0] | add) / ([.[].duration_ms] | add) end),
    with_db: ([.[] | select((.db_acquires // 0) > 0)] | length)
  };
  (map(select(.msg == "http request" and .method == "POST" and .route == "/v1/orders"))) as $orders
  | (map(select(.msg == "pgxpool stats"))) as $pool
  | {
      orders: {
        "201": ($orders | map(select(.status == 201)) | summary),
        "409": ($orders | map(select(.status == 409)) | summary),
        other: ($orders | map(select(.status != 201 and .status != 409)) | summary)
      },
      pool: {
        seconds_logged: ($pool | length),
        acquires: ([$pool[].acquires] | add),
        empty_acquires: ([$pool[].empty_acquires] | add),
        empty_acquire_wait_ms: ([$pool[].empty_acquire_wait_ms] | add),
        max_acquired_conns: ([$pool[].acquired_conns] | max),
        max_conns: ([$pool[].max_conns] | max)
      }
    }
' "$log"
