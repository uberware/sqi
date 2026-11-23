// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// TestMigration00049_LeavesStartedAtUnknown rewinds past workers.started_at
// and migrates up: a worker that existed before the column has no recorded
// process start, so the column is NULL rather than a guess, until the
// worker's next heartbeat fills it.
func TestMigration00049_LeavesStartedAtUnknown(t *testing.T) {
	path, s, _ := openSeedable(t)
	now := time.Now().UTC()
	if _, _, err := s.RegisterWorker(t.Context(), store.Worker{
		ID: "w-pre", Hostname: "node", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now, InstanceID: "i1",
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 48)
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	var startedAt sql.NullString
	if err := db.QueryRowContext(t.Context(), `SELECT started_at FROM workers WHERE id = 'w-pre'`).Scan(&startedAt); err != nil {
		t.Fatalf("read started_at: %v", err)
	}
	if startedAt.Valid {
		t.Errorf("started_at = %q, want NULL", startedAt.String)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// The worker's next heartbeat fills the unknown start from its uptime,
	// without a restart.
	s, err := sqlite.Open(t.Context(), path, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	start := stamp.Add(-90 * time.Second)
	if _, err := s.UpdateWorkerHeartbeat(t.Context(), "w-pre", stamp, &stamp, &start); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}
	w, err := s.GetWorker(t.Context(), "w-pre")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.StartedAt == nil || !w.StartedAt.Equal(start) {
		t.Errorf("after a heartbeat: StartedAt = %v, want %v", w.StartedAt, start)
	}
}
