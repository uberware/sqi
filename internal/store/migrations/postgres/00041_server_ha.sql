-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- Schema for running more than one sqi-server against one database.
--
-- server_lease is the leadership lease: holder is the instance leading (NULL
-- when released), epoch the epoch it leads at, and expires_us the instant the
-- lease lapses, in microseconds since the Unix epoch on the DATABASE clock.
-- server_epoch is the fencing token a leader's writes check; it is a separate
-- row so a lease renewal never contends with the writes that check it. Both
-- are pinned to one row (id = 1), seeded with no holder at epoch 0.
--
-- expires_us, last_seen_us and broker_ok_us are plain integers rather than
-- timestamps: they are compared and refreshed against the database clock
-- inside SQL, never read into a Go time, so they stay out of the ts domain.
--
-- server_members is one row per server instance that has run against this
-- database: who it is, what it runs, when it last checked in, and when it last
-- saw the message broker (NULL until it has).
--
-- workers.registration_broker_at is the message broker's stream timestamp of the
-- registration that wrote the row, so a registration the broker stamped earlier
-- than the stored one (a late redelivery) can be recognised and ignored. NULL
-- until a registration carrying one is recorded.
CREATE TABLE server_lease (
    id         BIGINT NOT NULL PRIMARY KEY CHECK (id = 1),
    holder     TEXT COLLATE "C",
    epoch      BIGINT NOT NULL,
    expires_us BIGINT NOT NULL
);

INSERT INTO server_lease (id, holder, epoch, expires_us) VALUES (1, NULL, 0, 0);

CREATE TABLE server_epoch (
    id    BIGINT NOT NULL PRIMARY KEY CHECK (id = 1),
    epoch BIGINT NOT NULL
);

INSERT INTO server_epoch (id, epoch) VALUES (1, 0);

CREATE TABLE server_members (
    instance_id        TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    name               TEXT COLLATE "C" NOT NULL,
    host               TEXT COLLATE "C" NOT NULL,
    pid                BIGINT NOT NULL,
    version            TEXT COLLATE "C" NOT NULL,
    grants_fingerprint TEXT COLLATE "C" NOT NULL,
    started_at         ts NOT NULL,
    last_seen_us       BIGINT NOT NULL,
    broker_ok_us       BIGINT
);

ALTER TABLE workers ADD COLUMN registration_broker_at ts;

-- +goose Down
ALTER TABLE workers DROP COLUMN registration_broker_at;
DROP TABLE server_members;
DROP TABLE server_epoch;
DROP TABLE server_lease;
