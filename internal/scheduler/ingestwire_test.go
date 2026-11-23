// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// markerBus is a busClient whose PublishIngestMarkers counts its calls,
// runs onPublish when set, records the timeout and returns err.
type markerBus struct {
	recordBus

	mu          sync.Mutex
	err         error
	calls       int
	onPublish   func()
	lastTimeout time.Duration
}

func (b *markerBus) PublishIngestMarkers(_ context.Context, timeout time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.lastTimeout = timeout
	if b.onPublish != nil {
		b.onPublish()
	}
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

// TestMarkerAdvancesOnlyItsConsumersTracker: each tracked consumer's handler
// advances its own tracker to a marker's broker stamp, leaves the other
// tracker alone, acks the marker, and does not handle it as an ordinary
// message.
func TestMarkerAdvancesOnlyItsConsumersTracker(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		subject string
		handle  func(*Scheduler, jetstream.Msg)
		task    bool
		// notLogged is what the handler would log had it taken the marker for
		// an ordinary message.
		notLogged string
	}{
		{"worker", bus.SubjectMarkerWorker, (*Scheduler).handleWorkerMessage, false, "unexpected subject"},
		{"task status", bus.SubjectMarkerTask, (*Scheduler).handleTaskStatusMessage, true, "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := newIngestScheduler(DefaultConfig(), &recordBus{}, &logs)
			msg := &fakeJSMsg{subject: tc.subject, stamp: t0, natsSeq: 9, delivered: 1}

			tc.handle(s, msg)

			if !msg.acked || msg.nacked {
				t.Fatalf("marker acked=%v nacked=%v, want acked", msg.acked, msg.nacked)
			}
			own, other := s.ingestWorker, s.ingestTask
			if tc.task {
				own, other = other, own
			}
			if got := own.effective(t0.Add(time.Second)); !got.Equal(t0) {
				t.Errorf("own tracker effective = %v, want %v", got, t0)
			}
			if got := other.effective(t0.Add(time.Second)); !got.IsZero() {
				t.Errorf("other tracker effective = %v, want zero (a marker advances only its consumer's)", got)
			}
			if strings.Contains(logs.String(), tc.notLogged) {
				t.Errorf("the marker was handled as an ordinary message:\n%s", logs.String())
			}
		})
	}
}

func TestNakMsg_RecordsPendingRef(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	now := time.Now().UTC()
	msg := newIngestMsg(s.ingestTask, &fakeJSMsg{subject: bus.TaskStatusSubject("w1", "j1"), natsSeq: 7, stamp: now, delivered: 1})

	s.nakMsg(t.Context(), msg, ingestRef{worker: "w1", task: "t1", attempt: "a1"})

	got := s.ingestTask.pending(now)
	if !got.workers["w1"] || !got.tasks["t1"] || !got.attempts["a1"] {
		t.Errorf("pending after the nak = %+v, want worker w1, task t1 and attempt a1", got)
	}
	if n := s.ingestWorker.pendingCount(now); n != 0 {
		t.Errorf("ingestWorker holds %d pending messages, want 0 (the message is bound to ingestTask)", n)
	}

	s.ackMsg(t.Context(), msg)
	if n := s.ingestTask.pendingCount(now); n != 0 {
		t.Errorf("ingestTask holds %d pending messages after the ack, want 0", n)
	}
}

// TestNakMsg_UnboundMessageChangesNoTracker: a message no tracked consumer
// received (a log chunk) is nak'd and acked without touching a tracker.
func TestNakMsg_UnboundMessageChangesNoTracker(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	now := time.Now().UTC()
	msg := &fakeJSMsg{subject: bus.TaskLogsSubject("w1", "t1"), natsSeq: 7, stamp: now, delivered: 1}

	s.nakMsg(t.Context(), msg, ingestRef{worker: "w1"})

	if !msg.nacked {
		t.Fatal("message not nak'd")
	}
	if w, k := s.ingestWorker.pendingCount(now), s.ingestTask.pendingCount(now); w != 0 || k != 0 {
		t.Errorf("pending worker = %d, task = %d, want 0 and 0", w, k)
	}
}

// TestNakMsg_FailedNakStillPending: a message whose nak did not reach the
// broker is still unapplied and comes back after AckWait, so what it names is
// recorded pending all the same.
func TestNakMsg_FailedNakStillPending(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	now := time.Now().UTC()
	msg := newIngestMsg(s.ingestWorker, &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("w1"), natsSeq: 5, stamp: now, delivered: 1,
		nakErr: errors.New("connection closed"),
	})

	s.nakMsg(t.Context(), msg, ingestRef{worker: "w1"})

	if !s.ingestWorker.pending(now).workers["w1"] {
		t.Error("worker w1 not pending after a nak that failed")
	}
}

// failingIngestStore fails the writes a worker or task status message makes
// first, so its handler naks it.
type failingIngestStore struct {
	store.Store
}

var errIngestStore = errors.New("store unavailable")

func (failingIngestStore) RegisterWorker(context.Context, store.Worker) (store.Worker, store.Registration, error) {
	return store.Worker{}, store.Registration{}, errIngestStore
}

func (failingIngestStore) UpdateWorkerHeartbeat(context.Context, string, time.Time, *time.Time, *time.Time) (store.HeartbeatOutcome, error) {
	return store.HeartbeatOutcome{}, errIngestStore
}

func (failingIngestStore) GetTaskAttempt(context.Context, string) (store.TaskAttempt, error) {
	return store.TaskAttempt{}, errIngestStore
}

// TestTrackedConsumers_NakRecordsWhatTheMessageNames: each tracked consumer
// records a message its handler naks in its own tracker, naming the
// subject's worker and, for a task status, the body's task and attempt.
func TestTrackedConsumers_NakRecordsWhatTheMessageNames(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		msg    *fakeJSMsg
		task   bool
		want   pendingNames
		handle func(*Scheduler, *fakeJSMsg)
	}{
		{
			name: "register",
			msg: &fakeJSMsg{subject: bus.WorkerRegisterSubject("w2"), data: workerMsgJSON(t, protocol.RegisterMsg{
				Version: protocol.ProtocolVersion, WorkerID: "w2", InstanceID: "i1",
			})},
			want:   pendingNames{workers: map[string]bool{"w2": true}},
			handle: func(s *Scheduler, m *fakeJSMsg) { s.handleWorkerMessage(m) },
		},
		{
			name: "heartbeat",
			msg: &fakeJSMsg{subject: bus.WorkerHeartbeatSubject("w1"), data: workerMsgJSON(t, protocol.HeartbeatMsg{
				Version: protocol.ProtocolVersion, WorkerID: "w1", At: now,
			})},
			want:   pendingNames{workers: map[string]bool{"w1": true}},
			handle: func(s *Scheduler, m *fakeJSMsg) { s.handleWorkerMessage(m) },
		},
		{
			name: "task status",
			msg: &fakeJSMsg{subject: bus.TaskStatusSubject("w1", "j1"), data: workerMsgJSON(t, protocol.TaskStatusMsg{
				Version: protocol.ProtocolVersion, WorkerID: "w1", TaskID: "t1", AttemptID: "a1",
			})},
			task: true,
			want: pendingNames{
				workers: map[string]bool{"w1": true}, tasks: map[string]bool{"t1": true}, attempts: map[string]bool{"a1": true},
			},
			handle: func(s *Scheduler, m *fakeJSMsg) { s.handleTaskStatusMessage(m) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
			s.store = failingIngestStore{Store: fake.New()}
			tc.msg.natsSeq, tc.msg.stamp, tc.msg.delivered = 3, now, 1

			tc.handle(s, tc.msg)

			if !tc.msg.nacked || tc.msg.acked {
				t.Fatalf("acked=%v nacked=%v, want nak'd", tc.msg.acked, tc.msg.nacked)
			}
			own, other := s.ingestWorker, s.ingestTask
			if tc.task {
				own, other = other, own
			}
			if got := own.pending(now); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("pending = %+v, want %+v", got, tc.want)
			}
			if n := other.pendingCount(now); n != 0 {
				t.Errorf("the other consumer's tracker holds %d pending messages, want 0", n)
			}
		})
	}
}

// TestAckNak_WithoutMetadataIsIgnored pins that a message whose metadata
// cannot be read changes no tracker.
func TestAckNak_WithoutMetadataIsIgnored(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	now := time.Now().UTC()
	raw := &fakeJSMsg{
		subject: bus.WorkerHeartbeatSubject("w1"), natsSeq: 3, stamp: now, delivered: 1,
		metaErr: errors.New("no metadata"),
	}
	s.nakMsg(t.Context(), newIngestMsg(s.ingestWorker, raw), ingestRef{worker: "w1"})
	if !raw.nacked {
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
	got := s.ingestWorker.pending(now)
	if want := map[string]bool{"w-ghost": true}; !maps.Equal(got.workers, want) {
		t.Errorf("pending workers = %v, want %v", got.workers, want)
	}
	if len(got.tasks) != 0 || len(got.attempts) != 0 {
		t.Errorf("pending tasks = %v, attempts = %v, want none", got.tasks, got.attempts)
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
	// A marker handled before the outage: the failed publishes must leave it
	// the effective time, neither advanced nor held back.
	handled := clock.t.Add(-time.Second)
	s.ingestWorker.marker(handled)
	s.ingestTask.marker(handled)

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

	// The loop's own ticker may have fired once more under a loaded host, so
	// count only the ticks driven here, which keep their own loop state.
	before := b.published()
	logs.Reset()
	var loop ingestLoopState
	for range 4 {
		clock.t = clock.t.Add(ingestMarkerInterval)
		s.ingestMarkerTick(t.Context(), &loop)
	}
	if got := b.published() - before; got != 4 {
		t.Errorf("published %d times in 4 ticks, want 4", got)
	}
	for name, tr := range map[string]*ingestTracker{"worker": s.ingestWorker, "task": s.ingestTask} {
		if got := tr.effective(clock.t); !got.Equal(handled) {
			t.Errorf("%s tracker effective = %v after failed publishes, want %v (the marker handled before)", name, got, handled)
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
	s.ingestMarkerTick(t.Context(), &loop)
	b.mu.Lock()
	b.err = errors.New("broker unreachable")
	b.mu.Unlock()
	clock.t = clock.t.Add(ingestMarkerInterval)
	s.ingestMarkerTick(t.Context(), &loop)
	if n := strings.Count(logs.String(), "broker unreachable"); n != 2 {
		t.Errorf("publish failure logged %d times across two outages, want twice:\n%s", n, logs.String())
	}
}

// TestRunIngestMarkers_PauseHolds: a gap of more than ingestPauseThreshold
// between the end of one tick's work and the start of the next holds both
// trackers at the end of that work for WorkerTimeout.
func TestRunIngestMarkers_PauseHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish time.Duration // how long the first tick's publish takes
		gap     time.Duration // from the end of the first tick to the second
		holds   bool
	}{
		{"7s gap holds", 0, 7 * time.Second, true},
		{"3s gap does not", 0, 3 * time.Second, false},
		{"7s gap after a slow publish holds", time.Second, 7 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			cfg := DefaultConfig()
			t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			clock := &testClock{t: t0}
			b := &markerBus{onPublish: func() { clock.t = clock.t.Add(tc.publish) }}
			s := newIngestScheduler(cfg, b, &logs)
			s.now = clock.now

			var loop ingestLoopState
			s.ingestMarkerTick(t.Context(), &loop)
			b.mu.Lock()
			b.onPublish = nil
			b.mu.Unlock()
			prevEnd := t0.Add(tc.publish)
			caught := prevEnd.Add(time.Second)
			s.ingestWorker.marker(caught)
			s.ingestTask.marker(caught)
			clock.t = prevEnd.Add(tc.gap)
			s.ingestMarkerTick(t.Context(), &loop)

			during := clock.t.Add(cfg.WorkerTimeout - time.Millisecond)
			after := clock.t.Add(cfg.WorkerTimeout + time.Millisecond)
			want := caught
			if tc.holds {
				want = prevEnd
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

// TestRunIngestMarkers_PublishUnderThresholdIsNotAPause: a publish slower
// than the ticker's period but within ingestPauseThreshold (a connected but
// stalled broker), with each tick starting as soon as the previous one ends as
// a ticker that has already fired does, is not reported as a process pause and
// holds nothing.
func TestRunIngestMarkers_PublishUnderThresholdIsNotAPause(t *testing.T) {
	for _, publish := range []time.Duration{3 * time.Second, 5 * time.Second} {
		t.Run(publish.String(), func(t *testing.T) {
			var logs bytes.Buffer
			t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			clock := &testClock{t: t0}
			b := &markerBus{onPublish: func() { clock.t = clock.t.Add(publish) }}
			s := newIngestScheduler(DefaultConfig(), b, &logs)
			s.now = clock.now
			caught := t0.Add(-time.Second)
			s.ingestWorker.marker(caught)
			s.ingestTask.marker(caught)

			var loop ingestLoopState
			for range 5 {
				s.ingestMarkerTick(t.Context(), &loop)
			}

			if strings.Contains(logs.String(), "scheduler: process pause detected") {
				t.Errorf("a %v publish was reported as a pause:\n%s", publish, logs.String())
			}
			for name, tr := range map[string]*ingestTracker{"worker": s.ingestWorker, "task": s.ingestTask} {
				if got := tr.effective(clock.t); !got.Equal(caught) {
					t.Errorf("%s effective = %v, want %v (no hold)", name, got, caught)
				}
			}
		})
	}
}

// TestRunIngestMarkers_PauseDuringPublishHolds: a tick whose own work outlasts
// ingestPauseThreshold (the process stopped while a bounded publish was in
// flight) holds both trackers at the tick's start until WorkerTimeout after
// its end, whether or not the publish then succeeded.
func TestRunIngestMarkers_PauseDuringPublishHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"publish succeeds", nil},
		{"publish fails", errors.New("broker unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			cfg := DefaultConfig()
			t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			clock := &testClock{t: t0}
			b := &markerBus{err: tc.err, onPublish: func() { clock.t = clock.t.Add(7 * time.Second) }}
			s := newIngestScheduler(cfg, b, &logs)
			s.now = clock.now

			var loop ingestLoopState
			s.ingestMarkerTick(t.Context(), &loop)
			end := clock.t
			// The marker the publish stored is processed after the pause.
			caught := end.Add(time.Second)
			s.ingestWorker.marker(caught)
			s.ingestTask.marker(caught)

			during := end.Add(cfg.WorkerTimeout - time.Millisecond)
			after := end.Add(cfg.WorkerTimeout + time.Millisecond)
			for name, tr := range map[string]*ingestTracker{"worker": s.ingestWorker, "task": s.ingestTask} {
				if got := tr.effective(during); !got.Equal(t0) {
					t.Errorf("%s effective during the hold window = %v, want %v (the tick's start)", name, got, t0)
				}
				if got := tr.effective(after); !got.Equal(caught) {
					t.Errorf("%s effective after WorkerTimeout = %v, want %v", name, got, caught)
				}
			}
			if !strings.Contains(logs.String(), "scheduler: process pause detected") {
				t.Errorf("a pause during the publish was not logged:\n%s", logs.String())
			}
		})
	}
}

// shiftMonotonic returns t with its monotonic reading moved by d and its wall
// clock reading unchanged: a reading taken after the monotonic clock stood
// still (d < 0) while the wall clock ran on, as on a host that slept. The time
// package offers no way to build one, so it edits the reading in place, and
// fails the test if the result is not what it should be.
func shiftMonotonic(t *testing.T, tm time.Time, d time.Duration) time.Time {
	t.Helper()
	out := tm
	// time.Time is {wall uint64; ext int64; loc *Location}; with a monotonic
	// reading, ext holds it.
	p := (*struct {
		wall uint64
		ext  int64
		loc  *time.Location
	})(unsafe.Pointer(&out))
	p.ext += int64(d)
	if !out.Round(0).Equal(tm.Round(0)) || out.Sub(tm) != d {
		t.Fatalf("shiftMonotonic: the time.Time layout is not the one expected (wall %v -> %v, monotonic moved %v, want %v)",
			tm.Round(0), out.Round(0), out.Sub(tm), d)
	}
	return out
}

// TestPauseGap: the gap between two clock readings is the larger of their
// monotonic and wall-clock differences, so a pause during which the monotonic
// clock stood still (a host that slept) still shows.
func TestPauseGap(t *testing.T) {
	mono := time.Now()
	for _, tc := range []struct {
		name      string
		prev, now time.Time
		want      time.Duration
	}{
		{"wall only", time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 12, 0, 7, 0, time.UTC), 7 * time.Second},
		{"both clocks agree", mono, mono.Add(7 * time.Second), 7 * time.Second},
		{"monotonic clock stood still", mono, shiftMonotonic(t, mono.Add(10*time.Second), -8*time.Second), 10 * time.Second},
		{"wall clock stepped back", mono, shiftMonotonic(t, mono.Add(-5*time.Second), 12*time.Second), 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pauseGap(tc.prev, tc.now); got != tc.want {
				t.Errorf("pauseGap = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunIngestMarkers_PublishHasADeadline: each publish is bounded by
// ingestMarkerInterval, so a stalled broker cannot hold up the loop.
func TestRunIngestMarkers_PublishHasADeadline(t *testing.T) {
	b := &markerBus{}
	s := newIngestScheduler(DefaultConfig(), b, &bytes.Buffer{})
	var loop ingestLoopState
	s.ingestMarkerTick(t.Context(), &loop)
	b.mu.Lock()
	timeout := b.lastTimeout
	b.mu.Unlock()
	if timeout <= 0 {
		t.Fatalf("PublishIngestMarkers was called with timeout %v, want a bound", timeout)
	}
	if timeout > ingestMarkerInterval {
		t.Errorf("publish timeout %v is longer than one marker interval (%v)", timeout, ingestMarkerInterval)
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
	noIngestMarker(s)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s.now = (&testClock{t: now}).now
	s.startedAt = now.Add(-40 * time.Second)
	s.ingestTask.marker(now.Add(-3 * time.Second))
	for i, id := range []string{"w1", "w2"} {
		msg := &fakeJSMsg{subject: bus.WorkerHeartbeatSubject(id), natsSeq: uint64(i + 1), stamp: now, delivered: 1}
		s.nakMsg(t.Context(), newIngestMsg(s.ingestWorker, msg), ingestRef{worker: id})
	}

	s.refreshIngestGauges(t.Context())

	if got := gaugeValue(t, s.metrics.SchedulerIngestPending.WithLabelValues("worker")); got != 2 {
		t.Errorf("worker pending = %v, want 2", got)
	}
	if got := gaugeValue(t, s.metrics.SchedulerIngestPending.WithLabelValues("task")); got != 0 {
		t.Errorf("task pending = %v, want 0", got)
	}
	if got := gaugeValue(t, s.metrics.SchedulerIngestLagSeconds.WithLabelValues("task")); got != 3 {
		t.Errorf("task lag = %vs, want 3s", got)
	}
	if got := gaugeValue(t, s.metrics.SchedulerIngestLagSeconds.WithLabelValues("worker")); got != 40 {
		t.Errorf("worker lag = %vs, want 40s (the time since the scheduler started) before its first marker", got)
	}
}

// TestHandleWorkerMessage_SequenceGapHoldsTracker: a first delivery whose
// stream sequence skips past the previous first delivery means the stream
// removed messages before the consumer read them, and holds the worker
// tracker at the marker processed before the skip. A redelivery, which may
// arrive out of order, is never read as a skip, and a marker can be the
// message that reveals one.
func TestHandleWorkerMessage_SequenceGapHoldsTracker(t *testing.T) {
	var logs bytes.Buffer
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &logs)
	s.store = fake.New()
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	marker := func(seq uint64, sec int, delivered uint64) {
		s.handleWorkerMessage(&fakeJSMsg{subject: bus.SubjectMarkerWorker, natsSeq: seq, stamp: at(sec), delivered: delivered})
	}
	heartbeat := func(seq uint64, sec int, delivered uint64) {
		s.handleWorkerMessage(&fakeJSMsg{
			subject: bus.WorkerHeartbeatSubject("w-ghost"), natsSeq: seq, stamp: at(sec), delivered: delivered,
			data: workerMsgJSON(t, protocol.HeartbeatMsg{Version: protocol.ProtocolVersion, WorkerID: "w-ghost", At: at(sec)}),
		})
	}

	marker(10, 0, 1)
	heartbeat(5, 1, 2)  // a redelivery of an earlier message
	heartbeat(11, 1, 1) // the next first delivery
	heartbeat(40, 2, 3) // a redelivery from further on
	marker(12, 2, 1)
	if got := s.ingestWorker.effective(at(3)); !got.Equal(at(2)) {
		t.Fatalf("effective with no skip = %v, want %v (redeliveries are not skips)", got, at(2))
	}
	if strings.Contains(logs.String(), "expired") {
		t.Fatalf("a skip was logged with none:\n%s", logs.String())
	}

	// 13 and 14 were removed unread; the marker at 15 reveals it.
	marker(15, 150, 1)
	marker(16, 152, 1)
	if got := s.ingestWorker.effective(at(153)); !got.Equal(at(2)) {
		t.Fatalf("effective after a skip = %v, want %v (the marker before it)", got, at(2))
	}
	if !strings.Contains(logs.String(), "scheduler: worker messages expired from the stream unread") {
		t.Errorf("the skip was not logged:\n%s", logs.String())
	}
	marker(17, 150+int(DefaultConfig().WorkerTimeout/time.Second), 1)
	if got, want := s.ingestWorker.effective(at(200)), at(150+int(DefaultConfig().WorkerTimeout/time.Second)); !got.Equal(want) {
		t.Fatalf("effective a WorkerTimeout past the skip = %v, want %v (released)", got, want)
	}
	if got := s.ingestTask.effective(at(200)); !got.IsZero() {
		t.Errorf("task tracker effective = %v, want zero (a worker-stream skip is the worker tracker's)", got)
	}
}

// TestHandleWorkerMessage_FirstDeliveryAfterStartIsNoSkip: the first message a
// fresh process reads sets the sequence it counts from, however high it is.
func TestHandleWorkerMessage_FirstDeliveryAfterStartIsNoSkip(t *testing.T) {
	s := newIngestScheduler(DefaultConfig(), &recordBus{}, &bytes.Buffer{})
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s.handleWorkerMessage(&fakeJSMsg{subject: bus.SubjectMarkerWorker, natsSeq: 5000, stamp: t0, delivered: 1})
	s.handleWorkerMessage(&fakeJSMsg{subject: bus.SubjectMarkerWorker, natsSeq: 5001, stamp: t0.Add(time.Second), delivered: 1})
	if got := s.ingestWorker.effective(t0.Add(2 * time.Second)); !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("effective = %v, want %v", got, t0.Add(time.Second))
	}
}
