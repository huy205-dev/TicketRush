-- Data checks after a load test run, for every inventory backend.
-- Each row is "<check>|<number>". Rows named info:* are informational; for
-- every other row the number is a count of violations and must be 0.
-- tools/invariants (M7) will replace this with the full invariant suite.

SELECT 'info:orders', count(*) FROM orders;

SELECT 'info:seats_in_held_orders', count(*)
FROM order_seats os JOIN orders o ON o.id = os.order_id
WHERE o.status = 'HELD';

-- No seat may belong to two HELD orders.
SELECT 'seat_in_two_held_orders', count(*) FROM (
  SELECT os.event_id, os.seat_id
  FROM order_seats os JOIN orders o ON o.id = os.order_id
  WHERE o.status = 'HELD'
  GROUP BY os.event_id, os.seat_id
  HAVING count(*) > 1
) d;

-- An order's total is the sum of its seat prices.
SELECT 'order_total_mismatch', count(*)
FROM orders o
WHERE o.total_vnd <> (SELECT coalesce(sum(price_vnd), 0) FROM order_seats WHERE order_id = o.id);

-- Every order wrote its order.held event in the same transaction.
SELECT 'order_without_held_event', count(*)
FROM orders o
WHERE NOT EXISTS (
  SELECT 1 FROM outbox m WHERE m.msg_key = o.id::text AND m.event_type = 'order.held'
);
