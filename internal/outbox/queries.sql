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

-- name: LockUnpublished :many
-- Oldest unpublished messages, locked until the relay commits. SKIP LOCKED
-- lets a second relay take the next batch instead of waiting (SPEC.md 9.5).
SELECT id, topic, msg_key, event_type, payload
FROM outbox
WHERE published_at IS NULL
ORDER BY id
LIMIT @batch_size::int
FOR UPDATE SKIP LOCKED;

-- name: MarkPublished :exec
UPDATE outbox
SET published_at = now()
WHERE id = ANY(@ids::bigint[]);

-- name: DeletePublishedBefore :execrows
-- Removes at most batch_size messages published longer ago than retention.
DELETE FROM outbox
WHERE id IN (
  SELECT id
  FROM outbox
  WHERE published_at < now() - @retention::interval
  ORDER BY id
  LIMIT @batch_size::int
);

-- name: CountUnpublished :one
SELECT count(*) FROM outbox WHERE published_at IS NULL;
