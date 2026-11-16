// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the liveness checks judging by how far the consumers have applied
// their streams (sweep_ingest.go): a heartbeat or status report still queued
// must not read as silence.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ingestSweepTimeout is the WorkerTimeout and AssignedTaskTimeout these tests
// run with.
const ingestSweepTimeout = 30 * time.Second

// newIngestSweepScheduler is a metrics scheduler with both timeouts at
// ingestSweepTimeout, its clock at now, and its log captured.
func newIngestSweepScheduler(t *testing.T, st store.Store, now time.Time) (*Scheduler, *testClock, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	s.cfg.WorkerTimeout = ingestSweepTimeout
	s.cfg.AssignedTaskTimeout = ingestSweepTimeout
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	clock := &testClock{t: now}
	s.now = clock.now
	return s, clock, &logs
}

// seedIngestSweepWorker seeds the stale worker of [seedStaleWorkerWithTask]
// with its heartbeat an hour ago and returns its ID, its task's ID and the
// heartbeat time.
func seedIngestSweepWorker(t *testing.T, st store.Store) (workerID, taskID string, t0 time.Time) {
	t.Helper()
	workerID, taskID, _ = seedStaleWorkerWithTask(t, st, time.Hour)
	w, err := st.GetWorker(t.Context(), workerID)
	if err != nil || w.LastHeartbeatAt == nil {
		t.Fatalf("GetWorker = (%+v, %v), want a heartbeat", w, err)
	}
	return workerID, taskID, *w.LastHeartbeatAt
}

// markers records that both consumers have processed a marker stamped at.
func markers(s *Scheduler, worker, task time.Time) {
	s.ingestWorker.marker(worker)
	s.ingestTask.marker(task)
}

func TestSweep_NoVerdictWhileWorkerConsumerLags(t *testing.T) {
	st := newCheckedFake(t)
	workerID, taskID, t0 := seedIngestSweepWorker(t, st)
	now := t0.Add(120 * time.Second)
	s, _, _ := newIngestSweepScheduler(t, st, now)
	markers(s, t0.Add(5*time.Second), now)

	s.sweepStaleWorkers(t.Context())

	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Errorf("worker status = %q, want online (its later heartbeats may still be queued)", got)
	}
	if got := taskStatus(t, st, taskID); got != store.TaskStatusRunning {
		t.Errorf("task status = %q, want running", got)
	}
}

func TestSweep_DeadWorkerSweptWithMarkersOnly(t *testing.T) {
	st := newCheckedFake(t)
	workerID, taskID, t0 := seedIngestSweepWorker(t, st)
	caughtUp := t0.Add(ingestSweepTimeout + time.Second)
	s, _, _ := newIngestSweepScheduler(t, st, caughtUp)
	markers(s, caughtUp, caughtUp)

	s.sweepStaleWorkers(t.Context())

	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOffline {
		t.Errorf("worker status = %q, want offline (no heartbeat once the consumers caught up)", got)
	}
	if got := taskStatus(t, st, taskID); got != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (reclaimed)", got)
	}
}

func TestSweep_SkipsWorkerWithPendingHeartbeat(t *testing.T) {
	testSweepSkipsPending(t, "heartbeat", func(s *Scheduler) *ingestTracker { return s.ingestWorker })
}

func TestSweep_SkipsWorkerWithPendingStatusReport(t *testing.T) {
	testSweepSkipsPending(t, "status report", func(s *Scheduler) *ingestTracker { return s.ingestTask })
}

// testSweepSkipsPending checks that a stale worker named by a nak'd message
// in tracker stays online until the message is applied.
func testSweepSkipsPending(t *testing.T, what string, tracker func(*Scheduler) *ingestTracker) {
	t.Helper()
	st := newCheckedFake(t)
	workerID, taskID, t0 := seedIngestSweepWorker(t, st)
	now := t0.Add(2 * time.Minute)
	s, _, _ := newIngestSweepScheduler(t, st, now)
	markers(s, now, now)
	tracker(s).nak(7, now.Add(-time.Second), 1, []string{workerKey(workerID), taskKey(taskID)})

	s.sweepStaleWorkers(t.Context())

	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Errorf("worker status = %q, want online (a nak'd %s names it)", got, what)
	}

	tracker(s).ack(7)
	s.sweepStaleWorkers(t.Context())

	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOffline {
		t.Errorf("worker status = %q after the %s was applied, want offline", got, what)
	}
}

func TestSweep_WaitsForTaskConsumer(t *testing.T) {
	st := newCheckedFake(t)
	workerID, _, t0 := seedIngestSweepWorker(t, st)
	now := t0.Add(60 * time.Second)
	s, _, _ := newIngestSweepScheduler(t, st, now)
	markers(s, now, t0.Add(20*time.Second))

	s.sweepStaleWorkers(t.Context())
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Fatalf("worker status = %q with the task consumer at t0+20s, want online", got)
	}

	s.ingestTask.marker(t0.Add(29 * time.Second))
	s.sweepStaleWorkers(t.Context())
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Fatalf("worker status = %q with the task consumer at t0+29s, want online", got)
	}

	s.ingestTask.marker(t0.Add(ingestSweepTimeout))
	s.sweepStaleWorkers(t.Context())
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOffline {
		t.Errorf("worker status = %q with the task consumer at t0+30s, want offline", got)
	}
}

func TestSweep_PromotedStandbyJudgesNothingBeforeFirstMarkers(t *testing.T) {
	st := newCheckedFake(t)
	workerID, taskID, t0 := seedIngestSweepWorker(t, st)
	assignedID, _ := seedAssignedTask(t, st, time.Hour)
	now := t0.Add(time.Hour)
	s, _, _ := newIngestSweepScheduler(t, st, now)
	s.cfg.BusReconnectGrace = testBusGrace
	longAgo := now.Add(-2 * testBusGrace)
	s.cfg.BusUpSince = func() time.Time { return longAgo }
	s.startedAt = longAgo

	s.sweepStaleWorkers(t.Context())
	s.reapStaleAssignedTasks(t.Context())

	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Errorf("worker status = %q, want online (no marker processed yet)", got)
	}
	if got := taskStatus(t, st, taskID); got != store.TaskStatusRunning {
		t.Errorf("task status = %q, want running", got)
	}
	if got := taskStatus(t, st, assignedID); got != store.TaskStatusAssigned {
		t.Errorf("assigned task status = %q, want assigned (no marker processed yet)", got)
	}

	// Only the task consumer has caught up: the worker sweep still waits.
	s.ingestTask.marker(now)
	s.sweepStaleWorkers(t.Context())
	if got := workerStatus(t, st, workerID); got != store.WorkerStatusOnline {
		t.Errorf("worker status = %q before the worker consumer's first marker, want online", got)
	}
}

func TestReap_UsesTaskEffectiveAndSkipsPending(t *testing.T) {
	tests := []struct {
		name       string
		marker     time.Duration // task marker, after the assignment
		pending    bool
		wantStatus store.TaskStatus
	}{
		{name: "consumer behind", marker: 10 * time.Second, wantStatus: store.TaskStatusAssigned},
		{name: "consumer caught up", marker: ingestSweepTimeout + time.Second, wantStatus: store.TaskStatusReady},
		{name: "report pending", marker: ingestSweepTimeout + time.Second, pending: true, wantStatus: store.TaskStatusAssigned},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newCheckedFake(t)
			taskID, _ := seedAssignedTask(t, st, time.Hour)
			tk, err := st.GetTask(t.Context(), taskID)
			if err != nil || tk.AssignedAt == nil {
				t.Fatalf("GetTask = (%+v, %v), want an assigned_at", tk, err)
			}
			t0 := *tk.AssignedAt
			now := t0.Add(2 * time.Minute)
			s, _, _ := newIngestSweepScheduler(t, st, now)
			markers(s, now, t0.Add(tc.marker))
			if tc.pending {
				s.ingestTask.nak(9, t0, 1, []string{workerKey("w-live"), taskKey(taskID)})
			}

			s.reapStaleAssignedTasks(t.Context())

			if got := taskStatus(t, st, taskID); got != tc.wantStatus {
				t.Errorf("task status = %q, want %q", got, tc.wantStatus)
			}
		})
	}
}

func TestSweep_HeldBackLoggedOncePerEpisode(t *testing.T) {
	const msg = "liveness sweep held back until the consumers catch up"
	st := newCheckedFake(t)
	now := time.Now().UTC()
	s, clock, logs := newIngestSweepScheduler(t, st, now)
	markers(s, now, now)

	s.sweepStaleWorkers(t.Context())
	if strings.Contains(logs.String(), msg) {
		t.Fatalf("held back logged while caught up:\n%s", logs.String())
	}

	// The consumers stop advancing: one log for the whole episode.
	for range 3 {
		clock.t = clock.t.Add(s.cfg.HeartbeatSweepInterval + time.Second)
		s.sweepStaleWorkers(t.Context())
	}
	if n := strings.Count(logs.String(), msg); n != 1 {
		t.Fatalf("held back logged %d times in one episode, want once:\n%s", n, logs.String())
	}
	for _, attr := range []string{"worker_caught_up_to=", "task_caught_up_to="} {
		if !strings.Contains(logs.String(), attr) {
			t.Errorf("held-back log lacks %s:\n%s", attr, logs.String())
		}
	}

	// They catch up, ending the episode; falling behind again is a new one.
	markers(s, clock.t, clock.t)
	s.sweepStaleWorkers(t.Context())
	clock.t = clock.t.Add(s.cfg.HeartbeatSweepInterval + time.Second)
	s.sweepStaleWorkers(t.Context())
	if n := strings.Count(logs.String(), msg); n != 2 {
		t.Errorf("held back logged %d times over two episodes, want twice:\n%s", n, logs.String())
	}
}

// ingestCaughtUp records both consumers as caught up to now, as the marker
// loop leaves them while the consumers keep up, so a test of what the sweeps
// decide is not held back by trackers that have processed no marker.
func ingestCaughtUp(s *Scheduler) {
	now := time.Now().UTC()
	markers(s, now, now)
}
