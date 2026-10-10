// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for deps.go's use of the guarded store operations ReleaseStep and
// CancelPendingStep (invariant I1): a step another writer already moved is
// skipped, never overwritten, and never retried forever.

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// depsJobID is the one job the deps-resolution tests build.
const depsJobID = "j1"

// submitSteps submits a running job "j1" holding steps and tasks in a single
// CreateJobSubmission, the way a real submission writes them, and returns what
// the store wrote. JobID is stamped on every step and task, so callers leave
// it out. A job's whole graph goes in one call: a second submission of the
// same job ID is a duplicate-ID error, so a step cannot be added to a job
// that already exists.
func submitSteps(t *testing.T, s store.Store, steps []store.Step, tasks ...store.Task) store.JobSubmission {
	t.Helper()
	for i := range steps {
		steps[i].JobID = depsJobID
	}
	for i := range tasks {
		tasks[i].JobID = depsJobID
	}
	return storetest.Submit(t, s, store.JobSubmission{
		Job:   store.Job{ID: depsJobID, Name: depsJobID, Status: store.JobStatusRunning},
		Steps: steps,
		Tasks: tasks,
	})
}

// depsSeed is one step of a job seedDepsJob builds and, when taskStatus is
// non-empty, the one task in it.
type depsSeed struct {
	id, name   string
	order      int
	status     store.StepStatus
	deps       []string
	taskStatus store.TaskStatus
}

// seedDepsJob submits job "j1" with one step per seed, and one task ("t-"+id)
// in each step whose seed names a taskStatus, in a single submission.
func seedDepsJob(t *testing.T, s store.Store, seeds ...depsSeed) {
	t.Helper()
	var (
		steps []store.Step
		tasks []store.Task
	)
	for _, sd := range seeds {
		steps = append(steps, store.Step{
			ID: sd.id, Name: sd.name, StepOrder: sd.order, Status: sd.status, DependsOn: sd.deps,
		})
		if sd.taskStatus != "" {
			tasks = append(tasks, store.Task{ID: "t-" + sd.id, StepID: sd.id, Status: sd.taskStatus})
		}
	}
	submitSteps(t, s, steps, tasks...)
}

func depsStepStatus(t *testing.T, s store.Store, id string) store.StepStatus {
	t.Helper()
	step, err := s.GetStep(t.Context(), id)
	if err != nil {
		t.Fatalf("GetStep %s: %v", id, err)
	}
	return step.Status
}

func depsTaskStatus(t *testing.T, s store.Store, id string) store.TaskStatus {
	t.Helper()
	task, err := s.GetTask(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTask %s: %v", id, err)
	}
	return task.Status
}

// staleListStore reports the listed steps as pending although another writer
// has already moved them, which is what a concurrent writer between
// ListSteps and the guarded write looks like.
type staleListStore struct {
	store.Store

	stalePending map[string]bool // step IDs to report as pending
}

func (s *staleListStore) ListSteps(ctx context.Context, jobID string) ([]store.Step, error) {
	steps, err := s.Store.ListSteps(ctx, jobID)
	for i := range steps {
		if s.stalePending[steps[i].ID] {
			steps[i].Status = store.StepStatusPending
		}
	}
	return steps, err
}

func TestResolveDependencies_StepMovedByAnotherWriterIsNotRevived(t *testing.T) {
	inner := fake.New()
	defer inner.Close()
	// The job was canceled after ResolveDependencies listed the steps.
	seedDepsJob(t, inner, depsSeed{"step1", "Step1", 0, store.StepStatusCanceled, nil, store.TaskStatusCanceled})
	st := &staleListStore{Store: inner, stalePending: map[string]bool{"step1": true}}

	n, err := openjd.ResolveDependencies(t.Context(), st, "j1")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n != 0 {
		t.Errorf("promoted = %d, want 0 (the guarded release declined)", n)
	}
	if got := depsStepStatus(t, inner, "step1"); got != store.StepStatusCanceled {
		t.Errorf("step status = %q, want canceled (not revived)", got)
	}
	if got := depsTaskStatus(t, inner, "t-step1"); got != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled (not revived)", got)
	}
}

func TestCancelDependents_StepMovedByAnotherWriterIsNotOverwritten(t *testing.T) {
	inner := fake.New()
	defer inner.Close()
	seedDepsJob(
		t, inner,
		depsSeed{"step1", "Step1", 0, store.StepStatusFailed, nil, ""},
		// step2 was listed as pending but has since been released and completed.
		depsSeed{"step2", "Step2", 1, store.StepStatusCompleted, []string{"Step1"}, store.TaskStatusSucceeded},
	)
	st := &staleListStore{Store: inner, stalePending: map[string]bool{"step2": true}}

	done := make(chan struct{})
	var (
		n     int
		tasks []store.Task
		err   error
	)
	go func() {
		defer close(done)
		n, tasks, err = openjd.CancelDependents(t.Context(), st, "j1")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CancelDependents did not return: a declined guarded cancel must not be retried forever")
	}
	if err != nil {
		t.Fatalf("CancelDependents: %v", err)
	}
	if n != 0 || len(tasks) != 0 {
		t.Errorf("canceled = %d steps, %d tasks; want 0, 0 (the guarded cancel declined)", n, len(tasks))
	}
	if got := depsStepStatus(t, inner, "step2"); got != store.StepStatusCompleted {
		t.Errorf("step2 status = %q, want completed (not overwritten)", got)
	}
	if got := depsTaskStatus(t, inner, "t-step2"); got != store.TaskStatusSucceeded {
		t.Errorf("t-step2 status = %q, want succeeded (not overwritten)", got)
	}
}

// A declined cancel still settles the step in the decision map, so a dependent
// that is visited BEFORE it in step order is picked up by the next pass.
func TestCancelDependents_DeclinedCancelStillUnblocksDependents(t *testing.T) {
	inner := fake.New()
	defer inner.Close()
	// step3 (order 0) depends on step2 (order 1), which depends on failed step1.
	seedDepsJob(
		t, inner,
		depsSeed{"step1", "Step1", 2, store.StepStatusFailed, nil, ""},
		depsSeed{"step3", "Step3", 0, store.StepStatusPending, []string{"Step2"}, store.TaskStatusPending},
		// step2 is already canceled in the store (another writer got there first)
		// but is listed as pending.
		depsSeed{"step2", "Step2", 1, store.StepStatusCanceled, []string{"Step1"}, store.TaskStatusCanceled},
	)
	st := &staleListStore{Store: inner, stalePending: map[string]bool{"step2": true}}

	n, tasks, err := openjd.CancelDependents(t.Context(), st, "j1")
	if err != nil {
		t.Fatalf("CancelDependents: %v", err)
	}
	if n != 1 || len(tasks) != 1 {
		t.Errorf("canceled = %d steps, %d tasks; want 1, 1 (only step3 was ours to cancel)", n, len(tasks))
	}
	if got := depsStepStatus(t, inner, "step3"); got != store.StepStatusCanceled {
		t.Errorf("step3 status = %q, want canceled", got)
	}
	if got := depsTaskStatus(t, inner, "t-step3"); got != store.TaskStatusCanceled {
		t.Errorf("t-step3 status = %q, want canceled", got)
	}
}
