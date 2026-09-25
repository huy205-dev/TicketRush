-- name: InsertHeldOrder :one
INSERT INTO orders (id, event_id, user_id, status, total_vnd, idempotency_key, request_hash, hold_expires_at)
VALUES (
  @order_id,
  @event_id,
  @user_id,
  'HELD',
  @total_vnd,
  @idempotency_key,
  @request_hash,
  now() + @hold_ttl::interval
)
RETURNING *;

-- name: InsertOrderSeats :exec
-- seat_ids and prices have the same length; the two unnests are zipped.
INSERT INTO order_seats (order_id, event_id, seat_id, price_vnd)
SELECT @order_id::uuid, @event_id::bigint, unnest(@seat_ids::text[]), unnest(@prices::bigint[]);

-- name: GetOrder :one
SELECT * FROM orders WHERE id = $1;

-- name: GetOrderForUpdate :one
-- Locks the order row so its status cannot change until the transaction ends.
SELECT * FROM orders WHERE id = $1
FOR UPDATE;

-- name: GetOrderByKey :one
SELECT * FROM orders
WHERE user_id = @user_id AND idempotency_key = @idempotency_key;

-- name: ListOrderSeats :many
SELECT seat_id, price_vnd
FROM order_seats
WHERE order_id = $1
ORDER BY seat_id;

-- name: TransitionOrder :one
-- Every status change goes through this conditional update. Zero rows means
-- the order was not in from_status; the caller re-reads and decides.
UPDATE orders
SET status = @to_status, updated_at = now()
WHERE id = @order_id AND status = @from_status
RETURNING *;

-- name: ExpireDueOrders :many
-- Moves up to batch_size HELD orders whose hold has passed (database clock)
-- to EXPIRED (SPEC.md 9.4). SKIP LOCKED lets several workers run side by
-- side and skips orders locked by a concurrent cancel or payment.
WITH due AS (
  SELECT id
  FROM orders
  WHERE status = 'HELD' AND hold_expires_at < now()
  ORDER BY hold_expires_at
  LIMIT @batch_size::int
  FOR UPDATE SKIP LOCKED
)
UPDATE orders o
SET status = 'EXPIRED', updated_at = now()
FROM due
WHERE o.id = due.id AND o.status = 'HELD'
RETURNING o.id, o.event_id, o.user_id, o.total_vnd;

-- name: ListSeatsOfOrders :many
SELECT order_id, seat_id, price_vnd
FROM order_seats
WHERE order_id = ANY(@order_ids::uuid[])
ORDER BY order_id, seat_id;
