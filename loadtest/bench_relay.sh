#!/usr/bin/env bash
# Measures outbox relay throughput: how fast a backlog of ROWS outbox rows
# (default 200,000) is published to Kafka. Repeats RUNS times (default 3) on
# a freshly reset database and reports rows/s as median, min and max.
#
# Usage: loadtest/bench_relay.sh [rows] [runs]
# Needs `make up`; the relay must not be running.
set -euo pipefail

rows=${1:-200000}
runs=${2:-3}
[[ "$rows" =~ ^[1-9][0-9]*$ && "$runs" =~ ^[1-9][0-9]*$ ]] || { echo "usage: bench_relay.sh [rows] [runs]" >&2; exit 2; }

cd "$(dirname "$0")/.."
for tool in go goose docker python3; do command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }; done
if pgrep -f 'cmd/relay|/relay$' >/dev/null; then echo "a relay is already running; stop it first" >&2; exit 1; fi
set -a
. ./.env
set +a
compose=(docker compose -f deploy/compose.yaml)
psql() { "${compose[@]}" exec -T postgres psql "$DATABASE_URL" -At -v ON_ERROR_STOP=1 "$@"; }
now() { python3 -c 'import time; print(time.time())'; }

commit=$(git rev-parse --short HEAD)
[ -z "$(git status --porcelain --untracked-files=no)" ] || commit="$commit-dirty"
out="loadtest/out/bench-relay-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$out"
go build -o "$out/relay" ./cmd/relay

relay_pid=""
stop_relay() { if [ -n "$relay_pid" ]; then kill -TERM "$relay_pid" 2>/dev/null || true; wait "$relay_pid" 2>/dev/null || true; relay_pid=""; fi; }
trap stop_relay EXIT

: >"$out/runs.txt"
for i in $(seq 1 "$runs"); do
  goose -dir migrations postgres "$DATABASE_URL" reset >/dev/null 2>&1
  goose -dir migrations postgres "$DATABASE_URL" up >/dev/null 2>&1
  # Rows shaped like real order events (about 250 bytes of JSON each).
  psql -v rows="$rows" <<'SQL' >/dev/null
INSERT INTO outbox (topic, msg_key, event_type, payload)
SELECT 'orders.v1', k, 'order.held',
       jsonb_build_object('event_id', g, 'event_type', 'order.held', 'occurred_at', now(),
                          'order_id', k, 'event', 1, 'user_id', g,
                          'seat_ids', jsonb_build_array('CAT2-A-1', 'CAT2-A-2'), 'total_vnd', 2000000)
FROM generate_series(1, :rows) AS g, LATERAL (SELECT gen_random_uuid()::text AS k) AS u;
SQL

  start=$(now)
  "$out/relay" >"$out/run-$i-relay.log" 2>&1 &
  relay_pid=$!
  while :; do
    left=$(psql -c "SELECT count(*) FROM outbox WHERE published_at IS NULL")
    [ "$left" -eq 0 ] && break
    sleep 0.1
  done
  end=$(now)
  stop_relay
  # Includes relay start-up (connect, topic check) and up to 100 ms of
  # polling delay: a conservative figure. Large backlogs keep both small.
  secs=$(python3 -c "print(round($end - $start, 3))")
  rate=$(python3 -c "print(round($rows / ($end - $start)))")
  echo "$rate $secs" >>"$out/runs.txt"
  echo "run $i: $rows rows in ${secs}s = $rate rows/s"
  [ "$i" -lt "$runs" ] && sleep 3
done

sort -n "$out/runs.txt" | awk -v rows="$rows" -v commit="$commit" '
  { r[NR] = $1 }
  END {
    n = NR; med = (n % 2) ? r[(n + 1) / 2] : (r[n / 2] + r[n / 2 + 1]) / 2
    printf "bench-relay: %d rows, %d runs, commit %s\n", rows, n, commit
    printf "rows/s median %d, min %d, max %d\n", med, r[1], r[n]
  }' | tee "$out/summary.txt"
echo "saved to $out"
