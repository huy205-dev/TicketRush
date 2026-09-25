-- Schema from SPEC.md section 5.

-- +goose Up
CREATE TABLE users (
  id          BIGINT PRIMARY KEY,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE events (
  id             BIGSERIAL PRIMARY KEY,
  name           TEXT NOT NULL,
  venue          TEXT NOT NULL,
  starts_at      TIMESTAMPTZ NOT NULL,
  sale_opens_at  TIMESTAMPTZ NOT NULL,
  max_seats_per_order INT NOT NULL DEFAULT 4
);

CREATE TABLE seats (
  event_id   BIGINT NOT NULL REFERENCES events(id),
  seat_id    TEXT   NOT NULL,          -- 'VIP-A-12'
  zone       TEXT   NOT NULL,          -- 'VIP', 'CAT1', 'CAT2', 'GA'
  row_label  TEXT   NOT NULL,
  seat_no    INT    NOT NULL,
  price_vnd  BIGINT NOT NULL CHECK (price_vnd > 0),
  PRIMARY KEY (event_id, seat_id)
);

CREATE TABLE orders (
  id                UUID PRIMARY KEY,
  event_id          BIGINT NOT NULL REFERENCES events(id),
  user_id           BIGINT NOT NULL,
  status            TEXT   NOT NULL CHECK (status IN
                      ('HELD','PAID','TICKETED','EXPIRED','CANCELLED','REFUNDING','REFUNDED')),
  total_vnd         BIGINT NOT NULL,
  idempotency_key   TEXT   NOT NULL,
  request_hash      TEXT   NOT NULL,   -- sha256 of the body, detects key reuse with a different body
  hold_expires_at   TIMESTAMPTZ NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, idempotency_key)
);

-- At most one HELD order per user per event.
CREATE UNIQUE INDEX orders_one_held_per_user
  ON orders (event_id, user_id) WHERE status = 'HELD';

CREATE INDEX orders_held_expiry ON orders (hold_expires_at) WHERE status = 'HELD';

CREATE TABLE order_seats (
  order_id  UUID   NOT NULL REFERENCES orders(id),
  event_id  BIGINT NOT NULL,
  seat_id   TEXT   NOT NULL,
  price_vnd BIGINT NOT NULL,
  PRIMARY KEY (order_id, seat_id),
  FOREIGN KEY (event_id, seat_id) REFERENCES seats(event_id, seat_id)
);

-- Last line of defence: one ticket per seat.
CREATE TABLE tickets (
  id         UUID   PRIMARY KEY,
  order_id   UUID   NOT NULL REFERENCES orders(id),
  event_id   BIGINT NOT NULL,
  seat_id    TEXT   NOT NULL,
  qr_payload TEXT,                     -- filled in by the ticket service
  issued_at  TIMESTAMPTZ,
  UNIQUE (event_id, seat_id)
);

CREATE TABLE payments (
  provider_txn_id TEXT PRIMARY KEY,    -- deduplicates webhooks
  order_id        UUID   NOT NULL REFERENCES orders(id),
  amount_vnd      BIGINT NOT NULL,
  status          TEXT   NOT NULL CHECK (status IN ('SUCCEEDED','FAILED')),
  outcome         TEXT   NOT NULL,     -- 'APPLIED' | 'REFUND_REQUIRED' | 'IGNORED_FAILED'
  received_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE refunds (
  provider_txn_id TEXT PRIMARY KEY REFERENCES payments(provider_txn_id),  -- at most one refund per transaction
  order_id        UUID   NOT NULL REFERENCES orders(id),
  amount_vnd      BIGINT NOT NULL,
  reason          TEXT   NOT NULL,     -- 'LATE_PAYMENT_SEAT_TAKEN', 'AMOUNT_MISMATCH', ...
  status          TEXT   NOT NULL CHECK (status IN ('PENDING','SUCCEEDED')),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at    TIMESTAMPTZ
);

CREATE TABLE outbox (
  id            BIGSERIAL PRIMARY KEY,
  topic         TEXT  NOT NULL,
  msg_key       TEXT  NOT NULL,        -- order_id, keeps per-order ordering
  event_type    TEXT  NOT NULL,
  payload       JSONB NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at  TIMESTAMPTZ
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

-- Only used by INVENTORY_BACKEND=pg (the baseline).
CREATE TABLE seat_holds (
  event_id   BIGINT NOT NULL,
  seat_id    TEXT   NOT NULL,
  order_id   UUID   NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (event_id, seat_id)
);

-- +goose Down
DROP TABLE seat_holds, outbox, refunds, payments, tickets, order_seats, orders, seats, events, users;
