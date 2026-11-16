// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// markerBus is a busClient whose PublishIngestMarkers counts its calls and
// returns err.
type markerBus struct {
	recordBus

	mu    sync.Mutex
	err   error
	calls int
}

func (b *markerBus) PublishIngestMarkers(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.err
}

func (b *markerBus) published() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// newIngestScheduler builds a Scheduler with no store, so any store call
// panics, logging to logs.
func newIngestScheduler(cfg Config, b busClient, logs *bytes.Buffer) *Scheduler {
	s := New(cfg, nil, b, nil, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ws.NoopNotifier{}, nil)
	s.ctx = context.Background()
	return s
}

// testClock is an injectable clock for the marker loop.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func TestHandleWorkerMessage_MarkerAdvancesTracker(t *testing.T) {
	var logs bytes.Buffer
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &logs)
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	msg := &fakeJSMsg{subject: bus.SubjectMarkerWorker, stamp: t0, natsSeq: 9, delivered: 1}

	s.handleWorkerMessage(msg)

	if !msg.acked || msg.nacked {
		t.Fatalf("marker acked=%v nacked=%v, want acked", msg.acked, msg.nacked)
	}
	if got := s.ingestWorker.effective(t0.Add(time.Second)); !got.Equal(t0) {
		t.Errorf("ingestWorker.effective = %v, want %v", got, t0)
	}
	if got := s.ingestTask.effective(t0.Add(time.Second)); !got.IsZero() {
		t.Errorf("ingestTask.effective = %v, want zero (a worker marker must not advance it)", got)
	}
	if strings.Contains(logs.String(), "unexpected subject") {
		t.Errorf("a marker was logged as an unexpected subject:\n%s", logs.String())
	}
}

func TestHandleTaskStatusMessage_MarkerAdvancesTracker(t *testing.T) {
	var logs bytes.Buffer
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &logs)
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	msg := &fakeJSMsg{subject: bus.SubjectMarkerTask, stamp: t0, natsSeq: 4, delivered: 1}

	s.handleTaskStatusMessage(msg)

	if !msg.acked || msg.nacked {
		t.Fatalf("marker acked=%v nacked=%v, want acked", msg.acked, msg.nacked)
	}
	if got := s.ingestTask.effective(t0.Add(time.Second)); !got.Equal(t0) {
		t.Errorf("ingestTask.effective = %v, want %v", got, t0)
	}
	if got := s.ingestWorker.effective(t0.Add(time.Second)); !got.IsZero() {
		t.Errorf("ingestWorker.effective = %v, want zero (a task marker must not advance it)", got)
	}
	if strings.Contains(logs.String(), "malformed") {
		t.Errorf("a marker's empty body was decoded as a status:\n%s", logs.String())
	}
}

func TestNakMsg_RecordsPendingKeys(t *testing.T) {
	var logs bytes.Buffer
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &logs)
	now := time.Now().UTC()
	body, err := json.Marshal(map[string]string{"task_id": "t1", "attempt_id": "a1"})
	if err != nil {
		t.Fatal(err)
	}
	msg := &fakeJSMsg{subject: bus.TaskStatusSubject("w1", "j1"), data: body, natsSeq: 7, stamp: now, delivered: 1}

	s.nakMsg(t.Context(), msg)

	for _, key := range []string{workerKey("w1"), taskKey("t1"), attemptKey("a1")} {
		if !s.ingestTask.pending(key, now) {
			t.Errorf("%s not pending after the nak", key)
		}
	}
	if n := s.ingestWorker.pendingCount(now); n != 0 {
		t.Errorf("ingestWorker holds %d pending messages, want 0 (a task status belongs to ingestTask)", n)
	}

	s.ackMsg(t.Context(), msg)
	if n := s.ingestTask.pendingCount(now); n != 0 {
		t.Errorf("ingestTask holds %d pending messages after the ack, want 0", n)
	}
}

func TestIngestKeys(t *testing.T) {
	statusBody := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name    string
		subject string
		data    []byte
		want    []string
	}{
		{"heartbeat", bus.WorkerHeartbeatSubject("w1"), []byte(`{}`), []string{workerKey("w1")}},
		{"register", bus.WorkerRegisterSubject("w2"), nil, []string{workerKey("w2")}},
		{"deregister", bus.WorkerDeregisterSubject("w3"), nil, []string{workerKey("w3")}},
		{
			"task status", bus.TaskStatusSubject("w1", "j1"),
			statusBody(protocol.TaskStatusMsg{TaskID: "t1", AttemptID: "a1"}),
			[]string{workerKey("w1"), taskKey("t1"), attemptKey("a1")},
		},
		{
			"task status without an attempt", bus.TaskStatusSubject("w1", "j1"),
			statusBody(protocol.TaskStatusMsg{TaskID: "t1"}),
			[]string{workerKey("w1"), taskKey("t1")},
		},
		{"undecodable task status", bus.TaskStatusSubject("w1", "j1"), []byte("{not json"), []string{workerKey("w1")}},
		{"marker", bus.SubjectMarkerTask, nil, nil},
		{"log chunk", bus.TaskLogsSubject("w1", "t1"), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ingestKeys(&fakeJSMsg{subject: tc.subject, data: tc.data})
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ingestKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTrackerFor(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	for _, tc := range []struct {
		subject string
		want    *ingestTracker
	}{
		{bus.WorkerHeartbeatSubject("w1"), s.ingestWorker},
		{bus.WorkerRegisterSubject("w1"), s.ingestWorker},
		{bus.SubjectMarkerWorker, s.ingestWorker},
		{bus.TaskStatusSubject("w1", "j1"), s.ingestTask},
		{bus.SubjectMarkerTask, s.ingestTask},
		{bus.TaskLogsSubject("w1", "t1"), nil},
		{"sqi.marker.other", nil},
	} {
		if got := s.trackerFor(tc.subject); got != tc.want {
			t.Errorf("trackerFor(%q) = %p, want %p", tc.subject, got, tc.want)
		}
	}
}

// TestAckNak_WithoutMetadataIsIgnored pins that a message whose metadata
// cannot be read changes no tracker.
func TestAckNak_WithoutMetadataIsIgnored(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	now := time.Now().UTC()
	msg := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("w1"), natsSeq: 3, stamp: now, delivered: 1,
		metaErr: errors.New("no metadata"),
	}
	s.nakMsg(t.Context(), msg)
	if !msg.nacked {
		t.Fatal("message not nak'd")
	}
	if n := s.ingestWorker.pendingCount(now); n != 0 {
		t.Errorf("ingestWorker holds %d pending messages, want 0", n)
	}
}

// TestHeartbeatForUnknownWorker_PendingOnlyThatWorker: a heartbeat the store
// cannot apply yet (its worker has not registered) is nak'd and leaves that
// worker, and nothing else, pending.
func TestHeartbeatForUnknownWorker_PendingOnlyThatWorker(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	s.store = fake.New()
	now := time.Now().UTC()
	msg := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("w-ghost"), natsSeq: 11, stamp: now, delivered: 1,
		data: workerMsgJSON(t, protocol.HeartbeatMsg{Version: protocol.ProtocolVersion, WorkerID: "w-ghost", At: now}),
	}

	s.handleWorkerMessage(msg)

	if !msg.nacked || msg.acked {
		t.Fatalf("heartbeat acked=%v nacked=%v, want nak'd", msg.acked, msg.nacked)
	}
	if got, want := s.ingestWorker.pendingIDs(keyKindWorker, now), map[string]bool{"w-ghost": true}; !maps.Equal(got, want) {
		t.Errorf("pending workers = %v, want %v", got, want)
	}
	for _, kind := range []string{keyKindTask, keyKindAttempt} {
		if got := s.ingestWorker.pendingIDs(kind, now); len(got) != 0 {
			t.Errorf("pending %s IDs = %v, want none", kind, got)
		}
	}
	if n := s.ingestTask.pendingCount(now); n != 0 {
		t.Errorf("ingestTask holds %d pending messages, want 0", n)
	}
}

func TestRunIngestMarkers_PublishFailureDoesNotAdvance(t *testing.T) {
	var logs bytes.Buffer
	b := &markerBus{err: errors.New("broker unreachable")}
	s := newIngestScheduler(DefaultConfig(), b, &logs)
	clock := &testClock{t: time.Now()}
	s.now = clock.now

	// The loop publishes as soon as it starts.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.runIngestMarkers(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for b.published() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if b.published() == 0 {
		t.Fatal("runIngestMarkers published no marker at start")
	}

	for range 4 {
		clock.t = clock.t.Add(ingestMarkerInterval)
		s.ingestMarkerTick(t.Context())
	}
	if got := b.published(); got != 5 {
		t.Errorf("published %d times, want 5", got)
	}
	for name, tr := range map[string]*ingestTracker{"worker": s.ingestWorker, "task": s.ingestTask} {
		if got := tr.effective(clock.t); !got.IsZero() {
			t.Errorf("%s tracker effective = %v after failed publishes, want zero", name, got)
		}
	}
	if n := strings.Count(logs.String(), "broker unreachable"); n != 1 {
		t.Errorf("publish failure logged %d times in one outage, want once:\n%s", n, logs.String())
	}

	// A success ends the outage; the next failure is logged again.
	b.mu.Lock()
	b.err = nil
	b.mu.Unlock()
	clock.t = clock.t.Add(ingestMarkerInterval)
	s.ingestMarkerTick(t.Context())
	b.mu.Lock()
	b.err = errors.New("broker unreachable")
	b.mu.Unlock()
	clock.t = clock.t.Add(ingestMarkerInterval)
	s.ingestMarkerTick(t.Context())
	if n := strings.Count(logs.String(), "broker unreachable"); n != 2 {
		t.Errorf("publish failure logged %d times across two outages, want twice:\n%s", n, logs.String())
	}
}

func TestRunIngestMarkers_PauseHolds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gap   time.Duration
		holds bool
	}{
		{"7s gap holds", 7 * time.Second, true},
		{"3s gap does not", 3 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			cfg := DefaultConfig()
			s := newIngestScheduler(cfg, &markerBus{}, &logs)
			t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			clock := &testClock{t: t0}
			s.now = clock.now

			s.ingestMarkerTick(t.Context())
			caught := t0.Add(time.Second)
			s.ingestWorker.marker(caught)
			s.ingestTask.marker(caught)
			clock.t = t0.Add(tc.gap)
			s.ingestMarkerTick(t.Context())

			during := clock.t.Add(cfg.WorkerTimeout - time.Millisecond)
			after := clock.t.Add(cfg.WorkerTimeout + time.Millisecond)
			want := caught
			if tc.holds {
				want = t0
			}
			for name, tr := range map[string]*ingestTracker{"worker": s.ingestWorker, "task": s.ingestTask} {
				if got := tr.effective(during); !got.Equal(want) {
					t.Errorf("%s effective during the hold window = %v, want %v", name, got, want)
				}
				if got := tr.effective(after); !got.Equal(caught) {
					t.Errorf("%s effective after WorkerTimeout = %v, want %v", name, got, caught)
				}
			}
			if logged := strings.Contains(logs.String(), "scheduler: process pause detected"); logged != tc.holds {
				t.Errorf("pause logged = %v, want %v:\n%s", logged, tc.holds, logs.String())
			}
		})
	}
}

func TestCheckBusInterruption_Holds(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var interrupted time.Time
	cfg := DefaultConfig()
	cfg.BusInterruptedAt = func() time.Time { return interrupted }
	s := newIngestScheduler(cfg, &markerBus{}, &bytes.Buffer{})
	caught := at.Add(time.Second)
	s.ingestWorker.marker(caught)
	s.ingestTask.marker(caught)

	// No interruption yet: nothing is held.
	s.checkBusInterruption(at)
	if got := s.ingestWorker.effective(at.Add(2 * time.Second)); !got.Equal(caught) {
		t.Fatalf("worker effective with no interruption = %v, want %v", got, caught)
	}

	interrupted = at
	s.checkBusInterruption(at.Add(2 * time.Second))
	for _, tc := range []struct {
		name  string
		tr    *ingestTracker
		until time.Duration
	}{
		{"worker", s.ingestWorker, 10 * time.Second},
		{"task", s.ingestTask, 30 * time.Second},
	} {
		if got := tc.tr.effective(at.Add(tc.until - time.Millisecond)); !got.Equal(at) {
			t.Errorf("%s effective inside the hold = %v, want %v", tc.name, got, at)
		}
		if got := tc.tr.effective(at.Add(tc.until + time.Millisecond)); !got.Equal(caught) {
			t.Errorf("%s effective after the hold = %v, want %v", tc.name, got, caught)
		}
	}

	// The same interruption seen again does not hold again.
	later := at.Add(time.Minute)
	s.ingestWorker.marker(later.Add(-time.Second))
	s.checkBusInterruption(later)
	if got := s.ingestWorker.effective(later); !got.Equal(later.Add(-time.Second)) {
		t.Errorf("worker effective after re-reading the same interruption = %v, want %v", got, later.Add(-time.Second))
	}
}

func TestRefreshIngestGauges(t *testing.T) {
	s := newMetricsScheduler(nil, &recordBus{}, "")
	now := time.Now().UTC()
	s.ingestTask.marker(now.Add(-3 * time.Second))
	s.nakMsg(t.Context(), &fakeJSMsg{subject: bus.WorkerHeartbeatSubject("w1"), natsSeq: 1, stamp: now, delivered: 1})
	s.nakMsg(t.Context(), &fakeJSMsg{subject: bus.WorkerHeartbeatSubject("w2"), natsSeq: 2, stamp: now, delivered: 1})

	s.refreshIngestGauges(t.Context())

	if got := gaugeValue(t, s.metrics.SchedulerIngestPending.WithLabelValues("worker")); got != 2 {
		t.Errorf("worker pending = %v, want 2", got)
	}
	if got := gaugeValue(t, s.metrics.SchedulerIngestPending.WithLabelValues("task")); got != 0 {
		t.Errorf("task pending = %v, want 0", got)
	}
	if got := gaugeValue(t, s.metrics.SchedulerIngestLagSeconds.WithLabelValues("task")); got < 3 || got > 60 {
		t.Errorf("task lag = %vs, want about 3s", got)
	}
	if got := gaugeValue(t, s.metrics.SchedulerIngestLagSeconds.WithLabelValues("worker")); got != 0 {
		t.Errorf("worker lag = %vs, want 0 before its first marker", got)
	}
}
