// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/ws"
)

// undeliverablePrefix begins the failure reason of a task whose assignment
// the server could not build.
const undeliverablePrefix = "server could not build the task's assignment: "

// newUndeliverableScheduler is newMetricsScheduler with a chosen retry limit.
func newUndeliverableScheduler(st store.Store, maxAttempts int) *Scheduler {
	cfg := DefaultConfig()
	cfg.FarmID = "f1"
	cfg.DefaultMaxAttempts = maxAttempts
	s := New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	s.ctx = context.Background()
	ingestCaughtUp(s)
	return s
}

// submitBrokenJob submits, into queue q1, a running job whose one ready step is
// named "missing", a step its template does not have, so building any of its
// assignments fails. Its priority puts it ahead of every other job. It returns
// the job and task IDs.
func submitBrokenJob(t *testing.T, st store.Store) (jobID, taskID string) {
	t.Helper()
	one := 1
	sub := store.JobSubmission{Job: store.Job{
		ID: uuid.NewString(), FarmID: "f1", QueueID: "q1", Name: "broken", Priority: 10,
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		RawTemplate: minimalRenderJSON,
	}}
	sub.Steps = []store.Step{{ID: uuid.NewString(), JobID: sub.Job.ID, Name: "missing", Status: store.StepStatusReady}}
	sub.Tasks = []store.Task{{
		ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID, Name: "t",
		Status: store.TaskStatusReady, Parameters: map[string]string{}, RequiredCores: &one,
	}}
	storetest.Submit(t, st, sub)
	return sub.Job.ID, sub.Tasks[0].ID
}

func TestClassifyAssignFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want assignFailure
	}{
		{"built", nil, assignBuilt},
		{"store read", fmt.Errorf("build: %w: %w", errAssignStoreRead, errors.New("conn reset")), assignStoreFailure},
		{"context canceled", fmt.Errorf("build: %w", context.Canceled), assignStoreFailure},
		{"deadline exceeded", context.DeadlineExceeded, assignStoreFailure},
		{"no root for the worker", fmt.Errorf("resolve: %w", &openjd.NoRootError{Location: "p", ComputeLocation: "b"}), assignSkipWorker},
		{"step missing from template", errors.New(`step "missing" not found in template`), assignFailsTask},
		{"storage location missing", errors.New(`storage location "p" not found`), assignFailsTask},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAssignFailure(tc.err); got != tc.want {
				t.Errorf("classifyAssignFailure(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestHandleLeaseRequest_UndeliverableTaskIsFailedAndBatchContinues(t *testing.T) {
	st := newCheckedFake(t)
	s := newUndeliverableScheduler(st, 1)
	s.leaseHoldTimeout = 50 * time.Millisecond
	one := 1
	w, healthy := seedLeaseFixture(t, st, []*int{&one})
	brokenJob, broken := submitBrokenJob(t, st)

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var got leaseReply
	if err := json.Unmarshal(s.handleLeaseRequest(w.ID, "q1", req), &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if len(got.Assignments) != 1 || !strings.Contains(string(got.Assignments[0]), healthy[0]) {
		t.Fatalf("assignments = %d, want exactly the healthy task %s", len(got.Assignments), healthy[0])
	}

	task := mustTask(t, st, broken)
	wantReason := undeliverablePrefix + `step "missing" not found in template for job ` + brokenJob
	if task.Status != store.TaskStatusFailed || task.FailureReason != wantReason {
		t.Errorf("broken task = %q (reason %q), want failed with reason %q", task.Status, task.FailureReason, wantReason)
	}
	attempts, err := st.ListTaskAttempts(t.Context(), broken)
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != store.AttemptStatusFailed {
		t.Errorf("broken task attempts = %+v, want one failed attempt", attempts)
	}
	if steps, err := st.ListSteps(t.Context(), brokenJob); err != nil || len(steps) != 1 || steps[0].Status != store.StepStatusFailed {
		t.Errorf("broken step = %+v (err %v), want failed", steps, err)
	}
	if job := mustJob(t, st, brokenJob); job.Status != store.JobStatusFailed {
		t.Errorf("broken job = %q, want failed", job.Status)
	}
	if cores, err := st.CommittedCores(t.Context(), w.ID, w.CPUCount); err != nil || cores != 1 {
		t.Errorf("CommittedCores = %d (err %v), want 1 (the healthy task only)", cores, err)
	}
}

func TestSelectLeaseBatch_UndeliverableTaskWaitsOutRetryDelay(t *testing.T) {
	st := newCheckedFake(t)
	s := newUndeliverableScheduler(st, 3)
	w, _ := seedLeaseFixture(t, st, nil)
	_, broken := submitBrokenJob(t, st)

	for i := range 2 {
		if batch, err := s.selectLeaseBatch(t.Context(), w); err != nil || len(batch) != 0 {
			t.Fatalf("selectLeaseBatch #%d = %d assignments, err %v; want none and no error", i+1, len(batch), err)
		}
	}

	task := mustTask(t, st, broken)
	if task.Status != store.TaskStatusReady || task.RetryAfter == nil || !task.RetryAfter.After(time.Now()) {
		t.Errorf("broken task = %q, retry after %v; want ready and backing off", task.Status, task.RetryAfter)
	}
	if task.FailedAttempts != 1 {
		t.Errorf("FailedAttempts = %d, want 1", task.FailedAttempts)
	}
	if attempts, err := st.ListTaskAttempts(t.Context(), broken); err != nil || len(attempts) != 1 {
		t.Errorf("attempts = %d (err %v), want 1: the second lease must not retry it before its delay", len(attempts), err)
	}
}

// storageListErrStore fails ListStorageLocations while failing is set.
type storageListErrStore struct {
	store.Store

	failing bool
}

var errInjectedStorageList = errors.New("injected storage list failure")

func (s *storageListErrStore) ListStorageLocations(ctx context.Context) ([]store.StorageLocation, error) {
	if s.failing {
		return nil, errInjectedStorageList
	}
	return s.Store.ListStorageLocations(ctx)
}

func TestSelectLeaseBatch_StorageListFailureLeasesNothing(t *testing.T) {
	inner := newCheckedFake(t)
	one := 1
	w, ids := seedLeaseFixture(t, inner, []*int{&one})
	st := &storageListErrStore{Store: inner, failing: true}
	s := newUndeliverableScheduler(st, 1)

	if _, err := s.selectLeaseBatch(t.Context(), w); !errors.Is(err, errInjectedStorageList) {
		t.Fatalf("selectLeaseBatch err = %v, want it to wrap the storage list failure", err)
	}
	task := mustTask(t, inner, ids[0])
	if task.Status != store.TaskStatusReady || task.FailedAttempts != 0 || task.FailureReason != "" {
		t.Errorf("task = %q, failed attempts %d, reason %q; want ready, 0, none", task.Status, task.FailedAttempts, task.FailureReason)
	}
	if attempts, err := inner.ListTaskAttempts(t.Context(), ids[0]); err != nil || len(attempts) != 0 {
		t.Errorf("attempts = %+v (err %v), want none: a store read failure must commit no lease", attempts, err)
	}

	st.failing = false
	if batch, err := s.selectLeaseBatch(t.Context(), w); err != nil || len(batch) != 1 {
		t.Errorf("re-lease after the fault cleared = %d assignments, err %v; want 1", len(batch), err)
	}
}

// TestSelectLeaseBatch_NoRootForWorkerSkipsOnlyThatWorker pins that a task
// whose loc:// storage location has no root for one worker's compute location
// is left for a worker that can resolve it: the first worker is leased
// nothing and charged nothing, and a worker in a location with a root takes it.
func TestSelectLeaseBatch_NoRootForWorkerSkipsOnlyThatWorker(t *testing.T) {
	st := newCheckedFake(t)
	s := newUndeliverableScheduler(st, 1)
	one := 1
	seedLeaseFixture(t, st, nil)
	if _, err := st.CreateStorageLocation(t.Context(), store.StorageLocation{
		ID: uuid.NewString(), Name: "projects", Type: store.StorageLocationTypeFilesystem,
		Roots: map[string]string{"studioA": "/mnt/projects"},
	}); err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}
	sub := store.JobSubmission{Job: store.Job{
		ID: uuid.NewString(), FarmID: "f1", QueueID: "q1", Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON, RawTemplate: minimalRenderJSON,
	}}
	sub.Steps = []store.Step{{ID: uuid.NewString(), JobID: sub.Job.ID, Name: "render", Status: store.StepStatusReady}}
	sub.Tasks = []store.Task{{
		ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID, Name: "t", Status: store.TaskStatusReady,
		Parameters: map[string]string{"Scene": "loc://projects/shot.ma"}, RequiredCores: &one,
	}}
	storetest.Submit(t, st, sub)
	ids := []string{sub.Tasks[0].ID}
	now := time.Now().UTC()
	register := func(id, location string) store.Worker {
		w, _, err := st.RegisterWorker(t.Context(), store.Worker{
			ID: id, FarmID: "f1", Hostname: id, Status: store.WorkerStatusOnline, CPUCount: 4,
			LastHeartbeatAt: &now, Tags: map[string]string{}, ComputeLocation: location,
		})
		if err != nil {
			t.Fatalf("RegisterWorker %s: %v", id, err)
		}
		return w
	}
	studioB, studioA := register("wB", "studioB"), register("wA", "studioA")

	if batch, err := s.selectLeaseBatch(t.Context(), studioB); err != nil || len(batch) != 0 {
		t.Fatalf("studioB lease = %d assignments, err %v; want none and no error", len(batch), err)
	}
	got := mustTask(t, st, ids[0])
	if got.Status != store.TaskStatusReady || got.FailedAttempts != 0 || got.RetryAfter != nil {
		t.Errorf("after studioB: task = %q, failed attempts %d, retry after %v; want ready, 0, none",
			got.Status, got.FailedAttempts, got.RetryAfter)
	}
	if attempts, err := st.ListTaskAttempts(t.Context(), ids[0]); err != nil || len(attempts) != 0 {
		t.Errorf("after studioB: attempts = %d (err %v), want none", len(attempts), err)
	}

	batch, err := s.selectLeaseBatch(t.Context(), studioA)
	if err != nil || len(batch) != 1 || !strings.Contains(string(batch[0]), "/mnt/projects") {
		t.Fatalf("studioA lease = %d assignments, err %v; want the task, resolved to /mnt/projects", len(batch), err)
	}
}
