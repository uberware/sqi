// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

// fixtureWorkerID is the worker every in-flight fixture task is assigned to.
const fixtureWorkerID = "w1"

// graphOpts configures seedGraph. The zero value is a pending job in an
// uncapped queue and farm.
type graphOpts struct {
	jobStatus   store.JobStatus
	queueCap    int
	farmCap     int
	queuePaused bool
	// share, when non-nil, reuses that graph's farm and queue so two jobs can
	// compete for the same caps.
	share *jobGraph
	// dependsOn are the job's upstream job IDs, written as the submission's
	// dependency edges.
	dependsOn []string
}

// stepSpec describes one step of a seeded job. Tasks are created in the given
// status, except that assigned and running tasks are created ready and then
// leased (and, for running, started) through production writes, so each comes
// with the attempt a real lease makes.
type stepSpec struct {
	name      string
	status    store.StepStatus
	dependsOn []string
	tasks     []store.TaskStatus
	// reasons, by index into tasks, is the FailureReason each task is
	// submitted with; a shorter slice leaves the rest empty.
	reasons []string
}

// jobGraph is what seedGraph built.
type jobGraph struct {
	Farm     store.Farm
	Queue    store.Queue
	Job      store.Job
	Steps    map[string]store.Step        // by step name
	Tasks    map[string][]store.Task      // by step name, in creation order
	Attempts map[string]store.TaskAttempt // by task ID, for each task seedGraph leased
}

func seedGraph(t *testing.T, st store.Store, opts graphOpts, specs ...stepSpec) jobGraph {
	t.Helper()
	now := time.Now().UTC()
	g := jobGraph{Steps: map[string]store.Step{}, Tasks: map[string][]store.Task{}}
	seedScope(t, st, &g, opts)

	out, attempts := storetest.SubmitLeasing(t, st, graphSubmission(g, opts, specs), func(store.Task) store.LeaseRequest {
		return store.LeaseRequest{WorkerID: fixtureWorkerID, Now: now}
	})
	g.Job, g.Attempts = out.Job, attempts
	stepNames := make(map[string]string, len(out.Steps)) // step ID -> name
	for _, step := range out.Steps {
		g.Steps[step.Name] = step
		stepNames[step.ID] = step.Name
	}
	for _, task := range out.Tasks {
		g.Tasks[stepNames[task.StepID]] = append(g.Tasks[stepNames[task.StepID]], task)
	}
	applyCapsAndPause(t, st, &g, opts, now)
	return g
}

// seedScope creates the graph's farm and queue uncapped and unpaused (caps and
// the pause are applied after the in-flight tasks are leased), or reuses the
// shared graph's.
func seedScope(t *testing.T, st store.Store, g *jobGraph, opts graphOpts) {
	t.Helper()
	if opts.share != nil {
		g.Farm, g.Queue = opts.share.Farm, opts.share.Queue
		return
	}
	var err error
	if g.Farm, err = st.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: uuid.NewString()}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if g.Queue, err = st.CreateQueue(t.Context(), store.Queue{ID: uuid.NewString(), FarmID: g.Farm.ID, Name: uuid.NewString()}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
}

// graphSubmission builds the job's submission. A job that will be paused is
// submitted pending (PauseJob moves it after the leases).
func graphSubmission(g jobGraph, opts graphOpts, specs []stepSpec) store.JobSubmission {
	status := opts.jobStatus
	if status == "" || status == store.JobStatusPaused {
		status = store.JobStatusPending
	}
	sub := store.JobSubmission{Job: store.Job{
		ID: uuid.NewString(), FarmID: g.Farm.ID, QueueID: g.Queue.ID, Name: "job",
		Status: status, TemplateFormat: store.TemplateFormatJSON,
	}, DependsOn: opts.dependsOn}
	for i, sp := range specs {
		step := store.Step{ID: uuid.NewString(), JobID: sub.Job.ID, Name: sp.name, DependsOn: sp.dependsOn, StepOrder: i, Status: sp.status}
		sub.Steps = append(sub.Steps, step)
		for j, ts := range sp.tasks {
			task := store.Task{ID: uuid.NewString(), JobID: sub.Job.ID, StepID: step.ID, Name: sp.name + "-" + uuid.NewString()[:4], Status: ts}
			if j < len(sp.reasons) {
				task.FailureReason = sp.reasons[j]
			}
			sub.Tasks = append(sub.Tasks, task)
		}
	}
	return sub
}

// applyCapsAndPause applies opts' caps and pauses after the leases, the state an
// operator lowering a cap or pausing a queue or job produces.
func applyCapsAndPause(t *testing.T, st store.Store, g *jobGraph, opts graphOpts, now time.Time) {
	t.Helper()
	ctx := t.Context()
	var err error
	if opts.share == nil && opts.farmCap > 0 {
		g.Farm.MaxConcurrentTasks = opts.farmCap
		if g.Farm, err = st.UpdateFarm(ctx, g.Farm); err != nil {
			t.Fatalf("UpdateFarm: %v", err)
		}
	}
	if opts.share == nil && (opts.queueCap > 0 || opts.queuePaused) {
		g.Queue.MaxConcurrentTasks, g.Queue.Paused = opts.queueCap, opts.queuePaused
		if g.Queue, err = st.UpdateQueue(ctx, g.Queue); err != nil {
			t.Fatalf("UpdateQueue: %v", err)
		}
	}
	if opts.jobStatus == store.JobStatusPaused {
		if err := st.PauseJob(ctx, g.Job.ID, now); err != nil {
			t.Fatalf("PauseJob: %v", err)
		}
	}
	if g.Job, err = st.GetJob(ctx, g.Job.ID); err != nil {
		t.Fatalf("GetJob: %v", err)
	}
}

// seedAttempt injects an attempt on task in the given status. Only for states
// production cannot reach (a second open attempt, an open attempt on a terminal
// task, a closed attempt that never ran): an assigned or running task from
// seedGraph already has its leased attempt in jobGraph.Attempts.
func seedAttempt(t *testing.T, st store.Store, task store.Task, status store.AttemptStatus) store.TaskAttempt {
	t.Helper()
	worker := task.AssignedWorkerID
	if worker == "" {
		worker = fixtureWorkerID
	}
	existing, err := st.ListTaskAttempts(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	return storetest.InjectAttempt(t, st, store.TaskAttempt{
		TaskID: task.ID, WorkerID: worker, AttemptNumber: len(existing) + 1, Status: status,
	})
}

// leaseClaiming leases taskID, which must be ready, to the fixture worker
// holding one slot in each of pools, and starts it when running is set: claimed
// in-flight work, leased the way the scheduler leases it.
func leaseClaiming(t *testing.T, st store.Store, taskID string, running bool, pools ...store.UsagePool) store.TaskAttempt {
	t.Helper()
	req := store.LeaseRequest{TaskID: taskID, WorkerID: fixtureWorkerID}
	for _, p := range pools {
		req.Claims = append(req.Claims, store.UsagePoolClaim{ClaimID: uuid.NewString(), PoolID: p.ID, PoolName: p.Name})
	}
	if running {
		return storetest.Running(t, st, req)
	}
	return storetest.Lease(t, st, req)
}

// seedClaim injects an active claim on attemptID. Only for states production
// cannot reach (a claim on a closed attempt); a claim held by in-flight work
// comes from LeaseRequest.Claims.
func seedClaim(t *testing.T, st store.Store, poolID, attemptID string) store.UsageClaim {
	t.Helper()
	return storetest.InjectClaim(t, st, store.UsageClaim{PoolID: poolID, TaskAttemptID: attemptID})
}

func seedPool(t *testing.T, st store.Store, maxConcurrent int) store.UsagePool {
	t.Helper()
	p, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	return p
}

// seedWorker registers the fixture worker with the given effective status. A
// disabled status is an online worker an operator then disabled: disabled is a
// flag over liveness, not a liveness of its own.
func seedWorker(t *testing.T, st store.Store, farmID string, status store.WorkerStatus, lastHeartbeat time.Time) store.Worker {
	t.Helper()
	liveness := status
	if status == store.WorkerStatusDisabled {
		liveness = store.WorkerStatusOnline
	}
	w, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: fixtureWorkerID, FarmID: farmID, Hostname: "node", Status: liveness, LastHeartbeatAt: &lastHeartbeat,
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if status == store.WorkerStatusDisabled {
		if w, err = st.SetWorkerDisabled(t.Context(), w.ID, true); err != nil {
			t.Fatalf("SetWorkerDisabled: %v", err)
		}
	}
	return w
}

// claimViolations runs the backend's I3 diagnostic.
func claimViolations(t *testing.T, st store.Store) []string {
	t.Helper()
	switch s := st.(type) {
	case *fake.Store:
		return s.ClaimInvariantViolations()
	case *sqlite.Store:
		v, err := s.ClaimInvariantViolations(t.Context())
		if err != nil {
			t.Fatalf("ClaimInvariantViolations: %v", err)
		}
		return v
	default:
		t.Fatalf("claimViolations: unsupported store %T", st)
		return nil
	}
}

func mustTask(t *testing.T, st store.Store, id string) store.Task {
	t.Helper()
	v, err := st.GetTask(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTask %s: %v", id, err)
	}
	return v
}

func mustStep(t *testing.T, st store.Store, id string) store.Step {
	t.Helper()
	v, err := st.GetStep(t.Context(), id)
	if err != nil {
		t.Fatalf("GetStep %s: %v", id, err)
	}
	return v
}

func mustJob(t *testing.T, st store.Store, id string) store.Job {
	t.Helper()
	v, err := st.GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob %s: %v", id, err)
	}
	return v
}

func errorsIsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

func TestSeedGraph_PausedQueueAndJobKeepLeasedTasks(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusPaused, queuePaused: true},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusRunning}})
			if !g.Queue.Paused || g.Job.Status != store.JobStatusPaused {
				t.Fatalf("queue paused=%v job=%s, want paused/paused", g.Queue.Paused, g.Job.Status)
			}
			for i, want := range []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusRunning} {
				task := g.Tasks["a"][i]
				if task.Status != want {
					t.Fatalf("task %d status = %s, want %s", i, task.Status, want)
				}
				a, ok := g.Attempts[task.ID]
				if !ok || a.Status != store.AttemptStatusRunning || a.AttemptNumber != 1 {
					t.Fatalf("task %d attempt = %+v (found %v), want a running attempt #1", i, a, ok)
				}
			}
		})
	}
}

func TestSeedGraph_InFlightBeyondCaps(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{queueCap: 1, farmCap: 1},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusRunning}})
			if g.Queue.MaxConcurrentTasks != 1 || g.Farm.MaxConcurrentTasks != 1 {
				t.Fatalf("caps = queue %d farm %d, want 1/1", g.Queue.MaxConcurrentTasks, g.Farm.MaxConcurrentTasks)
			}
			if n, err := st.CountActiveTasksInQueue(t.Context(), g.Queue.ID); err != nil || n != 2 {
				t.Fatalf("CountActiveTasksInQueue = %d, %v; want 2 (over the cap)", n, err)
			}
		})
	}
}
