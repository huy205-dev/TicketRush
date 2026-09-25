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

-- name: LockOrderForPayment :one
-- Locks the order for the webhook transaction (SPEC.md 9.3) and says, by the
-- database clock, whether the hold is still running.
SELECT o.*, (now() <= o.hold_expires_at) AS hold_active
FROM orders o
WHERE o.id = $1
FOR UPDATE;

-- name: InsertPayment :one
-- Returns nothing when the provider_txn_id was already recorded: the webhook
-- is a duplicate. The outcome is set once the transaction knows it.
INSERT INTO payments (provider_txn_id, order_id, amount_vnd, status, outcome)
VALUES (@provider_txn_id, @order_id, @amount_vnd, @status, '')
ON CONFLICT (provider_txn_id) DO NOTHING
RETURNING provider_txn_id;

-- name: SetPaymentOutcome :exec
UPDATE payments SET outcome = @outcome WHERE provider_txn_id = @provider_txn_id;

-- name: InsertTickets :exec
-- One ticket per seat; UNIQUE (event_id, seat_id) is the last guard against
-- selling a seat twice. ids and seat_ids have the same length.
INSERT INTO tickets (id, order_id, event_id, seat_id)
SELECT unnest(@ids::uuid[]), @order_id::uuid, @event_id::bigint, unnest(@seat_ids::text[]);

-- name: InsertRefund :exec
INSERT INTO refunds (provider_txn_id, order_id, amount_vnd, reason, status)
VALUES (@provider_txn_id, @order_id, @amount_vnd, @reason, 'PENDING');

-- name: ListOrderTickets :many
SELECT id, seat_id, qr_payload, issued_at
FROM tickets
WHERE order_id = $1
ORDER BY seat_id;

-- name: SetTicketQR :exec
-- Fills a ticket's QR payload once; a ticket already issued keeps its own.
UPDATE tickets
SET qr_payload = @qr_payload, issued_at = now()
WHERE id = @ticket_id AND qr_payload IS NULL;

-- name: GetRefund :one
SELECT * FROM refunds WHERE provider_txn_id = $1;

-- name: CompleteRefund :one
-- PENDING -> SUCCEEDED once; zero rows means it was already completed.
UPDATE refunds
SET status = 'SUCCEEDED', completed_at = now()
WHERE provider_txn_id = $1 AND status = 'PENDING'
RETURNING *;
