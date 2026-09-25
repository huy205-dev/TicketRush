-- name: GetEvent :one
SELECT id, name, venue, starts_at, sale_opens_at, max_seats_per_order
FROM events
WHERE id = $1;

-- name: ListSeats :many
-- Seats in display order: most expensive zone first, then row and seat number.
SELECT seat_id, zone, row_label, seat_no, price_vnd
FROM seats
WHERE event_id = $1
ORDER BY price_vnd DESC, zone, length(row_label), row_label, seat_no;

-- name: IsSaleOpen :one
-- Sale opening is a deadline decision, so it uses the database clock.
SELECT sale_opens_at <= now() AS open
FROM events
WHERE id = $1;

-- name: CreateEvent :one
INSERT INTO events (name, venue, starts_at, sale_opens_at, max_seats_per_order)
VALUES (
  @name,
  @venue,
  now() + @starts_in::interval,
  now() + @sale_opens_in::interval,
  @max_seats_per_order
)
RETURNING id;

-- name: InsertSeats :copyfrom
INSERT INTO seats (event_id, seat_id, zone, row_label, seat_no, price_vnd)
VALUES ($1, $2, $3, $4, $5, $6);
