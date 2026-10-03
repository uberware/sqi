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
)

// seedDepsStep creates one step and, when taskStatus is non-empty, one task in it.
func seedDepsStep(t *testing.T, s store.Store, id, name string, order int, status store.StepStatus, deps []string, taskStatus store.TaskStatus) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.CreateStep(ctx, store.Step{
		ID: id, JobID: "j1", Name: name, StepOrder: order, Status: status, DependsOn: deps,
	}); err != nil {
		t.Fatalf("CreateStep %s: %v", id, err)
	}
	if taskStatus == "" {
		return
	}
	if _, err := s.CreateTask(ctx, store.Task{ID: "t-" + id, JobID: "j1", StepID: id, Status: taskStatus}); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
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
	if _, err := inner.CreateJob(t.Context(), store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	// The job was canceled after ResolveDependencies listed the steps.
	seedDepsStep(t, inner, "step1", "Step1", 0, store.StepStatusCanceled, nil, store.TaskStatusCanceled)
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
	if _, err := inner.CreateJob(t.Context(), store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	seedDepsStep(t, inner, "step1", "Step1", 0, store.StepStatusFailed, nil, "")
	// step2 was listed as pending but has since been released and completed.
	seedDepsStep(t, inner, "step2", "Step2", 1, store.StepStatusCompleted, []string{"Step1"}, store.TaskStatusSucceeded)
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
	if _, err := inner.CreateJob(t.Context(), store.Job{ID: "j1", Name: "j1", Status: store.JobStatusRunning}); err != nil {
		t.Fatal(err)
	}
	// step3 (order 0) depends on step2 (order 1), which depends on failed step1.
	seedDepsStep(t, inner, "step1", "Step1", 2, store.StepStatusFailed, nil, "")
	seedDepsStep(t, inner, "step3", "Step3", 0, store.StepStatusPending, []string{"Step2"}, store.TaskStatusPending)
	// step2 is already canceled in the store (another writer got there first)
	// but is listed as pending.
	seedDepsStep(t, inner, "step2", "Step2", 1, store.StepStatusCanceled, []string{"Step1"}, store.TaskStatusCanceled)
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
