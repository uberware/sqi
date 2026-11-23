-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- When the worker's current process started, which the web UI's uptime
-- counts from. registered_at is kept across re-registrations, so it dates the
-- worker's first registration, not the process running now. A registration
-- sets started_at when it creates the row or carries a new instance ID, and
-- heartbeats refine it from the uptime the worker reports. No worker's
-- process start is known when the migration runs, so it is left NULL for the
-- worker's next heartbeat to fill.
ALTER TABLE workers ADD COLUMN started_at ts;

-- +goose Down
ALTER TABLE workers DROP COLUMN started_at;
