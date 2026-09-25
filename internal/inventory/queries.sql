-- name: LockSeats :many
-- Locks the requested seat rows in seat_id order so concurrent holds on
-- overlapping seats always lock in the same order and cannot deadlock.
SELECT seat_id
FROM seats
WHERE event_id = @event_id::bigint AND seat_id = ANY(@seat_ids::text[])
ORDER BY seat_id
FOR UPDATE;

-- name: TakenSeats :many
-- Seats held by another order (and not yet expired) or already ticketed.
-- A hold by the same order does not count, which makes Hold idempotent.
SELECT seat_id
FROM seat_holds
WHERE event_id = @event_id::bigint
  AND seat_id = ANY(@seat_ids::text[])
  AND expires_at > now()
  AND order_id <> @order_id::uuid
UNION
SELECT seat_id
FROM tickets
WHERE event_id = @event_id::bigint AND seat_id = ANY(@seat_ids::text[])
ORDER BY seat_id;

-- name: UpsertHolds :exec
INSERT INTO seat_holds (event_id, seat_id, order_id, expires_at)
SELECT @event_id::bigint, s, @order_id::uuid, now() + @ttl::interval
FROM unnest(@seat_ids::text[]) AS s
ON CONFLICT (event_id, seat_id) DO UPDATE
SET order_id = EXCLUDED.order_id,
    expires_at = EXCLUDED.expires_at;

-- name: DeleteHolds :execrows
DELETE FROM seat_holds
WHERE event_id = @event_id::bigint
  AND seat_id = ANY(@seat_ids::text[])
  AND order_id = @order_id::uuid;

-- name: CountOrderTickets :one
SELECT count(*)
FROM tickets
WHERE event_id = @event_id::bigint
  AND seat_id = ANY(@seat_ids::text[])
  AND order_id = @order_id::uuid;

-- name: SeatStatuses :many
-- Only seats that are not available; callers default the rest to AVAILABLE.
-- A seat can appear twice (ticket plus stale hold); SOLD wins.
SELECT seat_id, 'SOLD'::text AS status
FROM tickets
WHERE event_id = @event_id::bigint AND seat_id = ANY(@seat_ids::text[])
UNION ALL
SELECT seat_id, 'HELD'::text AS status
FROM seat_holds
WHERE event_id = @event_id::bigint
  AND seat_id = ANY(@seat_ids::text[])
  AND expires_at > now();
