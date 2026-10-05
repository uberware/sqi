// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// seedStep creates a job (jobStatus) with one step (stepStatus) holding tasks
// in the given statuses, using the raw creators: the point is a combination
// the guarded operations no longer produce.
func seedStep(t *testing.T, s *sqlite.Store, jobStatus store.JobStatus, stepStatus store.StepStatus, tasks ...store.TaskStatus) string {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	farm, err := s.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := s.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	job, err := s.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: farm.ID, QueueID: queue.ID, Name: "j",
		Status: jobStatus, TemplateFormat: store.TemplateFormatJSON, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := s.CreateStep(ctx, store.Step{ID: uuid.NewString(), JobID: job.ID, Name: "s", Status: stepStatus, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	for _, ts := range tasks {
		if _, err := s.CreateTask(ctx, store.Task{
			ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t", Status: ts, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	return step.ID
}

func TestMigration00033_FinalizesOpenStepsOfTerminalJobs(t *testing.T) {
	path, s, _ := openSeedable(t)
	C, S, F, P := store.TaskStatusCanceled, store.TaskStatusSucceeded, store.TaskStatusFailed, store.TaskStatusPending
	cases := []struct {
		name string
		id   string
		want store.StepStatus
	}{
		{"canceled job, ready step, mixed", seedStep(t, s, store.JobStatusCanceled, store.StepStatusReady, S, C), store.StepStatusCanceled},
		{"canceled job, ready step, a failure", seedStep(t, s, store.JobStatusCanceled, store.StepStatusReady, F, C), store.StepStatusFailed},
		{"canceled job, pending step", seedStep(t, s, store.JobStatusCanceled, store.StepStatusPending, C), store.StepStatusCanceled},
		{"canceled job, pending step with a failure", seedStep(t, s, store.JobStatusCanceled, store.StepStatusPending, F, C), store.StepStatusCanceled},
		{"failed job, ready step, all succeeded", seedStep(t, s, store.JobStatusFailed, store.StepStatusReady, S), store.StepStatusCompleted},
		// A step that never got a task is canceled, not "completed": with no
		// tasks the failed/canceled probes both miss and the ELSE would fire.
		{"canceled job, ready step, no tasks", seedStep(t, s, store.JobStatusCanceled, store.StepStatusReady), store.StepStatusCanceled},
		// Live jobs belong to the start-up reconcile, not to the migration.
		{"running job, stuck ready step", seedStep(t, s, store.JobStatusRunning, store.StepStatusReady, S), store.StepStatusReady},
		// A step with a task still in flight is never finalized.
		{"canceled job, task pending", seedStep(t, s, store.JobStatusCanceled, store.StepStatusReady, P), store.StepStatusReady},
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db := rewindTo(t, path, 32)
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	for _, c := range cases {
		var got string
		if err := db.QueryRowContext(t.Context(), `SELECT status FROM steps WHERE id = ?`, c.id).Scan(&got); err != nil {
			t.Fatalf("%s: read step: %v", c.name, err)
		}
		if store.StepStatus(got) != c.want {
			t.Errorf("%s: step = %q, want %q", c.name, got, c.want)
		}
	}
}
