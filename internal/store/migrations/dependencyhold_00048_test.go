// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
)

// TestMigration00048_BackfillsDependencyHold rewinds past jobs.dependency_hold
// and migrates up: every job blocked at that moment still holds its
// dependencies, and every other job does not.
func TestMigration00048_BackfillsDependencyHold(t *testing.T) {
	path, s, _ := openSeedable(t)
	// The backfill keys on status alone, so neither job needs a dependency edge.
	pending := submitJob(t, s, store.JobStatusPending, store.StepStatusPending).Job.ID
	blocked := submitJob(t, s, store.JobStatusBlocked, store.StepStatusPending).Job.ID
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 47)
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	for id, want := range map[string]bool{pending: false, blocked: true} {
		var got bool
		if err := db.QueryRowContext(t.Context(), `SELECT dependency_hold FROM jobs WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != want {
			t.Errorf("job %s dependency_hold = %v, want %v", id, got, want)
		}
	}
}
