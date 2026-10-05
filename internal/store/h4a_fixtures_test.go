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
	share *h4aGraph
}

// stepSpec describes one step of a seeded job. The per-row creators accept
// any status, so a test places rows in exactly the state it needs without
// walking the state machine.
type stepSpec struct {
	name      string
	status    store.StepStatus
	dependsOn []string
	tasks     []store.TaskStatus
}

// h4aGraph is what seedGraph built.
type h4aGraph struct {
	Farm  store.Farm
	Queue store.Queue
	Job   store.Job
	Steps map[string]store.Step   // by step name
	Tasks map[string][]store.Task // by step name, in creation order
}

func seedGraph(t *testing.T, st store.Store, opts graphOpts, specs ...stepSpec) h4aGraph {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	g := h4aGraph{Steps: map[string]store.Step{}, Tasks: map[string][]store.Task{}}

	if opts.share != nil {
		g.Farm, g.Queue = opts.share.Farm, opts.share.Queue
	} else {
		var err error
		if g.Farm, err = st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrentTasks: opts.farmCap}); err != nil {
			t.Fatalf("CreateFarm: %v", err)
		}
		if g.Queue, err = st.CreateQueue(ctx, store.Queue{
			ID: uuid.NewString(), FarmID: g.Farm.ID, Name: uuid.NewString(),
			MaxConcurrentTasks: opts.queueCap, Paused: opts.queuePaused,
		}); err != nil {
			t.Fatalf("CreateQueue: %v", err)
		}
	}
	status := opts.jobStatus
	if status == "" {
		status = store.JobStatusPending
	}
	var err error
	if g.Job, err = st.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: g.Farm.ID, QueueID: g.Queue.ID, Name: "job",
		Status: status, TemplateFormat: store.TemplateFormatJSON, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for i, sp := range specs {
		step, err := st.CreateStep(ctx, store.Step{
			ID: uuid.NewString(), JobID: g.Job.ID, Name: sp.name, DependsOn: sp.dependsOn,
			StepOrder: i, Status: sp.status, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateStep %s: %v", sp.name, err)
		}
		g.Steps[sp.name] = step
		for j, ts := range sp.tasks {
			task := store.Task{
				ID: uuid.NewString(), JobID: g.Job.ID, StepID: step.ID,
				Name: sp.name + "-" + uuid.NewString()[:4], Status: ts,
				CreatedAt: now.Add(time.Duration(j) * time.Microsecond), UpdatedAt: now,
			}
			if ts == store.TaskStatusAssigned || ts == store.TaskStatusRunning {
				at := now
				task.AssignedWorkerID, task.AssignedAt = fixtureWorkerID, &at
			}
			created, err := st.CreateTask(ctx, task)
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			g.Tasks[sp.name] = append(g.Tasks[sp.name], created)
		}
	}
	return g
}

func seedAttempt(t *testing.T, st store.Store, task store.Task, status store.AttemptStatus) store.TaskAttempt {
	t.Helper()
	worker := task.AssignedWorkerID
	if worker == "" {
		worker = fixtureWorkerID
	}
	latest, err := st.ListTaskAttempts(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	now := time.Now().UTC()
	a, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
		ID: uuid.NewString(), TaskID: task.ID, WorkerID: worker, AttemptNumber: len(latest) + 1,
		Status: status, StartedAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	return a
}

func seedPool(t *testing.T, st store.Store, maxConcurrent int) store.UsagePool {
	t.Helper()
	p, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	return p
}

func seedClaim(t *testing.T, st store.Store, poolID, attemptID string) store.UsageClaim {
	t.Helper()
	c, err := st.CreateClaim(t.Context(), store.UsageClaim{ID: uuid.NewString(), PoolID: poolID, TaskAttemptID: attemptID})
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	return c
}

// seedWorker registers the fixture worker in the given status. RegisterWorker
// stores the status given, except that an existing disabled worker stays
// disabled; a fresh fixture worker has none.
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
