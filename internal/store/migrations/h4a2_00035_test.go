// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// seedAttemptsOn creates a task (taskStatus) with one attempt per given status,
// numbered 1.., each holding one active claim on poolID. It returns the
// attempt and claim IDs in order.
func seedAttemptsOn(t *testing.T, s *sqlite.Store, poolID string, taskStatus store.TaskStatus, attempts ...store.AttemptStatus) (attemptIDs, claimIDs []string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	stepID := seedStep(t, s, store.JobStatusRunning, store.StepStatusReady)
	step, err := s.GetStep(ctx, stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	task, err := s.CreateTask(ctx, store.Task{ID: uuid.NewString(), JobID: step.JobID, StepID: stepID, Name: "t", Status: taskStatus, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for i, as := range attempts {
		a, err := s.CreateTaskAttempt(ctx, store.TaskAttempt{
			ID: uuid.NewString(), TaskID: task.ID, WorkerID: "w", AttemptNumber: i + 1, Status: as, StartedAt: now, CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateTaskAttempt: %v", err)
		}
		c, err := s.CreateClaim(ctx, store.UsageClaim{ID: uuid.NewString(), PoolID: poolID, TaskAttemptID: a.ID})
		if err != nil {
			t.Fatalf("CreateClaim: %v", err)
		}
		attemptIDs, claimIDs = append(attemptIDs, a.ID), append(claimIDs, c.ID)
	}
	return attemptIDs, claimIDs
}

func TestMigration00035_ClosesOrphanAttempts(t *testing.T) {
	path, s, poolID := openSeedable(t)
	R, Fd := store.AttemptStatusRunning, store.AttemptStatusFailed
	healthyA, healthyC := seedAttemptsOn(t, s, poolID, store.TaskStatusRunning, Fd, R)      // superseded closed, latest open
	terminalA, terminalC := seedAttemptsOn(t, s, poolID, store.TaskStatusSucceeded, R)      // F4: open attempt on a finished task
	supersededA, supersededC := seedAttemptsOn(t, s, poolID, store.TaskStatusRunning, R, R) // F5: an older attempt left open
	readyA, readyC := seedAttemptsOn(t, s, poolID, store.TaskStatusReady, R)                // an open attempt on a task out of flight
	assignedA, assignedC := seedAttemptsOn(t, s, poolID, store.TaskStatusAssigned, R)       // a fresh lease: the task is assigned, the attempt open
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
		{"assigned task", assignedA[0], assignedC[0], true},
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
