-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Worker CPU architecture, as runtime.GOARCH spells it ("amd64", "arm64").
--
-- Backs OpenJD's reserved attr.worker.cpu.arch host requirement. That attribute
-- was already accepted and validated by internal/openjd (its enum is
-- {x86_64, arm64}), but the scheduler had nothing to compare it against: the
-- worker never reported its architecture, and internal/scheduler's matcher had
-- no case for the name, so it resolved to the empty string and could never
-- match on any platform. A template gating on it validated, was accepted, and
-- then waited for a worker that could not exist.
--
-- Stored as GOARCH, not as the specification's token. The two vocabularies
-- differ ("amd64" vs "x86_64"), and every other worker-reported field in this
-- table is the raw value the host gave us -- os is "darwin" on a Mac even
-- though attr.worker.os.family says "macos". The translation lives in one place
-- on the read side (scheduler.cpuArch, beside scheduler.osFamily), so what is
-- shown in the API and the UI stays the value an operator would recognize from
-- `go env` or `uname -m`.
--
-- NOT NULL DEFAULT '' matches the `version` column, the other late-added
-- worker string: scanWorker reads it into a plain string, so a NULL would be a
-- scan error rather than an empty value. Every pre-existing row gets '', and so
-- does any worker still running a binary that does not send the field. An empty
-- arch matches NO attr.worker.cpu.arch requirement, which is the safe
-- direction -- treating unknown as universally acceptable would dispatch
-- x86_64 work to an arm64 host. Such a worker starts reporting the moment it
-- restarts and re-registers.
--
-- The Down migration's ALTER TABLE ... DROP COLUMN requires SQLite >= 3.35.0
-- (the same note 00002, 00008, 00013 and 00026 carry). Deliberately no CHECK
-- constraint and no index: SQLite refuses DROP COLUMN on a column either
-- references, which would make the Down impossible without a full table rebuild.

-- +goose Up
ALTER TABLE workers ADD COLUMN arch TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE workers DROP COLUMN arch;
