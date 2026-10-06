// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for taskstatus.go.
//
// handleTaskStatusMessage and processTaskStatus are unexported methods on
// *Scheduler, so these are white-box tests in package scheduler.
// The same fakeJSMsg type from logingest_test.go is reused here.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// statusTestWorkerID is the worker every fixture attempt in this file opens
// on; statusTestSubject is the matching task.status subject these tests
// publish on. handleTaskStatusMessage now requires the subject's worker ID
// to match the attempt's recorded owner, so the two must agree. The job leaf
// of the subject is not itself checked (only the worker token is), so a
// fixed placeholder is fine across every test regardless of which job it
// actually seeds.
const statusTestWorkerID = "worker-1"

var statusTestSubject = bus.TaskStatusSubject(statusTestWorkerID, "job")

// ── helpers ───────────────────────────────────────────────────────────────────

func newStatusTestScheduler(st store.Store) *Scheduler {
	return newStatusTestSchedulerWithNotifier(st, ws.NoopNotifier{})
}

func newStatusTestSchedulerWithNotifier(st store.Store, notifier ws.Notifier) *Scheduler {
	// MaxAttempts=1 (retry disabled) so these generic terminal-cascade tests
	// keep asserting the pre-auto-retry behavior: a single genuine failure
	// goes straight to store.TaskStatusFailed. Retry/park-specific behavior is
	// covered separately in failure_test.go.
	cfg := DefaultConfig()
	cfg.DefaultMaxAttempts = 1
	return New(
		cfg,
		st,
		nil, // bus — not called by processTaskStatus
		nil, // metrics — not used (retry/park branches are disabled by MaxAttempts=1 above)
		slog.New(slog.DiscardHandler),
		notifier,
		nil, // diagBuf — diagnostics disabled
	)
}

// recordingNotifier captures NotifyTask events for assertions; other Notify*
// methods are inherited as no-ops from ws.NoopNotifier.
type recordingNotifier struct {
	ws.NoopNotifier

	tasks []ws.TaskEvent
}

func (n *recordingNotifier) NotifyTask(e ws.TaskEvent) {
	n.tasks = append(n.tasks, e)
}

// taskStatusMsgJSON marshals a TaskStatusMsg to JSON bytes for use as fakeJSMsg.data.
func taskStatusMsgJSON(t *testing.T, m protocol.TaskStatusMsg) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal TaskStatusMsg: %v", err)
	}
	return b
}

// seedStatusFixture builds a complete job/step/task in the store with the job
// already in the running state, plus the attempt a real lease writes when
// taskStatus is assigned or running (see [seedStatusJob]). Returns all four
// records; the attempt is the zero value for any other task status.
func seedStatusFixture(t *testing.T, st store.Store, taskStatus store.TaskStatus) (
	job store.Job, step store.Step, task store.Task, attempt store.TaskAttempt,
) {
	t.Helper()
	return seedStatusFixtureWithJobStatus(t, st, store.JobStatusRunning, taskStatus)
}

// seedStatusFixtureWithJobStatus is like seedStatusFixture but lets the caller
// choose the initial job status (e.g. pending, to exercise promotion to running).
func seedStatusFixtureWithJobStatus(
	t *testing.T, st store.Store, jobStatus store.JobStatus, taskStatus store.TaskStatus,
) (
	job store.Job, step store.Step, task store.Task, attempt store.TaskAttempt,
) {
	t.Helper()
	g := seedStatusJob(t, st, statusJob{jobStatus: jobStatus, steps: []statusStep{
		{name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{taskStatus}},
	}})
	task = g.tasks[0][0]
	return g.job, g.steps[0], task, g.attempts[task.ID]
}

// statusStep is one step of a job [seedStatusJob] builds. Tasks are submitted
// in the given status, except that assigned and running tasks are submitted
// ready and then leased (and, for running, started) through production writes,
// so each comes with the attempt a real lease writes.
type statusStep struct {
	name      string
	status    store.StepStatus
	dependsOn []string
	tasks     []store.TaskStatus
	// reasons, by index into tasks, is the FailureReason each task is
	// submitted with; a shorter slice leaves the rest empty.
	reasons []string
}

// statusJob configures [seedStatusJob].
type statusJob struct {
	// jobStatus is the job's status; "" is running. A paused job is submitted
	// running and paused after its tasks are leased, the state an operator
	// pausing a job with work in flight produces.
	jobStatus store.JobStatus
	// worker is the worker every in-flight task is leased to; "" is
	// statusTestWorkerID.
	worker string
	// leasedAt is when the in-flight tasks are leased; zero is now. A time
	// past the reaper's timeout makes the assignments stale.
	leasedAt time.Time
	// pool, when set, is a usage pool each lease claims one slot of, as a
	// lease of a step that requires the pool does.
	pool  *store.UsagePool
	steps []statusStep
}

// statusGraph is what [seedStatusJob] built. Tasks are as they are after the
// leases.
type statusGraph struct {
	job      store.Job
	steps    []store.Step                 // in statusJob.steps order
	tasks    [][]store.Task               // by step index, in submission order
	attempts map[string]store.TaskAttempt // by task ID, for each leased task
}

// seedStatusJob creates farm-1 and queue-1 and submits one job into them in a
// single submission, then leases its assigned and running tasks. A terminal
// job cannot hold leased work through production writes, so asking for one is
// a test bug.
func seedStatusJob(t *testing.T, st store.Store, spec statusJob) statusGraph {
	t.Helper()
	ctx := t.Context()

	// handleTaskFailed resolves retry policy via GetQueue/GetFarm, so a real
	// job needs real farm/queue rows behind its FarmID/QueueID below.
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "queue-1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	sub, inflight := statusSubmission(t, spec)
	out := storetest.Submit(t, st, sub)
	g := statusGraph{job: out.Job, attempts: map[string]store.TaskAttempt{}}
	leaseStatusTasks(t, st, spec, inflight, g.attempts)
	if spec.jobStatus == store.JobStatusPaused {
		if err := st.PauseJob(ctx, g.job.ID, time.Now().UTC()); err != nil {
			t.Fatalf("PauseJob: %v", err)
		}
	}
	g.job = mustJob(t, st, g.job.ID)
	for _, step := range sub.Steps {
		g.steps = append(g.steps, mustStep(t, st, step.ID))
	}
	for _, task := range sub.Tasks {
		i := slices.IndexFunc(g.steps, func(s store.Step) bool { return s.ID == task.StepID })
		for len(g.tasks) <= i {
			g.tasks = append(g.tasks, nil)
		}
		g.tasks[i] = append(g.tasks[i], mustTaskOf(t, st, task.ID))
	}
	return g
}

// statusSubmission builds seedStatusJob's submission and returns it with the
// tasks to lease, each mapped to whether it must also be started.
func statusSubmission(t *testing.T, spec statusJob) (store.JobSubmission, map[string]bool) {
	t.Helper()
	status := spec.jobStatus
	switch status {
	case "", store.JobStatusPaused:
		status = store.JobStatusRunning
	}
	sub := store.JobSubmission{Job: store.Job{
		ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "job",
		Status: status, TemplateFormat: store.TemplateFormatJSON,
	}}
	inflight := map[string]bool{}
	for i, sp := range spec.steps {
		step := store.Step{
			ID: uuid.NewString(), JobID: sub.Job.ID, Name: sp.name, Status: sp.status,
			StepOrder: i, DependsOn: sp.dependsOn,
		}
		sub.Steps = append(sub.Steps, step)
		for j, ts := range sp.tasks {
			task := store.Task{
				ID: uuid.NewString(), JobID: sub.Job.ID, StepID: step.ID,
				Name: fmt.Sprintf("task-%d", len(sub.Tasks)), Status: ts,
			}
			if j < len(sp.reasons) {
				task.FailureReason = sp.reasons[j]
			}
			if ts == store.TaskStatusAssigned || ts == store.TaskStatusRunning {
				task.Status = store.TaskStatusReady
				inflight[task.ID] = ts == store.TaskStatusRunning
			}
			sub.Tasks = append(sub.Tasks, task)
		}
	}
	if status.IsTerminal() && len(inflight) > 0 {
		t.Fatalf("seedStatusJob: a %s job cannot hold assigned or running tasks through production writes", status)
	}
	return sub, inflight
}

// leaseStatusTasks leases each of inflight's tasks to spec's worker (starting
// those that must run) and records the attempts in attempts.
func leaseStatusTasks(t *testing.T, st store.Store, spec statusJob, inflight map[string]bool, attempts map[string]store.TaskAttempt) {
	t.Helper()
	worker := spec.worker
	if worker == "" {
		worker = statusTestWorkerID
	}
	for id, running := range inflight {
		req := store.LeaseRequest{TaskID: id, WorkerID: worker, Now: spec.leasedAt}
		if spec.pool != nil {
			req.Claims = []store.UsagePoolClaim{{
				ClaimID: uuid.NewString(), PoolID: spec.pool.ID, PoolName: spec.pool.Name, MaxConcurrent: spec.pool.MaxConcurrent,
			}}
		}
		if running {
			attempts[id] = storetest.Running(t, st, req)
		} else {
			attempts[id] = storetest.Lease(t, st, req)
		}
	}
}

// seedStaleAssignment is seedStatusFixture's job with its task leased to
// worker an hour ago, past the reaper timeout the tests set, so the reaper
// reclaims it. pool, when non-nil, is a usage pool the lease claims a slot of.
func seedStaleAssignment(t *testing.T, st store.Store, worker string, pool *store.UsagePool) (
	job store.Job, task store.Task, attempt store.TaskAttempt,
) {
	t.Helper()
	g := seedStatusJob(t, st, statusJob{
		worker: worker, leasedAt: time.Now().UTC().Add(-time.Hour), pool: pool,
		steps: []statusStep{{name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusAssigned}}},
	})
	task = g.tasks[0][0]
	return g.job, task, g.attempts[task.ID]
}

// ── handleTaskStatusMessage — routing and discard ─────────────────────────────

func TestHandleTaskStatusMessage_MalformedJSON(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{data: []byte("{bad json")}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Error("malformed message should be acked (discarded)")
	}
}

func TestHandleTaskStatusMessage_MissingTaskID(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    "",
			AttemptID: uuid.NewString(),
			Status:    "running",
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Error("message with missing task_id should be acked (discarded)")
	}
}

func TestHandleTaskStatusMessage_UnknownAttemptID(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    uuid.NewString(),
			AttemptID: "no-such-attempt",
			Status:    "running",
		}),
	}
	// Unknown attempt → acked (discarded gracefully), no panic.
	s.handleTaskStatusMessage(msg)
	if !msg.acked {
		t.Error("message with unknown attempt_id should be acked")
	}
}

// ── processTaskStatus — "running" path ───────────────────────────────────────

func TestProcessTaskStatus_Running(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusAssigned)

	sessionID := "openjd-session-abc"
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "running",
			SessionID: sessionID,
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}
	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusRunning {
		t.Errorf("task status = %q, want running", stored.Status)
	}
	// Verify session ID was persisted on the attempt.
	updatedAttempt, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if updatedAttempt.SessionID != sessionID {
		t.Errorf("attempt.SessionID = %q, want %q", updatedAttempt.SessionID, sessionID)
	}
}

// TestProcessTaskStatus_Running_PromotesPendingJob verifies that when the first
// task of a still-pending job reports "running", the enclosing job is promoted
// to running and its StartedAt is stamped.
func TestProcessTaskStatus_Running_PromotesPendingJob(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, _, task, attempt := seedStatusFixtureWithJobStatus(
		t, st, store.JobStatusPending, store.TaskStatusAssigned,
	)

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "running",
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}
	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusRunning {
		t.Errorf("job status = %q, want running", storedJob.Status)
	}
	if storedJob.StartedAt == nil {
		t.Error("job StartedAt should be set once its first task is running")
	}
}

// TestProcessTaskStatus_Running_DoesNotUnpauseJob verifies that a "running"
// status for a job that is not pending (e.g. paused) does not flip it back to
// running.
func TestProcessTaskStatus_Running_DoesNotUnpauseJob(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, _, task, attempt := seedStatusFixtureWithJobStatus(
		t, st, store.JobStatusPaused, store.TaskStatusAssigned,
	)

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "running",
		}),
	}
	s.handleTaskStatusMessage(msg)

	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusPaused {
		t.Errorf("job status = %q, want paused (unchanged)", storedJob.Status)
	}
}

// ── processTaskStatus — terminal paths ───────────────────────────────────────

func TestProcessTaskStatus_Succeeded(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	exitCode := 0

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}
	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusSucceeded {
		t.Errorf("task status = %q, want succeeded", stored.Status)
	}
}

// TestProcessTaskStatus_Succeeded_EvictsAttemptCache proves handleTaskTerminal
// evicts the attempt-owner cache entry on a terminal status, not just the
// store row. Every other test in this file only asserts on the store, so a
// deleted evict call in handleTaskTerminal would leave them all green.
func TestProcessTaskStatus_Succeeded_EvictsAttemptCache(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	s.attemptCache.put(attempt.ID, attempt.WorkerID, task.ID)
	exitCode := 0

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if _, ok := s.attemptCache.get(attempt.ID); ok {
		t.Error("expected attempt-owner cache entry to be evicted on terminal status")
	}
}

func TestProcessTaskStatus_Failed(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	exitCode := 1

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusFailed {
		t.Errorf("task status = %q, want failed", stored.Status)
	}
}

func TestProcessTaskStatus_Canceled(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "canceled",
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
}

// TestProcessTaskStatus_Canceled_PersistsMessageAndReason asserts that a
// worker-canceled status (whose attempt is closed directly by
// handleTaskTerminal, not RecordTaskFailure) still persists the worker's
// Message onto both the closed attempt and the task's durable FailureReason.
func TestProcessTaskStatus_Canceled_PersistsMessageAndReason(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "canceled",
			Message:   "canceled by operator",
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
	if stored.FailureReason != "canceled by operator" {
		t.Errorf("task.failure_reason = %q, want %q", stored.FailureReason, "canceled by operator")
	}

	storedAttempt, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if storedAttempt.Message != "canceled by operator" {
		t.Errorf("attempt.message = %q, want %q", storedAttempt.Message, "canceled by operator")
	}
}

// TestProcessTaskStatus_Canceled_EmptyWorkerEchoPreservesServerReason pins
// that a user-canceled running task keeps its "canceled by user" reason.
// CancelTask/CancelJob set the task's failure_reason up front, then kill the
// worker; the worker always echoes back "canceled" with an empty Message
// (internal/worker/executor/run.go). handleTaskTerminal guards its
// failure-reason write (CompleteTaskAttempt's FailureReason) so an empty
// synthesized reason never overwrites an existing one.
func TestProcessTaskStatus_Canceled_EmptyWorkerEchoPreservesServerReason(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	// Simulate what CancelTask/CancelJob did before killing the worker: the
	// running task already carries the authoritative reason, submitted with it.
	g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
		name: "Step1", status: store.StepStatusRunning,
		tasks: []store.TaskStatus{store.TaskStatusRunning}, reasons: []string{"canceled by user"},
	}}})
	task := g.tasks[0][0]
	attempt := g.attempts[task.ID]

	// The killed worker's terminal echo always carries an empty Message.
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "canceled",
			Message:   "",
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
	if stored.FailureReason != "canceled by user" {
		t.Errorf("task.failure_reason = %q, want %q (must not be clobbered by the empty worker echo)",
			stored.FailureReason, "canceled by user")
	}
}

// ── Step and job completion ───────────────────────────────────────────────────

func TestProcessTaskStatus_AllTasksSucceeded_StepAndJobComplete(t *testing.T) {
	// Single-step job: when the only task succeeds, the step and job should
	// both transition to their terminal states.
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, step, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	exitCode := 0
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	// Step should be completed.
	steps, err := st.ListSteps(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	for _, st2 := range steps {
		if st2.ID == step.ID && st2.Status != store.StepStatusCompleted {
			t.Errorf("step status = %q, want completed", st2.Status)
		}
	}

	// Job should be completed.
	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusCompleted {
		t.Errorf("job status = %q, want completed", storedJob.Status)
	}
}

func TestProcessTaskStatus_TaskFailed_JobFails(t *testing.T) {
	// Single-step job: failed task → step failed → job failed.
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	exitCode := 1
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusFailed {
		t.Errorf("job status = %q, want failed", storedJob.Status)
	}
}

func TestProcessTaskStatus_SucceededStep_UnblocksDependentStep(t *testing.T) {
	// Two-step job: Step1 → Step2 (depends on Step1).
	// When Step1's task succeeds, Step2's tasks should move from pending→ready.
	st := newCheckedFake(t)
	ctx := t.Context()
	now := time.Now()

	g := seedDependentStepJob(t, st)
	task1, task2 := g.tasks[0][0], g.tasks[1][0]
	attempt1 := g.attempts[task1.ID]

	s := newStatusTestScheduler(st)
	s.ctx = ctx

	exitCode := 0
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        now,
		}),
	}
	s.handleTaskStatusMessage(msg)

	// task2 should now be ready (dependency on Step1 resolved).
	stored2, err := st.GetTask(ctx, task2.ID)
	if err != nil {
		t.Fatalf("GetTask(task2): %v", err)
	}
	if stored2.Status != store.TaskStatusReady {
		t.Errorf("task2 status = %q after Step1 completion, want ready", stored2.Status)
	}
}

// seedDependentStepJob builds a two-step job: Step1 holding one running task
// (leased, with its attempt) and a pending Step2, depending on Step1, holding
// one pending task.
func seedDependentStepJob(t *testing.T, st store.Store) statusGraph {
	t.Helper()
	return seedStatusJob(t, st, statusJob{steps: []statusStep{
		{name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusRunning}},
		{name: "Step2", status: store.StepStatusPending, dependsOn: []string{"Step1"}, tasks: []store.TaskStatus{store.TaskStatusPending}},
	}})
}

func TestProcessTaskStatus_FailedStep_CascadeCancelsDependentAndCompletesJob(t *testing.T) {
	// Two-step job: Step1 → Step2 (depends on Step1).
	// When Step1's task fails, Step2 can never run, so it must be canceled
	// (along with its pending tasks) and the job must reach a terminal state
	// rather than hanging in running forever.
	st := newCheckedFake(t)
	ctx := t.Context()

	g := seedDependentStepJob(t, st)
	job, step2 := g.job, g.steps[1]
	task1, task2 := g.tasks[0][0], g.tasks[1][0]
	attempt1 := g.attempts[task1.ID]

	s := newStatusTestScheduler(st)
	s.ctx = ctx

	exitCode := 1
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Error("successful cascade should ack the message")
	}

	// Step2 must be canceled (its only dependency failed).
	stored2Step, err := st.GetStep(ctx, step2.ID)
	if err != nil {
		t.Fatalf("GetStep(step2): %v", err)
	}
	if stored2Step.Status != store.StepStatusCanceled {
		t.Errorf("step2 status = %q, want canceled", stored2Step.Status)
	}

	// task2 (pending) must be canceled too.
	stored2, err := st.GetTask(ctx, task2.ID)
	if err != nil {
		t.Fatalf("GetTask(task2): %v", err)
	}
	if stored2.Status != store.TaskStatusCanceled {
		t.Errorf("task2 status = %q, want canceled", stored2.Status)
	}
	if stored2.FailureReason != "canceled: upstream step failed" {
		t.Errorf("task2 failure_reason = %q, want %q", stored2.FailureReason, "canceled: upstream step failed")
	}

	// The job must reach a terminal state, not hang in running.
	storedJob, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusFailed {
		t.Errorf("job status = %q, want failed", storedJob.Status)
	}
}

func TestProcessTaskStatus_CascadeCancel_NotifiesCanceledTasks(t *testing.T) {
	// A cascade-canceled dependent task must be fanned out to WebSocket clients,
	// otherwise the live UI shows it frozen as pending.
	st := newCheckedFake(t)
	ctx := t.Context()

	g := seedDependentStepJob(t, st)
	task1, task2 := g.tasks[0][0], g.tasks[1][0]
	attempt1 := g.attempts[task1.ID]

	notifier := &recordingNotifier{}
	s := newStatusTestSchedulerWithNotifier(st, notifier)
	s.ctx = ctx

	exitCode := 1
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	// Expect a canceled-task event for task2 (task1's own failed event also fires).
	var got *ws.TaskEvent
	for i := range notifier.tasks {
		if notifier.tasks[i].TaskID == task2.ID {
			got = &notifier.tasks[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no TaskEvent emitted for cascade-canceled task2; events = %+v", notifier.tasks)
	}
	if got.Status != string(store.TaskStatusCanceled) {
		t.Errorf("task2 event status = %q, want canceled", got.Status)
	}
}

func TestProcessTaskStatus_CascadeCancel_StoreError_Nacked(t *testing.T) {
	// If the cascade hits a transient store error, the message must be nacked so
	// JetStream redelivers — otherwise dependents strand and the job hangs, the
	// exact failure this cascade exists to prevent.
	inner := newCheckedFake(t)
	ctx := t.Context()

	g := seedDependentStepJob(t, inner)
	task1 := g.tasks[0][0]
	attempt1 := g.attempts[task1.ID]

	est := &cancelTasksErrSt{Store: inner}
	s := newStatusTestScheduler(est)
	s.ctx = ctx

	exitCode := 1
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.nacked {
		t.Error("store error during cascade should cause the message to be nacked")
	}
}

// cancelTasksErrSt makes the dependent-task cancellation fail mid-cascade.
type cancelTasksErrSt struct {
	store.Store
}

func (*cancelTasksErrSt) CancelPendingStep(context.Context, string, string, time.Time) (bool, []store.Task, error) {
	return false, nil, errInjectedLog
}

// ── Store error on CompleteTaskAttempt → message nacked ──────────────────────

func TestProcessTaskStatus_CompleteAttemptError_Nacked(t *testing.T) {
	inner := newCheckedFake(t)
	_, _, task, attempt := seedStatusFixture(t, inner, store.TaskStatusRunning)
	est := &completeAttemptErrSt{Store: inner}

	s := newStatusTestScheduler(est)
	s.ctx = t.Context()

	exitCode := 0
	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.nacked {
		t.Error("store error should cause message to be nacked")
	}
}

// completeAttemptErrSt makes CompleteTaskAttempt fail.
type completeAttemptErrSt struct {
	store.Store
}

func (*completeAttemptErrSt) CompleteTaskAttempt(context.Context, store.AttemptCompletion) (store.CompletionResult, error) {
	return store.CompletionResult{}, errInjectedLog
}
