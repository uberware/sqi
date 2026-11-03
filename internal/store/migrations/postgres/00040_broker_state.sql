-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- The single row recording which broker this database last ran against:
-- "embedded:<account>" or "external:<account>@<cluster>". The server compares
-- it with the broker it is configured for at boot, so a database that moves
-- between brokers is noticed rather than resuming against streams and
-- consumers that are not the ones it wrote.
--
-- id is pinned to 1 so the table can never hold a second row.
CREATE TABLE broker_state (
    id          BIGINT NOT NULL PRIMARY KEY CHECK (id = 1),
    identity    TEXT COLLATE "C" NOT NULL,
    recorded_at ts NOT NULL
);

-- +goose Down
DROP TABLE broker_state;
