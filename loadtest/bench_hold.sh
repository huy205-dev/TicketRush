#!/usr/bin/env bash
# Reference measurement of the opening-sale scenario (docs/results.md).
#
# Repeats RUNS times (default 3):
#   1. reset the database (goose reset + up), flush Redis, seed the demo event;
#   2. start a fresh booking process with INVENTORY_BACKEND=<backend>;
#   3. prepare access tokens (loadtest/make_tokens.js, dev-login) so that
#      logging in stays out of the measured load;
#   4. run loadtest/hold_contention.js (BUYER_MODE=iteration);
#   5. stop booking, check the data (loadtest/sql/*.sql, plus
#      loadtest/check_redis.sh for the redis backend) and break down the
#      server-side latency from the booking log (analyze_booking_log.sh).
# Then reports the median, min and max of every metric across the runs.
#
# Usage: loadtest/bench_hold.sh <pg|redis> [runs]    (or: make bench-hold BACKEND=pg)
# Output: loadtest/out/bench-hold-<backend>-<UTC time>/summary.md, plus the
#         raw k6 summary, CSV, booking log and checks of each run.
set -euo pipefail

usage="usage: bench_hold.sh <pg|redis> [runs]"
backend=${1:?$usage}
runs=${2:-3}
case "$backend" in pg | redis) ;; *) echo "$usage" >&2; exit 2 ;; esac
[[ "$runs" =~ ^[1-9][0-9]*$ ]] || { echo "runs must be a positive integer" >&2; exit 2; }

cd "$(dirname "$0")/.."
for tool in k6 jq goose docker curl lsof go; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done
[ -f .env ] || { echo ".env is missing; run: make .env" >&2; exit 1; }
if lsof -nP -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "port 8080 is in use; stop the running booking service first" >&2
  exit 1
fi

set -a
. ./.env
set +a
export INVENTORY_BACKEND=$backend # after .env so it takes precedence
export BUYER_MODE=iteration
k6_params="VUS=${VUS:-2000}, RAMP=${RAMP:-10s}, HOLD=${HOLD:-60s}"
token_count=$(( ${VUS:-2000} * 25 )) # 25 orders per VU before it runs out
if [ -n "${VUS:-}${RAMP:-}${HOLD:-}" ]; then
  echo "warning: non-default k6 parameters ($k6_params); not a reference measurement" >&2
fi
compose=(docker compose -f deploy/compose.yaml)

commit=$(git rev-parse --short HEAD)
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  commit="$commit-dirty"
  echo "warning: uncommitted changes; results will be labelled $commit" >&2
fi
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
out="loadtest/out/bench-hold-$backend-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$out"

echo "building booking and seed at $commit"
go build -o "$out/booking" ./cmd/booking
go build -o "$out/seed" ./cmd/seed

booking_pid=""
stop_booking() {
  if [ -n "$booking_pid" ]; then
    kill -TERM "$booking_pid" 2>/dev/null || true
    wait "$booking_pid" 2>/dev/null || true
    booking_pid=""
  fi
}
trap stop_booking EXIT
trap 'stop_booking; exit 130' INT TERM

# infra_started prints when each infrastructure container last started. A
# change during a run means something restarted it, and the run is void.
infra_started() {
  docker inspect -f '{{.Name}} {{.State.StartedAt}}' \
    $("${compose[@]}" ps -q postgres redis redpanda) | sort
}

# check_sql prints "<name>|<number>" rows for one checks file.
check_sql() {
  "${compose[@]}" exec -T postgres psql "$DATABASE_URL" -At -F '|' -v ON_ERROR_STOP=1 -f - <"$1"
}

: >"$out/runs.jsonl"
for i in $(seq 1 "$runs"); do
  echo "== run $i/$runs"
  started_before=$(infra_started)
  goose -dir migrations postgres "$DATABASE_URL" reset >/dev/null 2>&1
  goose -dir migrations postgres "$DATABASE_URL" up >/dev/null 2>&1
  "${compose[@]}" exec -T redis redis-cli FLUSHALL >/dev/null
  "$out/seed" >/dev/null

  "$out/booking" >"$out/run-$i-booking.log" 2>&1 &
  booking_pid=$!
  ready=""
  for _ in $(seq 1 50); do
    if curl -sf http://localhost:8080/readyz >/dev/null; then ready=1; break; fi
    sleep 0.2
  done
  [ -n "$ready" ] || { echo "booking did not become ready; see $out/run-$i-booking.log" >&2; exit 1; }

  if ! COUNT=$token_count TOKENS_OUT="$out/run-$i-tokens.json" k6 run -q loadtest/make_tokens.js >"$out/run-$i-tokens.txt" 2>&1; then
    echo "token preparation failed; see $out/run-$i-tokens.txt" >&2
    exit 1
  fi

  k6_exit=0
  # open() in k6 resolves relative paths against the script's directory.
  TOKENS_FILE="$PWD/$out/run-$i-tokens.json" k6 run --summary-export="$out/run-$i-summary.json" --out csv="$out/run-$i.csv.gz" \
    loadtest/hold_contention.js >"$out/run-$i-k6.txt" 2>&1 || k6_exit=$?
  rm -f "$out/run-$i-tokens.json"
  stop_booking
  if [ "$(infra_started)" != "$started_before" ]; then
    echo "an infrastructure container restarted during run $i; results are void" >&2
    echo "before: $started_before" >&2
    echo "after:  $(infra_started)" >&2
    exit 1
  fi
  # 99 means thresholds were crossed, which is a result, not a failure.
  if [ "$k6_exit" -ne 0 ] && [ "$k6_exit" -ne 99 ]; then
    echo "k6 failed with exit $k6_exit; see $out/run-$i-k6.txt" >&2
    exit 1
  fi

  peak=$(./loadtest/peak_rps.sh "$out/run-$i.csv.gz" hold json)
  {
    check_sql loadtest/sql/checks_common.sql
    if [ "$backend" = pg ]; then check_sql loadtest/sql/checks_pg.sql; fi
    if [ "$backend" = redis ]; then ./loadtest/check_redis.sh 1; fi
  } >"$out/run-$i-checks.txt"
  checks=$(jq -R -s 'split("\n") | map(select(length > 0) | split("|") | {(.[0]): (.[1] | tonumber)}) | add' "$out/run-$i-checks.txt")
  log_errors=$(grep -c '"level":"ERROR"' "$out/run-$i-booking.log" || true)
  ./loadtest/analyze_booking_log.sh "$out/run-$i-booking.log" >"$out/run-$i-server.json"

  jq -c -n \
    --argjson run "$i" --argjson k6_exit "$k6_exit" --argjson peak "$peak" \
    --argjson checks "$checks" --argjson log_errors "$log_errors" \
    --slurpfile summary "$out/run-$i-summary.json" --slurpfile server "$out/run-$i-server.json" '
    $summary[0].metrics as $m
    | $server[0] as $s
    | $m["http_req_duration{name:hold}"] as $d
    | {
        run: $run,
        hold_requests: $peak.requests,
        created: $m.hold_created.count,
        seats_unavailable: ($m.hold_seats_unavailable.count // 0),
        other_errors: ($m.hold_other_error.count // 0),
        peak_rps: $peak.peak_rps,
        mean_rps: $peak.mean_rps,
        p50_ms: $d.med, p95_ms: $d["p(95)"], p99_ms: $d["p(99)"], max_ms: $d.max,
        error_rate: $m["http_req_failed{name:hold}"].value,
        # In k6 summary exports a threshold value of true means it was crossed.
        p99_threshold_ok: ($d.thresholds["p(99)<200"] | not),
        all_requests: $m.http_reqs.count,
        created_p50_ms: $m.hold_created_ms.med, created_p99_ms: $m.hold_created_ms["p(99)"],
        conflict_p50_ms: $m.hold_seats_unavailable_ms.med, conflict_p99_ms: $m.hold_seats_unavailable_ms["p(99)"],
        tokens_exhausted: ($m.tokens_exhausted.count // 0),
        srv_201_p50_ms: $s.orders["201"].duration_ms.p50, srv_201_p99_ms: $s.orders["201"].duration_ms.p99,
        srv_409_p50_ms: $s.orders["409"].duration_ms.p50, srv_409_p99_ms: $s.orders["409"].duration_ms.p99,
        srv_201_acquire_p99_ms: $s.orders["201"].db_acquire_ms.p99, srv_409_acquire_p99_ms: $s.orders["409"].db_acquire_ms.p99,
        srv_409_acquire_p50_ms: $s.orders["409"].db_acquire_ms.p50,
        srv_409_query_p99_ms: $s.orders["409"].db_query_ms.p99, srv_409_redis_p99_ms: $s.orders["409"].redis_ms.p99,
        srv_409_share_db_wait: $s.orders["409"].share_waiting_for_db, srv_201_share_db_wait: $s.orders["201"].share_waiting_for_db,
        pool_empty_acquires: $s.pool.empty_acquires, pool_empty_wait_ms: $s.pool.empty_acquire_wait_ms,
        pool_acquires: $s.pool.acquires,
        orders: $checks["info:orders"],
        invariant_violations: ([$checks | to_entries[] | select(.key | startswith("info:") | not) | .value] | add),
        booking_log_errors: $log_errors,
        k6_exit: $k6_exit
      }' >>"$out/runs.jsonl"
  jq -r '"   hold=\(.hold_requests) created=\(.created) peak=\(.peak_rps)/s p50=\(.p50_ms)ms p99=\(.p99_ms)ms errors=\(.error_rate) violations=\(.invariant_violations)"' <(tail -1 "$out/runs.jsonl")

  if [ "$i" -lt "$runs" ]; then sleep 5; fi # let the machine settle
done

# ---- summary ---------------------------------------------------------------

if [ "$(uname)" = Darwin ]; then
  machine="$(sysctl -n machdep.cpu.brand_string), $(sysctl -n hw.ncpu) logical CPUs, $(($(sysctl -n hw.memsize) / 1073741824)) GiB RAM, macOS $(sw_vers -productVersion)"
else
  machine="$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs), $(nproc) CPUs, $(($(awk '/MemTotal/ {print $2}' /proc/meminfo) / 1048576)) GiB RAM, $(uname -sr)"
fi
docker_vm=$(docker info --format '{{.NCPU}} CPU, {{.MemTotal}}' | awk -F', ' '{printf "%s, %.1f GiB", $1, $2 / 1073741824}')
pg_settings=$("${compose[@]}" exec -T postgres psql "$DATABASE_URL" -At -c \
  "SELECT 'PostgreSQL ' || current_setting('server_version') || ', shared_buffers=' || current_setting('shared_buffers') || ', max_connections=' || current_setting('max_connections')")
booking_cfg=$(grep -m1 '"msg":"booking started"' "$out/run-1-booking.log" |
  jq -r '.config | "INVENTORY_BACKEND=\(.inventory_backend), DB_MAX_CONNS=\(.db_max_conns), HOLD_TTL=\(.hold_ttl), HOLD_GRACE=\(.hold_grace)"')

jq -s '
  def median: sort | if length % 2 == 1 then .[length / 2 | floor] else (.[length / 2 - 1] + .[length / 2]) / 2 end;
  def stat(f): [.[] | f] | {median: median, min: min, max: max};
  {
    runs: length,
    hold_requests: stat(.hold_requests), created: stat(.created),
    seats_unavailable: stat(.seats_unavailable),
    peak_rps: stat(.peak_rps), mean_rps: stat(.mean_rps),
    p50_ms: stat(.p50_ms), p95_ms: stat(.p95_ms), p99_ms: stat(.p99_ms), max_ms: stat(.max_ms),
    error_rate: stat(.error_rate),
    created_p50_ms: stat(.created_p50_ms), created_p99_ms: stat(.created_p99_ms),
    conflict_p50_ms: stat(.conflict_p50_ms), conflict_p99_ms: stat(.conflict_p99_ms),
    srv_201_p50_ms: stat(.srv_201_p50_ms), srv_201_p99_ms: stat(.srv_201_p99_ms),
    srv_409_p50_ms: stat(.srv_409_p50_ms), srv_409_p99_ms: stat(.srv_409_p99_ms),
    srv_201_acquire_p99_ms: stat(.srv_201_acquire_p99_ms),
    srv_409_acquire_p50_ms: stat(.srv_409_acquire_p50_ms), srv_409_acquire_p99_ms: stat(.srv_409_acquire_p99_ms),
    srv_409_query_p99_ms: stat(.srv_409_query_p99_ms), srv_409_redis_p99_ms: stat(.srv_409_redis_p99_ms),
    srv_201_share_db_wait: stat(.srv_201_share_db_wait), srv_409_share_db_wait: stat(.srv_409_share_db_wait),
    pool_empty_acquires: stat(.pool_empty_acquires), pool_empty_wait_ms: stat(.pool_empty_wait_ms),
    tokens_exhausted: ([.[].tokens_exhausted] | add),
    p99_threshold_ok_runs: ([.[] | select(.p99_threshold_ok)] | length),
    invariants_ok_runs: ([.[] | select(.invariant_violations == 0)] | length),
    booking_log_errors: ([.[].booking_log_errors] | add),
    per_run: .
  }' "$out/runs.jsonl" >"$out/summary.json"

jq -r \
  --arg backend "$backend" --arg commit "$commit" --arg started "$started" \
  --arg machine "$machine" --arg docker_vm "$docker_vm" --arg pg "$pg_settings" \
  --arg booking "$booking_cfg" --arg k6v "$(k6 version | head -1 | awk '{print $2}')" --arg k6p "$k6_params" '
  def r2: (. * 100 | round) / 100;
  def fmt: if type == "number" then (if . == (. | floor) then tostring else (r2 | tostring) end) else tostring end;
  def row(name; s): "| \(name) | \(s.median | fmt) | \(s.min | fmt) | \(s.max | fmt) |";
  "## bench-hold: backend \($backend), \(.runs) lần",
  "",
  "- Bắt đầu: \($started), commit `\($commit)`",
  "- Máy: \($machine); Docker VM: \($docker_vm)",
  "- \($pg); booking: \($booking)",
  "- k6 \($k6v), kịch bản `loadtest/hold_contention.js`, BUYER_MODE=iteration, \($k6p); reset DB + flush Redis + seed trước mỗi lần",
  "",
  "| Chỉ số | Trung vị | Min | Max |",
  "|---|---|---|---|",
  row("Request giữ ghế"; .hold_requests),
  row("201 (đơn tạo được)"; .created),
  row("409 SEATS_UNAVAILABLE"; .seats_unavailable),
  row("RPS giữ ghế đỉnh (req/s)"; .peak_rps),
  row("RPS giữ ghế TB trên giây có tải (req/s)"; .mean_rps),
  row("p50 (ms)"; .p50_ms),
  row("p95 (ms)"; .p95_ms),
  row("p99 (ms)"; .p99_ms),
  row("max (ms)"; .max_ms),
  row("Tỉ lệ lỗi"; .error_rate),
  row("p50 request thắng (201), k6 (ms)"; .created_p50_ms),
  row("p99 request thắng (201), k6 (ms)"; .created_p99_ms),
  row("p50 request thua (409), k6 (ms)"; .conflict_p50_ms),
  row("p99 request thua (409), k6 (ms)"; .conflict_p99_ms),
  "",
  "Phía server, từ log booking (POST /v1/orders; thời gian trong process, không tính mạng và hàng đợi của k6):",
  "",
  "| Chỉ số | Trung vị | Min | Max |",
  "|---|---|---|---|",
  row("201: p50 (ms)"; .srv_201_p50_ms),
  row("201: p99 (ms)"; .srv_201_p99_ms),
  row("201: p99 chờ pgxpool (ms)"; .srv_201_acquire_p99_ms),
  row("201: tỉ lệ thời gian chờ pgxpool"; .srv_201_share_db_wait),
  row("409: p50 (ms)"; .srv_409_p50_ms),
  row("409: p99 (ms)"; .srv_409_p99_ms),
  row("409: p50 chờ pgxpool (ms)"; .srv_409_acquire_p50_ms),
  row("409: p99 chờ pgxpool (ms)"; .srv_409_acquire_p99_ms),
  row("409: p99 thời gian query PG (ms)"; .srv_409_query_p99_ms),
  row("409: p99 thời gian Redis (ms)"; .srv_409_redis_p99_ms),
  row("409: tỉ lệ thời gian chờ pgxpool"; .srv_409_share_db_wait),
  row("Số lần acquire phải chờ (pool rỗng)"; .pool_empty_acquires),
  row("Tổng thời gian chờ khi pool rỗng (ms)"; .pool_empty_wait_ms),
  "",
  "- VU hết token: \(.tokens_exhausted) lần (phải là 0, nếu không kịch bản đã bị cắt bớt)",
  "- Ngưỡng p99 < 200 ms đạt ở \(.p99_threshold_ok_runs)/\(.runs) lần",
  "- Kiểm tra dữ liệu (loadtest/sql) không vi phạm ở \(.invariants_ok_runs)/\(.runs) lần; dòng ERROR trong log booking: \(.booking_log_errors)",
  "",
  "| Lần | Request | 201 / 409 | Đỉnh | p50 | p95 | p99 | Lỗi | Vi phạm |",
  "|---|---|---|---|---|---|---|---|---|",
  (.per_run[] | "| \(.run) | \(.hold_requests) | \(.created) / \(.seats_unavailable) | \(.peak_rps) | \(.p50_ms | fmt) | \(.p95_ms | fmt) | \(.p99_ms | fmt) | \(.error_rate | fmt) | \(.invariant_violations) |")
  ' "$out/summary.json" >"$out/summary.md"

echo
cat "$out/summary.md"
echo
echo "saved to $out"
