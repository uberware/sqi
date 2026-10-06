// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// mustWorker reads a worker back, failing the test on any error.
func mustWorker(t *testing.T, st store.Store, id string) store.Worker {
	t.Helper()
	w, err := st.GetWorker(t.Context(), id)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", id, err)
	}
	return w
}

// sortedTaskIDs returns the IDs of tasks in ascending order, so a returned set
// can be compared with the set a test expects regardless of backend ordering.
func sortedTaskIDs(tasks []store.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestOfflineStaleWorker(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			now := time.Now().UTC()
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusOnline, now.Add(-time.Hour))
			pool := seedPool(t, st, 1)
			attempt := seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, attempt.ID)

			tasks, ok, err := st.OfflineStaleWorker(t.Context(), fixtureWorkerID, now.Add(-time.Minute), now)
			if err != nil || !ok || len(tasks) != 1 {
				t.Fatalf("OfflineStaleWorker = (%d tasks, %v, %v), want (1, true, nil)", len(tasks), ok, err)
			}
			// The returned set is the post-reset row (invariant I2).
			if got := tasks[0]; got.ID != g.Tasks["a"][0].ID || got.Status != store.TaskStatusReady || got.AssignedWorkerID != "" || got.AssignedAt != nil {
				t.Fatalf("returned task = %+v, want the reclaimed task as it is after the reset", got)
			}
			if got := mustTask(t, st, g.Tasks["a"][0].ID); got.Status != store.TaskStatusReady || got.AssignedWorkerID != "" {
				t.Fatalf("stored task = %+v, want reclaimed", got)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusOffline {
				t.Fatalf("worker = %q, want offline", w.Status)
			}
			if a := mustAttempt(t, st, attempt.ID); a.Status != store.AttemptStatusFailed || a.EndedAt == nil || a.Message != store.FailureReasonWorkerOffline {
				t.Fatalf("attempt = %+v, want failed, ended, with the worker-offline message", a)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// TestOfflineStaleWorker_FreshHeartbeatWins pins that a heartbeat landing
// after the stale-worker listing keeps the worker online and its task running.
func TestOfflineStaleWorker_FreshHeartbeatWins(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			now := time.Now().UTC()
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusOnline, now) // heartbeat just landed
			pool := seedPool(t, st, 1)
			attempt := seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, attempt.ID)

			tasks, ok, err := st.OfflineStaleWorker(t.Context(), fixtureWorkerID, now.Add(-time.Minute), now)
			if err != nil || ok || len(tasks) != 0 {
				t.Fatalf("OfflineStaleWorker on a live worker = (%d, %v, %v), want (0, false, nil)", len(tasks), ok, err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusOnline {
				t.Fatalf("worker = %q, want online", w.Status)
			}
			if mustTask(t, st, g.Tasks["a"][0].ID).Status != store.TaskStatusRunning {
				t.Fatal("live worker's task was reclaimed")
			}
			if a := mustAttempt(t, st, attempt.ID); a.Status != store.AttemptStatusRunning {
				t.Fatalf("live worker's attempt = %q, want running", a.Status)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want the live attempt's 1", n)
			}
		})
	}
}

// TestOfflineStaleWorker_OnlyAnOnlineWorker pins the other half of the guard
// for a stale worker holding a running task. An offline worker is left exactly
// as it is, task and all. A disabled one is online underneath, so it goes
// offline and is reclaimed like any other (its task returns to ready), and it
// stays disabled: the sweep writes liveness, never the flag.
func TestOfflineStaleWorker_OnlyAnOnlineWorker(t *testing.T) {
	tests := []struct {
		status    store.WorkerStatus
		wantOK    bool
		wantTasks int
		wantTask  store.TaskStatus
	}{
		{status: store.WorkerStatusDisabled, wantOK: true, wantTasks: 1, wantTask: store.TaskStatusReady},
		{status: store.WorkerStatusOffline, wantOK: false, wantTasks: 0, wantTask: store.TaskStatusRunning},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			for name, st := range newStores(t) {
				t.Run(name, func(t *testing.T) {
					g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
					now := time.Now().UTC()
					seedWorker(t, st, g.Farm.ID, tc.status, now.Add(-time.Hour))

					tasks, ok, err := st.OfflineStaleWorker(t.Context(), fixtureWorkerID, now.Add(-time.Minute), now)
					if err != nil || ok != tc.wantOK || len(tasks) != tc.wantTasks {
						t.Fatalf("OfflineStaleWorker on a %s worker = (%d, %v, %v), want (%d, %v, nil)",
							tc.status, len(tasks), ok, err, tc.wantTasks, tc.wantOK)
					}
					if w := mustWorker(t, st, fixtureWorkerID); w.EffectiveStatus() != tc.status {
						t.Fatalf("worker = %q, want %q kept", w.EffectiveStatus(), tc.status)
					}
					if got := mustTask(t, st, g.Tasks["a"][0].ID).Status; got != tc.wantTask {
						t.Fatalf("task = %q, want %q", got, tc.wantTask)
					}
				})
			}
		})
	}
}

// TestOfflineStaleWorker_NoHeartbeatIsNotStale pins an edge the two backends
// must agree on: a worker with no recorded heartbeat has no age to compare, so
// the guard `last_heartbeat_at < cutoff` is not met (SQL's NULL comparison) and
// the worker stays online. The scheduler stamps a heartbeat at registration, so
// this never occurs on a registered worker.
func TestOfflineStaleWorker_NoHeartbeatIsNotStale(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			if _, _, err := st.RegisterWorker(t.Context(), store.Worker{ID: fixtureWorkerID, FarmID: g.Farm.ID, Hostname: "node", Status: store.WorkerStatusOnline}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			now := time.Now().UTC()
			tasks, ok, err := st.OfflineStaleWorker(t.Context(), fixtureWorkerID, now, now)
			if err != nil || ok || len(tasks) != 0 {
				t.Fatalf("OfflineStaleWorker with no heartbeat = (%d, %v, %v), want (0, false, nil)", len(tasks), ok, err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusOnline {
				t.Fatalf("worker = %q, want online", w.Status)
			}
		})
	}
}

func TestOfflineStaleWorker_UnknownWorker(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			tasks, ok, err := st.OfflineStaleWorker(t.Context(), "no-such-worker", now, now)
			if err != nil || ok || len(tasks) != 0 {
				t.Fatalf("OfflineStaleWorker(unknown) = (%d, %v, %v), want (0, false, nil)", len(tasks), ok, err)
			}
		})
	}
}

// TestOfflineStaleWorker_ReclaimsOnlyItsOwnInFlightTasks seeds a second worker's
// running task, a task of this worker that is already terminal, and tasks of
// this worker in two different jobs. Only this worker's assigned and running
// tasks come back, from both jobs, and the other worker's attempt and claim
// survive.
func TestOfflineStaleWorker_ReclaimsOnlyItsOwnInFlightTasks(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			first := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusSucceeded},
			})
			second := seedGraph(t, st, graphOpts{share: &first}, stepSpec{
				name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning},
			})
			now := time.Now().UTC()
			seedWorker(t, st, first.Farm.ID, store.WorkerStatusOnline, now.Add(-time.Hour))
			if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
				ID: "w2", FarmID: first.Farm.ID, Hostname: "other", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
			}); err != nil {
				t.Fatalf("RegisterWorker w2: %v", err)
			}
			otherTask, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: first.Job.ID, StepID: first.Steps["a"].ID, Name: "other",
				Status: store.TaskStatusRunning, AssignedWorkerID: "w2", AssignedAt: &now, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			pool := seedPool(t, st, 0)
			ours := seedAttempt(t, st, first.Tasks["a"][0], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, ours.ID)
			theirs := seedAttempt(t, st, otherTask, store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, theirs.ID)

			tasks, ok, err := st.OfflineStaleWorker(t.Context(), fixtureWorkerID, now.Add(-time.Minute), now)
			if err != nil || !ok {
				t.Fatalf("OfflineStaleWorker = (%d, %v, %v), want marked offline", len(tasks), ok, err)
			}
			want := []string{first.Tasks["a"][0].ID, second.Tasks["a"][0].ID}
			slices.Sort(want)
			if got := sortedTaskIDs(tasks); !slices.Equal(got, want) {
				t.Fatalf("reclaimed %v, want exactly the worker's assigned and running tasks %v", got, want)
			}
			if got := mustTask(t, st, first.Tasks["a"][1].ID); got.Status != store.TaskStatusSucceeded {
				t.Fatalf("terminal task = %q, want untouched", got.Status)
			}
			if got := mustTask(t, st, otherTask.ID); got.Status != store.TaskStatusRunning || got.AssignedWorkerID != "w2" {
				t.Fatalf("other worker's task = %+v, want untouched", got)
			}
			if a := mustAttempt(t, st, theirs.ID); a.Status != store.AttemptStatusRunning {
				t.Fatalf("other worker's attempt = %q, want running", a.Status)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want only the other worker's 1", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

func TestOfflineWorker(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned}})
			now := time.Now().UTC()
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusOnline, now)
			pool := seedPool(t, st, 1)
			seedClaim(t, st, pool.ID, seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning).ID)

			tasks, _, err := st.OfflineWorker(t.Context(), fixtureWorkerID, "", now)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("OfflineWorker = (%d, %v), want (1, nil)", len(tasks), err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusOffline {
				t.Fatalf("worker = %q, want offline (the heartbeat is fresh; the guard does not apply)", w.Status)
			}
			if got := mustTask(t, st, g.Tasks["a"][0].ID); got.Status != store.TaskStatusReady || got.AssignedWorkerID != "" {
				t.Fatalf("task = %+v, want reclaimed", got)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
			if _, _, err := st.OfflineWorker(t.Context(), "nope", "", now); !errorsIsNotFound(err) {
				t.Fatalf("OfflineWorker(unknown) = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestOfflineWorker_AlreadyOfflineIsHarmless covers a graceful deregister that
// arrives after the heartbeat sweep already took the worker offline, or a
// redelivered one: it succeeds and reclaims nothing.
func TestOfflineWorker_AlreadyOfflineIsHarmless(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
			now := time.Now().UTC()
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusOffline, now.Add(-time.Hour))

			tasks, _, err := st.OfflineWorker(t.Context(), fixtureWorkerID, "", now)
			if err != nil || len(tasks) != 0 {
				t.Fatalf("OfflineWorker on an offline worker = (%d, %v), want (0, nil)", len(tasks), err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusOffline {
				t.Fatalf("worker = %q, want offline", w.Status)
			}
		})
	}
}

// TestAttemptClose_MessageSemantics pins how a close treats an attempt's message,
// the one place the offline reclaim differs from the cancel and the reaper: the
// offline reclaim states why it closed the attempt, so its non-empty message is
// recorded over whatever was there, while a close with nothing to say (a cancel,
// a reap) must never blank a message the attempt already carries.
func TestAttemptClose_MessageSemantics(t *testing.T) {
	const existing = "rendering frame 12"
	cases := []struct {
		name string
		// task is the status the seeded task starts in.
		task store.TaskStatus
		// close runs the operation under test against the seeded task.
		close func(t *testing.T, st store.Store, task store.Task)
		want  string
	}{
		{
			name: "offline reclaim records its reason over an existing message",
			task: store.TaskStatusRunning,
			close: func(t *testing.T, st store.Store, _ store.Task) {
				t.Helper()
				if _, _, err := st.OfflineWorker(t.Context(), fixtureWorkerID, "", time.Now().UTC()); err != nil {
					t.Fatalf("OfflineWorker: %v", err)
				}
			},
			want: store.FailureReasonWorkerOffline,
		},
		{
			name: "cancel keeps an existing message",
			task: store.TaskStatusRunning,
			close: func(t *testing.T, st store.Store, task store.Task) {
				t.Helper()
				if _, _, err := st.CancelTaskExecution(t.Context(), task.ID, store.FailureReasonCanceledByUser, time.Now().UTC()); err != nil {
					t.Fatalf("CancelTaskExecution: %v", err)
				}
			},
			want: existing,
		},
		{
			name: "reaper keeps an existing message",
			task: store.TaskStatusAssigned,
			close: func(t *testing.T, st store.Store, _ store.Task) {
				t.Helper()
				if _, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().UTC().Add(time.Minute)); err != nil {
					t.Fatalf("ReclaimStaleAssignedTasks: %v", err)
				}
			},
			want: existing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range newStores(t) {
				t.Run(name, func(t *testing.T) {
					g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{tc.task}})
					now := time.Now().UTC()
					seedWorker(t, st, g.Farm.ID, store.WorkerStatusOnline, now)
					task := g.Tasks["a"][0]
					attempt, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
						ID: uuid.NewString(), TaskID: task.ID, WorkerID: fixtureWorkerID, AttemptNumber: 1,
						Status: store.AttemptStatusRunning, StartedAt: now, CreatedAt: now, Message: existing,
					})
					if err != nil {
						t.Fatalf("CreateTaskAttempt: %v", err)
					}

					tc.close(t, st, task)

					got := mustAttempt(t, st, attempt.ID)
					if got.Status == store.AttemptStatusRunning || got.EndedAt == nil {
						t.Fatalf("attempt = %+v, want it closed", got)
					}
					if got.Message != tc.want {
						t.Fatalf("attempt message = %q, want %q", got.Message, tc.want)
					}
				})
			}
		})
	}
}
