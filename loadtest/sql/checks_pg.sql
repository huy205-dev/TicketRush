-- Extra checks for INVENTORY_BACKEND=pg: live seat_holds rows and the seats
-- of HELD orders must match one to one. Valid right after a run, while no
-- hold has expired yet (HOLD_TTL is 10 minutes).

SELECT 'info:live_seat_holds', count(*) FROM seat_holds WHERE expires_at > now();

SELECT 'hold_without_held_order', count(*)
FROM seat_holds h
WHERE h.expires_at > now()
  AND NOT EXISTS (
    SELECT 1
    FROM order_seats os JOIN orders o ON o.id = os.order_id
    WHERE o.status = 'HELD' AND o.id = h.order_id AND os.seat_id = h.seat_id
  );

SELECT 'held_order_seat_without_hold', count(*)
FROM order_seats os JOIN orders o ON o.id = os.order_id
WHERE o.status = 'HELD'
  AND NOT EXISTS (
    SELECT 1 FROM seat_holds h
    WHERE h.order_id = o.id AND h.seat_id = os.seat_id AND h.expires_at > now()
  );
