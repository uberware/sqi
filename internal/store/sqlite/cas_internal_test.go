// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestCasTaskStatusTx_GuardsOnObservedStatus pins the write shape: the UPDATE
// is conditioned on the status that was read, so a concurrent change (which
// SQLite cannot produce, but Postgres will) is detected instead of overwritten.
func TestCasTaskStatusTx_GuardsOnObservedStatus(t *testing.T) {
	ctx := t.Context()
	s := openTestStoreWB(t)
	seedCASTask(t, s, "t1", store.TaskStatusAssigned)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rollbackOnCleanup(t, tx)
	ok, err := casWriteTaskStatus(ctx, tx, "t1", store.TaskStatusReady, store.TaskStatusRunning, time.Now())
	if err != nil || ok {
		t.Fatalf("CAS with a stale observed status = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = casWriteTaskStatus(ctx, tx, "t1", store.TaskStatusAssigned, store.TaskStatusRunning, time.Now())
	if err != nil || !ok {
		t.Fatalf("CAS with the current status = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestCasTaskStatusTx_Outcomes pins the three results callers branch on: the
// write happened, the task already held the status (a no-op), and the state
// machine refused the arrow (the row is untouched).
func TestCasTaskStatusTx_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		id      string // the task the CAS targets; "t1" is the seeded one
		from    store.TaskStatus
		to      store.TaskStatus
		want    casResult
		wantErr error
		wantNow store.TaskStatus // status of t1 afterwards
	}{
		{"legal arrow is applied", "t1", store.TaskStatusAssigned, store.TaskStatusRunning, casApplied, nil, store.TaskStatusRunning},
		{"current status is a no-op", "t1", store.TaskStatusRunning, store.TaskStatusRunning, casSame, nil, store.TaskStatusRunning},
		{"terminal to another status is rejected", "t1", store.TaskStatusCanceled, store.TaskStatusSucceeded, casRejected, store.ErrInvalidTransition, store.TaskStatusCanceled},
		{"unknown task is rejected as not found", "missing", store.TaskStatusAssigned, store.TaskStatusRunning, casRejected, store.ErrNotFound, store.TaskStatusAssigned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := openTestStoreWB(t)
			seedCASTask(t, s, "t1", tc.from)

			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			rollbackOnCleanup(t, tx)
			got, err := casTaskStatusTx(ctx, tx, tc.id, tc.to, time.Now())
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("casTaskStatusTx = (%v, %v), want (%v, %v)", got, err, tc.want, tc.wantErr)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			task, err := s.GetTask(ctx, "t1")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.Status != tc.wantNow {
				t.Fatalf("task status = %s, want %s", task.Status, tc.wantNow)
			}
		})
	}
}

// rollbackOnCleanup rolls tx back when the test ends. A transaction the test
// already committed reports sql.ErrTxDone, which is expected; anything else is
// a real failure.
func rollbackOnCleanup(t *testing.T, tx *sql.Tx) {
	t.Helper()
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback: %v", err)
		}
	})
}

// seedCASTask seeds a running job with one step and the task id in the given
// status. An assigned or running task is submitted ready and then leased (and
// started, for running) to worker "w", so it carries the attempt a real lease
// makes; any other status is written as given.
func seedCASTask(t *testing.T, s *Store, id string, status store.TaskStatus) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.CreateFarm(ctx, store.Farm{ID: "f", Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateQueue(ctx, store.Queue{ID: "q", FarmID: "f", Name: "q"}); err != nil {
		t.Fatal(err)
	}
	created := status
	if status == store.TaskStatusAssigned || status == store.TaskStatusRunning {
		created = store.TaskStatusReady
	}
	storetest.Submit(t, s, store.JobSubmission{
		Job: store.Job{
			ID: "j", FarmID: "f", QueueID: "q", Name: "j",
			Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		},
		Steps: []store.Step{{ID: "s", JobID: "j", Name: "s", Status: store.StepStatusReady}},
		Tasks: []store.Task{{ID: id, JobID: "j", StepID: "s", Name: "t", Status: created}},
	})
	req := store.LeaseRequest{TaskID: id, WorkerID: "w"}
	switch status {
	case store.TaskStatusAssigned:
		storetest.Lease(t, s, req)
	case store.TaskStatusRunning:
		storetest.Running(t, s, req)
	}
}
