#!/usr/bin/env bash
# Compares the seat keys in Redis with PostgreSQL for one event, right after
# a load test (INVENTORY_BACKEND=redis). Expected state from PostgreSQL:
# every seat of a HELD order is "held:<orderId>", every ticket is
# "sold:<orderId>", and Redis has nothing else.
#
# Prints "<check>|<number>" rows like loadtest/sql/*.sql; info:* rows are
# informational, the others count violations and must be 0.
# tools/invariants (M7) will replace this.
#
# Usage: loadtest/check_redis.sh [event_id]   (default 1)
set -euo pipefail

event=${1:-1}
[[ "$event" =~ ^[1-9][0-9]*$ ]] || { echo "event_id must be a positive integer" >&2; exit 2; }

cd "$(dirname "$0")/.."
set -a
. ./.env
set +a
compose=(docker compose -f deploy/compose.yaml)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export LC_ALL=C

# Redis: "<seat_id> <held|sold> <order_id>" for every seat key of the event.
"${compose[@]}" exec -T redis sh -c "
  redis-cli --scan --pattern 'seat:{$event}:*' > /tmp/keys
  if [ -s /tmp/keys ]; then
    xargs redis-cli MGET < /tmp/keys > /tmp/values
    paste -d ' ' /tmp/keys /tmp/values
  fi
  rm -f /tmp/keys /tmp/values
" | awk -v prefix="seat:{$event}:" '
  # Plain string prefix: sub() would read the braces as a regex interval.
  index($1, prefix) == 1 { split($2, v, ":"); print substr($1, length(prefix) + 1), v[1], v[2]; next }
  { print "unexpected key " $1 > "/dev/stderr"; exit 1 }
' | sort >"$tmp/redis"

# PostgreSQL: the same triples as they should be.
"${compose[@]}" exec -T postgres psql "$DATABASE_URL" -At -F ' ' -v ON_ERROR_STOP=1 -v event="$event" -f - <<'SQL' | sort >"$tmp/pg"
SELECT os.seat_id, 'held', o.id
FROM order_seats os JOIN orders o ON o.id = os.order_id
WHERE o.status = 'HELD' AND o.event_id = :event
UNION ALL
SELECT t.seat_id, 'sold', t.order_id
FROM tickets t
WHERE t.event_id = :event;
SQL

echo "info:redis_seat_keys|$(wc -l <"$tmp/redis" | tr -d ' ')"
echo "info:pg_expected_keys|$(wc -l <"$tmp/pg" | tr -d ' ')"
echo "redis_key_not_in_pg|$(comm -23 "$tmp/redis" "$tmp/pg" | wc -l | tr -d ' ')"
echo "pg_state_missing_in_redis|$(comm -13 "$tmp/redis" "$tmp/pg" | wc -l | tr -d ' ')"
