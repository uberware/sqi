// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the bus-liveness grace: a worker's heartbeat travels over the bus,
// so the sweeps must not judge workers by heartbeats the bus could not have
// delivered.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// busUpAt returns a bus-up source reporting the bus as up since d ago, or as
// down (the zero time) when d is negative.
func busUpAt(d time.Duration) func() time.Time {
	return func() time.Time {
		if d < 0 {
			return time.Time{}
		}
		return time.Now().UTC().Add(-d)
	}
}

// testBusGrace is the BusReconnectGrace the tests run with: longer than
// WorkerTimeout (30 s), so a test can tell "up for a worker timeout" from "up
// for the configured grace".
const testBusGrace = 90 * time.Second

// newLivenessScheduler is a metrics scheduler with the bus-up source wired and
// its log captured.
func newLivenessScheduler(t *testing.T, st store.Store, busUp func() time.Time) (*Scheduler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	cfg := DefaultConfig()
	cfg.FarmID = "farm-1"
	cfg.BusUpSince = busUp
	cfg.BusReconnectGrace = testBusGrace
	s := New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.NewTextHandler(&logs, nil)), ws.NoopNotifier{}, nil)
	s.ctx = t.Context()
	return s, &logs
}

func workerStatus(t *testing.T, st store.Store, id string) store.WorkerStatus {
	t.Helper()
	w, err := st.GetWorker(t.Context(), id)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	return w.Status
}

func taskStatus(t *testing.T, st store.Store, id string) store.TaskStatus {
	t.Helper()
	tk, err := st.GetTask(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return tk.Status
}

func TestSweepStaleWorkers_DeferredWhileBusDown(t *testing.T) {
	st := newCheckedFake(t)
	s, logs := newLivenessScheduler(t, st, busUpAt(-1))
	workerID, taskID, _ := seedStaleWorkerWithTask(t, st, 40*time.Second)

	s.sweepStaleWorkers(t.Context())

	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Errorf("worker status = %q, want online (bus is down)", got)
	}
	if got := taskStatus(t, st, taskID); got != store.TaskStatusRunning {
		t.Errorf("task status = %q, want running (not reclaimed while the bus is down)", got)
	}
	if n := strings.Count(logs.String(), "bus is down; liveness sweep deferred"); n != 1 {
		t.Errorf("deferral INFO logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "bus_down=true") {
		t.Errorf("deferral INFO lacks bus_down=true:\n%s", logs.String())
	}
}

func TestSweepStaleWorkers_DeferredUntilReconnectGraceAfterReconnect(t *testing.T) {
	st := newCheckedFake(t)
	up := 10 * time.Second
	s, logs := newLivenessScheduler(t, st, func() time.Time { return time.Now().UTC().Add(-up) })
	workerID, taskID, _ := seedStaleWorkerWithTask(t, st, 40*time.Second)

	s.sweepStaleWorkers(t.Context())
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Fatalf("worker status = %q after 10s of bus uptime, want online (deferred)", got)
	}
	if !strings.Contains(logs.String(), "bus_down=false") {
		t.Errorf("deferral INFO lacks bus_down=false:\n%s", logs.String())
	}

	// A full WorkerTimeout and a second: a worker that is still backing off
	// its redial looks exactly like this, so the sweep must still wait.
	up = 31 * time.Second
	s.sweepStaleWorkers(t.Context())
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Fatalf("worker status = %q after 31s of bus uptime, want online (the grace is %s)", got, testBusGrace)
	}

	up = testBusGrace + time.Second // grace over, still no heartbeat
	s.sweepStaleWorkers(t.Context())
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOffline {
		t.Errorf("worker status = %q after the grace, want offline (a dead worker is not hidden)", got)
	}
	if got := taskStatus(t, st, taskID); got != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (reclaimed)", got)
	}
}

func TestReapStaleAssignedTasks_DeferredAfterReconnect(t *testing.T) {
	st := newCheckedFake(t)
	up := 10 * time.Second
	s, _ := newLivenessScheduler(t, st, func() time.Time { return time.Now().UTC().Add(-up) })
	taskID, _ := seedAssignedTask(t, st, time.Hour)

	s.reapStaleAssignedTasks(t.Context())
	if got := taskStatus(t, st, taskID); got != store.TaskStatusAssigned {
		t.Fatalf("task status = %q after 10s of bus uptime, want assigned (deferred)", got)
	}

	up = 31 * time.Second
	s.reapStaleAssignedTasks(t.Context())
	if got := taskStatus(t, st, taskID); got != store.TaskStatusAssigned {
		t.Fatalf("task status = %q after 31s of bus uptime, want assigned (the grace is %s)", got, testBusGrace)
	}

	up = testBusGrace + time.Second
	s.reapStaleAssignedTasks(t.Context())
	if got := taskStatus(t, st, taskID); got != store.TaskStatusReady {
		t.Errorf("task status = %q after the grace, want ready (reaped)", got)
	}
}

func TestReapStaleAssignedTasks_DeferredWhileBusDown(t *testing.T) {
	st := newCheckedFake(t)
	s, _ := newLivenessScheduler(t, st, busUpAt(-1))
	taskID, _ := seedAssignedTask(t, st, time.Hour)

	s.reapStaleAssignedTasks(t.Context())

	if got := taskStatus(t, st, taskID); got != store.TaskStatusAssigned {
		t.Errorf("task status = %q, want assigned (bus is down)", got)
	}
}

func TestSweepStaleWorkers_NoSourceKeepsTodaysBehaviour(t *testing.T) {
	st := newCheckedFake(t)
	s, logs := newLivenessScheduler(t, st, nil)
	workerID, taskID, _ := seedStaleWorkerWithTask(t, st, 40*time.Second)

	s.sweepStaleWorkers(t.Context())

	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOffline {
		t.Errorf("worker status = %q, want offline", got)
	}
	if got := taskStatus(t, st, taskID); got != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (reclaimed)", got)
	}
	if strings.Contains(logs.String(), "liveness sweep deferred") {
		t.Errorf("deferral logged with no source wired:\n%s", logs.String())
	}
}

// TestLivenessDeferred_Boundary pins the edge: exactly BusReconnectGrace after
// the reconnect the sweep acts, a nanosecond earlier it does not.
func TestLivenessDeferred_Boundary(t *testing.T) {
	st := newCheckedFake(t)
	upAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, _ := newLivenessScheduler(t, st, func() time.Time { return upAt })

	for _, tc := range []struct {
		name         string
		now          time.Time
		wantDeferred bool
	}{
		{"just before the grace ends", upAt.Add(testBusGrace - time.Nanosecond), true},
		{"exactly at the grace", upAt.Add(testBusGrace), false},
		{"after the grace", upAt.Add(testBusGrace + time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deferred, busDown := s.livenessDeferred(tc.now)
			if deferred != tc.wantDeferred || busDown {
				t.Errorf("livenessDeferred = (%v, busDown=%v), want (%v, false)", deferred, busDown, tc.wantDeferred)
			}
		})
	}
}

// TestNew_BusReconnectGraceDefaultsToWorkerTimeout pins the self-contained
// default: a caller that wires only BusUpSince gets one WorkerTimeout of grace.
func TestNew_BusReconnectGraceDefaultsToWorkerTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkerTimeout = 45 * time.Second
	s := New(cfg, newCheckedFake(t), &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	if got := s.cfg.BusReconnectGrace; got != 45*time.Second {
		t.Errorf("BusReconnectGrace = %v, want WorkerTimeout (45s)", got)
	}
}

// TestSweepOnce_DeferralLoggedOncePerTick drives a whole tick: the worker sweep
// and the assigned-task reap both defer, and the INFO line appears once.
func TestSweepOnce_DeferralLoggedOncePerTick(t *testing.T) {
	st := newCheckedFake(t)
	s, logs := newLivenessScheduler(t, st, busUpAt(-1))
	workerID, workerTask, _ := seedStaleWorkerWithTask(t, st, 40*time.Second)
	assignedTask, _ := seedAssignedTask(t, st, time.Hour)

	s.sweepOnce(t.Context())

	if n := strings.Count(logs.String(), "liveness sweep deferred"); n != 1 {
		t.Errorf("deferral INFO logged %d times in one tick, want 1:\n%s", n, logs.String())
	}
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Errorf("worker status = %q, want online", got)
	}
	if got := taskStatus(t, st, workerTask); got != store.TaskStatusRunning {
		t.Errorf("running task status = %q, want running", got)
	}
	if got := taskStatus(t, st, assignedTask); got != store.TaskStatusAssigned {
		t.Errorf("assigned task status = %q, want assigned", got)
	}
}

// sweepLogs counts the deferral INFO lines of each kind in logs.
func sweepLogs(logs string) (down, startup, reconnect int) {
	return strings.Count(logs, "bus is down; liveness sweep deferred"),
		strings.Count(logs, "bus connected at startup; liveness sweep deferred"),
		strings.Count(logs, "bus reconnected recently; liveness sweep deferred")
}

// TestSweepStaleWorkers_DeferralLoggedOncePerWindow checks that the deferral
// is logged once per outage and once per grace window, not on every sweep
// tick (every 15 s for about 189 s after every start), and that the window
// after startup is not reported as a reconnect.
func TestSweepStaleWorkers_DeferralLoggedOncePerWindow(t *testing.T) {
	st := newCheckedFake(t)
	upAt := time.Now().UTC().Add(-10 * time.Second)
	up := upAt
	s, logs := newLivenessScheduler(t, st, func() time.Time { return up })
	seedStaleWorkerWithTask(t, st, 40*time.Second)

	for range 3 {
		s.sweepStaleWorkers(t.Context())
	}
	if down, startup, reconnect := sweepLogs(logs.String()); down != 0 || startup != 1 || reconnect != 0 {
		t.Fatalf("after three ticks in the startup window: down=%d startup=%d reconnect=%d, want 0/1/0:\n%s",
			down, startup, reconnect, logs.String())
	}
	if !strings.Contains(logs.String(), "resumes_at=") {
		t.Errorf("deferral INFO does not say when the sweep resumes:\n%s", logs.String())
	}

	up = time.Time{} // the bus drops
	for range 3 {
		s.sweepStaleWorkers(t.Context())
	}
	if down, startup, reconnect := sweepLogs(logs.String()); down != 1 || startup != 1 || reconnect != 0 {
		t.Fatalf("after three ticks while down: down=%d startup=%d reconnect=%d, want 1/1/0:\n%s",
			down, startup, reconnect, logs.String())
	}

	up = time.Now().UTC().Add(-time.Second) // the bus comes back
	for range 3 {
		s.sweepStaleWorkers(t.Context())
	}
	if down, startup, reconnect := sweepLogs(logs.String()); down != 1 || startup != 1 || reconnect != 1 {
		t.Fatalf("after three ticks after the reconnect: down=%d startup=%d reconnect=%d, want 1/1/1:\n%s",
			down, startup, reconnect, logs.String())
	}
}
