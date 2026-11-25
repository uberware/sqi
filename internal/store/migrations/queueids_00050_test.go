// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
)

// TestMigration00050_CarriesQueueIDIntoList rewinds past workers.queue_ids and
// migrates up again. Rewinding puts a one-queue worker's queue back into
// queue_id and leaves a worker with no queue NULL; migrating up turns them
// into ["q1"] and [], and dropping the foreign-key column in place leaves no
// foreign key violated.
func TestMigration00050_CarriesQueueIDIntoList(t *testing.T) {
	path, s, _ := openSeedable(t)
	ctx := t.Context()
	if _, err := s.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := s.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now().UTC()
	for _, w := range []store.Worker{
		{ID: "w-one", FarmID: "f1", QueueIDs: []string{"q1"}},
		{ID: "w-any", FarmID: "f1"},
	} {
		w.Hostname, w.Status, w.LastHeartbeatAt = "node", store.WorkerStatusOnline, &now
		if _, _, err := s.RegisterWorker(ctx, w); err != nil {
			t.Fatalf("RegisterWorker %s: %v", w.ID, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 49)
	readQueueID := func(id string) sql.NullString {
		t.Helper()
		var q sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT queue_id FROM workers WHERE id = ?`, id).Scan(&q); err != nil {
			t.Fatalf("read queue_id of %s: %v", id, err)
		}
		return q
	}
	if q := readQueueID("w-one"); !q.Valid || q.String != "q1" {
		t.Errorf("at 49, w-one queue_id = %v, want q1", q)
	}
	if q := readQueueID("w-any"); q.Valid {
		t.Errorf("at 49, w-any queue_id = %q, want NULL", q.String)
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	for id, want := range map[string]string{"w-one": `["q1"]`, "w-any": `[]`} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT queue_ids FROM workers WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read queue_ids of %s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s queue_ids = %q, want %q", id, got, want)
		}
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("foreign_key_check reports a violation after migrating up")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
}
