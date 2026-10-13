// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"slices"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
)

// TestMigration00038_DedupesLogChunksAndEnforcesUniqueness plants the damage
// redelivery used to leave (one log chunk stored several times under fresh row
// IDs) in a database rewound to 37, then migrates. The copy received first
// survives, a chunk stored once is untouched, and a further duplicate is
// refused.
func TestMigration00038_DedupesLogChunksAndEnforcesUniqueness(t *testing.T) {
	path, s, _ := openSeedable(t)
	task := submitJob(t, s, store.JobStatusRunning, store.StepStatusReady, store.TaskStatusRunning).Tasks[0]
	attempt := injectAttempt(t, s, task.ID, 1, store.AttemptStatusRunning)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 37)
	insert := func(id string, workerSeq, natsSeq int, receivedAt string) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(),
			`INSERT INTO task_logs (id, task_id, attempt_id, worker_seq, nats_seq, at, received_at, stream, data)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 'stdout', 'x')`,
			id, task.ID, attempt.ID, workerSeq, natsSeq,
			"2026-10-10T12:00:00.000000000Z", receivedAt); err != nil {
			t.Fatalf("plant %s: %v", id, err)
		}
	}
	// Inserted out of order so the survivor is chosen by received_at, not by
	// position: "mid" arrives first despite being written second.
	insert("late", 1, 12, "2026-10-10T12:00:03.000000000Z")
	insert("mid", 1, 10, "2026-10-10T12:00:01.000000000Z")
	insert("later", 1, 11, "2026-10-10T12:00:02.000000000Z")
	insert("other", 2, 13, "2026-10-10T12:00:04.000000000Z")

	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	rows := queryStrings(t, db, `SELECT id FROM task_logs WHERE attempt_id = '`+attempt.ID+`' ORDER BY id`)
	if want := []string{"mid", "other"}; !slices.Equal(rows, want) {
		t.Fatalf("surviving chunks = %v, want %v (the copy received first, and the untouched one)", rows, want)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO task_logs (id, task_id, attempt_id, worker_seq, nats_seq, at, received_at, stream, data)
		 VALUES ('again', ?, ?, 2, 99, '2026-10-10T12:00:05.000000000Z', '2026-10-10T12:00:05.000000000Z', 'stdout', 'x')`,
		task.ID, attempt.ID); err == nil {
		t.Fatal("a duplicate (attempt_id, worker_seq) was inserted after 00038")
	}
}
