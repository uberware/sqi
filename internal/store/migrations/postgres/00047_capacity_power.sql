-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- Power state for capacity group members, and the runtime state of a group.
--
-- workers.power_state is where a worker is in the power cycle: awake, waking,
-- retiring, asleep or wake_failed. power_state_at is when it last changed
-- (NULL until it has) and power_reason says why. interfaces is a JSON list of
-- the worker's network interfaces (name, mac, addrs), which a wake-on-LAN packet
-- is addressed with, and cloud_instance_id is the cloud provider's identifier
-- for the machine; both are reported at registration.
--
-- capacity_group_state holds what the server decides about a group at run
-- time: how many machines it has asked for, whether it is degraded, and when it
-- may ask for more. It is kept out of capacity_groups so that replacing an
-- operator's fields can never overwrite it. requested_at, degraded_at and
-- backoff_until are NULL until set.
ALTER TABLE workers ADD COLUMN power_state TEXT COLLATE "C" NOT NULL DEFAULT 'awake';
ALTER TABLE workers ADD COLUMN power_state_at ts;
ALTER TABLE workers ADD COLUMN power_reason TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN interfaces TEXT COLLATE "C" NOT NULL DEFAULT '[]';
ALTER TABLE workers ADD COLUMN cloud_instance_id TEXT COLLATE "C" NOT NULL DEFAULT '';

CREATE TABLE capacity_group_state (
    group_id        TEXT COLLATE "C" NOT NULL PRIMARY KEY REFERENCES capacity_groups (id) ON DELETE CASCADE,
    requested       BIGINT NOT NULL DEFAULT 0,
    requested_at    ts,
    degraded_reason TEXT COLLATE "C" NOT NULL DEFAULT '',
    degraded_at     ts,
    backoff_until   ts,
    updated_at      ts NOT NULL
);

-- +goose Down
DROP TABLE capacity_group_state;

ALTER TABLE workers DROP COLUMN cloud_instance_id;
ALTER TABLE workers DROP COLUMN interfaces;
ALTER TABLE workers DROP COLUMN power_reason;
ALTER TABLE workers DROP COLUMN power_state_at;
ALTER TABLE workers DROP COLUMN power_state;
