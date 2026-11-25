// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestMigration00050_CarriesQueueIDIntoList is the PostgreSQL leg of the
// migrations package's test of the same name: rewinding puts a one-queue
// worker's queue back into queue_id and leaves a worker with no queue NULL,
// and migrating up turns them into ["q1"] and [].
func TestMigration00050_CarriesQueueIDIntoList(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	st, err := postgres.Open(t.Context(), dsn, postgres.DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now().UTC()
	for _, w := range []store.Worker{
		{ID: "w-one", FarmID: "f1", QueueIDs: []string{"q1"}},
		{ID: "w-any", FarmID: "f1"},
	} {
		w.Hostname, w.Status, w.LastHeartbeatAt = "node", store.WorkerStatusOnline, &now
		if _, _, err := st.RegisterWorker(ctx, w); err != nil {
			t.Fatalf("RegisterWorker %s: %v", w.ID, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openRaw(t, dsn)
	p, err := migrations.NewProvider(db, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 49); err != nil {
		t.Fatalf("DownTo(49): %v", err)
	}
	for id, want := range map[string]sql.NullString{
		"w-one": {String: "q1", Valid: true},
		"w-any": {},
	} {
		var got sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT queue_id FROM workers WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read queue_id of %s: %v", id, err)
		}
		if got != want {
			t.Errorf("at 49, %s queue_id = %v, want %v", id, got, want)
		}
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for id, want := range map[string]string{"w-one": `["q1"]`, "w-any": `[]`} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT queue_ids FROM workers WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read queue_ids of %s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s queue_ids = %q, want %q", id, got, want)
		}
	}
}
