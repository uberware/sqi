// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

// seedAttemptsOn creates a task (taskStatus) with one attempt per given status,
// numbered 1.., each holding one active claim on poolID. It returns the
// attempt and claim IDs in order. The task is submitted in taskStatus and every
// attempt and claim is injected, because each caller plants a state production
// cannot reach: an open attempt on a task that is out of flight or finished, a
// second open attempt on one task, or an active claim on a closed attempt. A
// task is submitted running only because its injected attempts follow at once;
// a state a lease can reach is seedRetriedAttempts' or seedAssignedAttempt's.
func seedAttemptsOn(t *testing.T, s *sqlite.Store, poolID string, taskStatus store.TaskStatus, attempts ...store.AttemptStatus) (attemptIDs, claimIDs []string) {
	t.Helper()
	task := submitJob(t, s, store.JobStatusRunning, store.StepStatusReady, taskStatus).Tasks[0]
	for i, as := range attempts {
		a := injectAttempt(t, s, task.ID, i+1, as)
		attemptIDs, claimIDs = append(attemptIDs, a.ID), append(claimIDs, injectClaim(t, s, poolID, a.ID))
	}
	return attemptIDs, claimIDs
}

// seedRetriedAttempts creates a task on its second attempt through production
// writes: the first attempt is leased, started and reclaimed (closed as failed,
// its claim released, the task back to ready), then the task is leased again
// and started. It returns both attempt and claim IDs in order. This is the
// healthy twin of the superseded-attempt orphan: an older closed attempt behind
// a running latest one.
func seedRetriedAttempts(t *testing.T, s *sqlite.Store, poolID string) (attemptIDs, claimIDs []string) {
	t.Helper()
	task := submitJob(t, s, store.JobStatusRunning, store.StepStatusReady, store.TaskStatusReady).Tasks[0]
	first, firstClaim := leaseWithClaim(t, s, task.ID, poolID)
	storetest.Start(t, s, first, "", first.StartedAt)
	reclaimed, err := s.ReclaimTaskAttempt(t.Context(), first.ID, task.ID, time.Now().UTC())
	if err != nil || !reclaimed {
		t.Fatalf("ReclaimTaskAttempt = %v, %v; want it reclaimed", reclaimed, err)
	}
	second, secondClaim := leaseWithClaim(t, s, task.ID, poolID)
	if second.AttemptNumber != first.AttemptNumber+1 {
		t.Fatalf("second attempt is number %d after %d, want the next number", second.AttemptNumber, first.AttemptNumber)
	}
	storetest.Start(t, s, second, "", second.StartedAt)
	return []string{first.ID, second.ID}, []string{firstClaim, secondClaim}
}

// seedAssignedAttempt creates a task freshly leased through production writes:
// the task is assigned, its first attempt is open, and the lease holds one
// active claim on poolID. It returns the attempt and claim IDs.
func seedAssignedAttempt(t *testing.T, s *sqlite.Store, poolID string) (attemptID, claimID string) {
	t.Helper()
	task := submitJob(t, s, store.JobStatusRunning, store.StepStatusReady, store.TaskStatusReady).Tasks[0]
	attempt, claimID := leaseWithClaim(t, s, task.ID, poolID)
	return attempt.ID, claimID
}

func TestMigration00035_ClosesOrphanAttempts(t *testing.T) {
	path, s, poolID := openSeedable(t)
	R := store.AttemptStatusRunning
	healthyA, healthyC := seedRetriedAttempts(t, s, poolID)                                 // superseded closed, latest open
	terminalA, terminalC := seedAttemptsOn(t, s, poolID, store.TaskStatusSucceeded, R)      // an open attempt on a finished task
	supersededA, supersededC := seedAttemptsOn(t, s, poolID, store.TaskStatusRunning, R, R) // an older attempt left open
	readyA, readyC := seedAttemptsOn(t, s, poolID, store.TaskStatusReady, R)                // an open attempt on a task out of flight
	assignedA, assignedC := seedAssignedAttempt(t, s, poolID)                               // a fresh lease: the task is assigned, the attempt open
	keptA, keptC := seedAttemptsOn(t, s, poolID, store.TaskStatusFailed, R)                 // an orphan an earlier path already gave a reason
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db := rewindTo(t, path, 34)
	const keptReason = "worker lost"
	if _, err := db.ExecContext(t.Context(), `UPDATE task_attempts SET message = ? WHERE id = ?`, keptReason, keptA[0]); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	ranFrom := time.Now().UTC().Add(-time.Second)
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	ranTo := time.Now().UTC().Add(time.Second)
	attemptRow := func(id string) (status, message string, endedAt sql.NullString) {
		if err := db.QueryRowContext(t.Context(), `SELECT status, message, ended_at FROM task_attempts WHERE id = ?`, id).Scan(&status, &message, &endedAt); err != nil {
			t.Fatalf("read attempt %s: %v", id, err)
		}
		return status, message, endedAt
	}
	for _, c := range []struct {
		name           string
		attempt, claim string
		wantOpen       bool
	}{
		{"healthy latest", healthyA[1], healthyC[1], true},
		{"terminal task", terminalA[0], terminalC[0], false},
		{"superseded older", supersededA[0], supersededC[0], false},
		{"superseded latest", supersededA[1], supersededC[1], true},
		{"ready task", readyA[0], readyC[0], false},
		{"assigned task", assignedA, assignedC, true},
		{"orphan with a reason", keptA[0], keptC[0], false},
	} {
		status, message, endedAt := attemptRow(c.attempt)
		open := status == string(store.AttemptStatusRunning)
		released := claimReleasedAt(t, db, c.claim).Valid
		if open != c.wantOpen || released == c.wantOpen {
			t.Errorf("%s: attempt open=%v claim released=%v, want open=%v released=%v", c.name, open, released, c.wantOpen, !c.wantOpen)
		}
		if c.wantOpen {
			if endedAt.Valid || message != "" {
				t.Errorf("%s: an attempt left open was touched: ended_at=%v message=%q", c.name, endedAt, message)
			}
			continue
		}
		// A closed orphan reads back as a failed attempt stamped with the
		// migration's own clock in the layout the store parses, with a reason.
		at, err := time.Parse(time.RFC3339Nano, endedAt.String)
		if !endedAt.Valid || err != nil || at.Before(ranFrom) || at.After(ranTo) {
			t.Errorf("%s: ended_at = %v (parse err %v), want the migration time between %s and %s", c.name, endedAt, err, ranFrom, ranTo)
		}
		if status != string(store.AttemptStatusFailed) || message == "" {
			t.Errorf("%s: closed as status=%q message=%q, want failed with a reason", c.name, status, message)
		}
	}
	// The repair names an orphan it closes, but never overwrites a reason one
	// already carries.
	if _, message, _ := attemptRow(keptA[0]); message != keptReason {
		t.Errorf("orphan with a reason: message = %q, want %q kept", message, keptReason)
	}
}
