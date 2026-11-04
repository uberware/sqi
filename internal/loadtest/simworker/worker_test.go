// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker_test

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/driver"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// farm is an in-process server with one farm and queue.
type farm struct {
	srv     *testserver.Server
	c       *client.Client
	farmID  string
	queueID string
}

func newFarm(t *testing.T) *farm {
	t.Helper()
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	farmID, err := c.CreateFarm(t.Context(), "sim-farm")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queueID, err := c.CreateQueue(t.Context(), farmID, "sim-queue", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return &farm{srv: srv, c: c, farmID: farmID, queueID: queueID}
}

// startWorker starts a queue-less simulated worker and stops it at cleanup.
func (f *farm) startWorker(t *testing.T, n int, b simworker.Behavior, l *simworker.Ledger) *simworker.Worker {
	t.Helper()
	spec := simworker.WorkerSpec{
		ID:       uuid.NewString(),
		Hostname: fmt.Sprintf("sim-test-%d", n),
		FarmID:   f.farmID,
		Group: scenario.WorkerGroup{
			Name: "sim", CPU: 4, RAMMb: 8192, OS: "linux",
			Heartbeat: scenario.Duration{Duration: 15 * time.Second},
		},
		NATSURL:  f.srv.NATSURL(),
		RandSeed: uint64(n),
	}
	w := simworker.NewWorker(spec, b, l, slog.New(slog.DiscardHandler))
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start worker %d: %v", n, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.Stop(ctx); err != nil {
			t.Errorf("Stop worker %d: %v", n, err)
		}
	})
	return w
}

// submit submits one job of steps × tasks and returns its ID.
func (f *farm) submit(t *testing.T, steps, tasks int) string {
	t.Helper()
	jc := scenario.JobClass{Name: "sim", Steps: steps, TasksPerStep: tasks}
	tmpl, err := driver.BuildTemplate(jc, "sim-1", scenario.Command{Path: "noop"})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	id, _, err := f.c.SubmitJob(t.Context(), client.SubmitRequest{FarmID: f.farmID, QueueID: f.queueID, Template: tmpl})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	return id
}

func (f *farm) waitJob(t *testing.T, jobID, want string, timeout time.Duration) {
	t.Helper()
	var last string
	deadline := time.Now().Add(timeout)
	for {
		j, err := f.c.GetJob(t.Context(), jobID)
		if err == nil {
			last = j.Status
			if last == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s status %q after %s, want %q", jobID, last, timeout, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *farm) onlyTask(t *testing.T, jobID string) client.Task {
	t.Helper()
	page, err := f.c.ListTasks(t.Context(), jobID, 10, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("job has %d tasks, want 1", len(page.Items))
	}
	return page.Items[0]
}

// dumpOnFailure logs the ledger and the server's view of the job's tasks when
// the test fails.
func (f *farm) dumpOnFailure(t *testing.T, jobID string, l *simworker.Ledger) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, e := range l.Events() {
			t.Logf("ledger: %s %s task=%s attempt=%s instance=%s outcome=%q", e.At.Format("15:04:05.000"), e.Kind, e.TaskID, e.AttemptID, e.InstanceID, e.Outcome)
		}
		ctx := context.Background()
		page, err := f.c.ListTasks(ctx, jobID, 100, 0)
		if err != nil {
			t.Logf("ListTasks: %v", err)
			return
		}
		for _, task := range page.Items {
			t.Logf("server task %s: %s (worker %s)", task.ID, task.Status, task.AssignedWorkerID)
			attempts, err := f.c.ListAttempts(ctx, task.ID)
			if err != nil {
				t.Logf("ListAttempts: %v", err)
				continue
			}
			for _, a := range attempts {
				t.Logf("  attempt %d: %s worker %s", a.AttemptNumber, a.Status, a.WorkerID)
			}
		}
	})
}

func fixedPlan(p simworker.TaskPlan) simworker.Behavior {
	return simworker.BehaviorFunc(func(*protocol.AssignMsg, *rand.Rand) simworker.TaskPlan { return p })
}

func TestWorker_RegistersAndCompletesTasks(t *testing.T) {
	f := newFarm(t)
	l := simworker.NewLedger()
	b := fixedPlan(simworker.TaskPlan{Duration: 50 * time.Millisecond, Outcome: simworker.OutcomeSucceeded, LogLines: 3, LineBytes: 32})
	for n := range 3 {
		f.startWorker(t, n, b, l)
	}

	// The stream acks a registration before the server applies it.
	var hosts []string
	waitFor(t, 10*time.Second, "three online workers", func() bool {
		page, err := f.c.ListWorkers(t.Context(), f.farmID, 10, 0)
		if err != nil {
			return false
		}
		hosts = hosts[:0]
		for _, w := range page.Items {
			if w.Status == "online" {
				hosts = append(hosts, w.Hostname)
			}
		}
		return len(hosts) == 3
	})
	slices.Sort(hosts)
	if !slices.Equal(hosts, []string{"sim-test-0", "sim-test-1", "sim-test-2"}) {
		t.Errorf("registered hostnames = %v", hosts)
	}

	jobID := f.submit(t, 2, 5)
	f.waitJob(t, jobID, "completed", 30*time.Second)

	terminals := 0
	tasks := map[string]bool{}
	for _, e := range l.Events() {
		if e.Kind == simworker.KindTerminal {
			terminals++
			tasks[e.TaskID] = true
			if e.Outcome != simworker.OutcomeSucceeded || e.LogLines != 3 || e.InstanceID == "" {
				t.Errorf("terminal event %+v", e)
			}
		}
	}
	if terminals != 10 || len(tasks) != 10 {
		t.Errorf("ledger has %d terminal events over %d tasks, want 10 over 10", terminals, len(tasks))
	}
}

func TestWorker_RestartReclaims(t *testing.T) {
	f := newFarm(t)
	l := simworker.NewLedger()
	// The first attempt of each task hangs; any later one succeeds.
	var mu sync.Mutex
	seen := map[string]bool{}
	b := simworker.BehaviorFunc(func(m *protocol.AssignMsg, _ *rand.Rand) simworker.TaskPlan {
		mu.Lock()
		defer mu.Unlock()
		if !seen[m.TaskID] {
			seen[m.TaskID] = true
			return simworker.TaskPlan{Duration: 10 * time.Millisecond, Outcome: simworker.OutcomeHang}
		}
		return simworker.TaskPlan{Duration: 10 * time.Millisecond, Outcome: simworker.OutcomeSucceeded}
	})
	w := f.startWorker(t, 0, b, l)

	jobID := f.submit(t, 1, 1)
	f.dumpOnFailure(t, jobID, l)
	waitFor(t, 30*time.Second, "first attempt running on the server", func() bool {
		task := f.onlyTask(t, jobID)
		return task.Status == "running"
	})
	// A real restart takes seconds, by which time the old process's next lease
	// request is parked on the server, and a parked request from a superseded
	// instance is refused. A request still in the handler's front path when
	// the new registration lands is not re-checked against it and can be
	// served the reclaimed task into a dead inbox (left to the 2 min attempt
	// reconciliation), so give that request time to park first.
	time.Sleep(250 * time.Millisecond)
	first := l.Events()[0]

	oldInstance := w.InstanceID()
	if err := w.Restart(t.Context()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if w.InstanceID() == oldInstance || w.InstanceID() == "" {
		t.Errorf("InstanceID after Restart = %q, old %q; want a new one", w.InstanceID(), oldInstance)
	}
	f.waitJob(t, jobID, "completed", 30*time.Second)

	var leased []string
	crashed := false
	for _, e := range l.Events() {
		if e.TaskID != first.TaskID {
			continue
		}
		switch e.Kind {
		case simworker.KindLeased:
			leased = append(leased, e.AttemptID)
		case simworker.KindCrashed:
			crashed = e.AttemptID == first.AttemptID && e.InstanceID == oldInstance
		}
	}
	if len(leased) != 2 || leased[0] == leased[1] {
		t.Errorf("leased attempts = %v, want two different ones", leased)
	}
	if !crashed {
		t.Error("no crashed event for the abandoned first attempt")
	}
	attempts, err := f.c.ListAttempts(t.Context(), first.TaskID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Errorf("server records %d attempts, want 2", len(attempts))
	}
}

func TestWorker_ReconnectKeepsInstance(t *testing.T) {
	f := newFarm(t)
	l := simworker.NewLedger()
	b := fixedPlan(simworker.TaskPlan{Duration: 2 * time.Second, Outcome: simworker.OutcomeSucceeded, LogLines: 5, LineBytes: 20})
	w := f.startWorker(t, 0, b, l)

	jobID := f.submit(t, 1, 1)
	waitFor(t, 30*time.Second, "task running", func() bool { return countKind(l, "", simworker.KindRunning) == 1 })

	before := w.InstanceID()
	if err := w.Disconnect(t.Context()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if w.InstanceID() != before {
		t.Errorf("InstanceID changed across Disconnect: %q -> %q", before, w.InstanceID())
	}
	f.waitJob(t, jobID, "completed", 30*time.Second)

	task := f.onlyTask(t, jobID)
	if task.Status != "succeeded" {
		t.Errorf("task status %q, want succeeded", task.Status)
	}
	if n := countKind(l, task.ID, simworker.KindLeased); n != 1 {
		t.Errorf("task leased %d times, want once (no reclaim)", n)
	}
	attempts, err := f.c.ListAttempts(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Errorf("server records %d attempts, want 1", len(attempts))
	}
}

func TestWorker_LifecycleErrors(t *testing.T) {
	f := newFarm(t)
	l := simworker.NewLedger()
	w := f.startWorker(t, 0, fixedPlan(simworker.TaskPlan{Duration: time.Millisecond}), l)
	if err := w.Start(t.Context()); err == nil {
		t.Error("second Start succeeded, want an error")
	}
	w.Crash()
	if err := w.Disconnect(t.Context()); err == nil {
		t.Error("Disconnect of a crashed worker succeeded, want an error")
	}
	if err := w.Restart(t.Context()); err != nil {
		t.Fatalf("Restart after Crash: %v", err)
	}
}

func TestWorker_StopAndCrashWithATaskInFlight(t *testing.T) {
	tests := []struct {
		name       string
		end        func(t *testing.T, w *simworker.Worker)
		wantKind   string
		wantResult string // ledger outcome of the end event
		wantWorker string // the server's view of the worker afterwards
	}{
		{
			name: "stop gives up after its grace",
			end: func(t *testing.T, w *simworker.Worker) {
				ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
				defer cancel()
				if err := w.Stop(ctx); err != nil {
					t.Errorf("Stop: %v", err)
				}
			},
			wantKind: simworker.KindTerminal, wantResult: protocol.MessageWorkerShutdown, wantWorker: "offline",
		},
		{
			name:     "crash publishes nothing",
			end:      func(_ *testing.T, w *simworker.Worker) { w.Crash() },
			wantKind: simworker.KindCrashed, wantWorker: "online",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFarm(t)
			l := simworker.NewLedger()
			// A zero duration emits the one line right after running, then hangs.
			w := f.startWorker(t, 0, fixedPlan(simworker.TaskPlan{Outcome: simworker.OutcomeHang, LogLines: 1}), l)
			jobID := f.submit(t, 1, 1)
			waitFor(t, 30*time.Second, "task running on the server", func() bool {
				page, err := f.c.ListTasks(t.Context(), jobID, 10, 0)
				return err == nil && len(page.Items) == 1 && page.Items[0].Status == "running"
			})

			tt.end(t, w)

			evs := l.Events()
			last := evs[len(evs)-1]
			if last.Kind != tt.wantKind || last.Outcome != tt.wantResult || last.LogLines != 1 {
				t.Errorf("last ledger event = %+v, want kind %q outcome %q with 1 line", last, tt.wantKind, tt.wantResult)
			}
			task := f.onlyTask(t, jobID)
			waitFor(t, 10*time.Second, "worker status "+tt.wantWorker, func() bool {
				page, err := f.c.ListWorkers(t.Context(), f.farmID, 10, 0)
				return err == nil && len(page.Items) == 1 && page.Items[0].Status == tt.wantWorker
			})
			if tt.wantKind == simworker.KindCrashed && task.Status != "running" {
				t.Errorf("task status after a crash = %q, want running (nothing told the server)", task.Status)
			}
		})
	}
}
