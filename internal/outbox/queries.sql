-- name: InsertMessage :one
-- The id is drawn first so the stored payload can carry it as event_id
-- (consumers deduplicate on it). occurred_at uses the database clock.
INSERT INTO outbox (id, topic, msg_key, event_type, payload)
SELECT
  s.id,
  @topic::text,
  @msg_key::text,
  @event_type::text,
  @payload::jsonb || jsonb_build_object(
    'event_id', s.id,
    'event_type', @event_type::text,
    'occurred_at', now()
  )
FROM (SELECT nextval(pg_get_serial_sequence('outbox', 'id')) AS id) AS s
RETURNING id;
