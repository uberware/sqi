// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Lifecycle test for Run/Stop and the background loops (the lease subscriber
// and runHeartbeatSweep). The recordBus stub returns a nil
// jetstream.ConsumeContext with no error from every Consume* call and a nil
// subscription from SubscribeLease, so Run can start its consumers without a
// real NATS broker. The loops are driven to their ctx.Done() exit paths by
// canceling the parent context — no shared scheduler state is read from the
// test goroutine, so the run is race-clean.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

func TestRun_StartAndStop(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	// Tiny interval so the heartbeat-sweep ticker (which also refreshes the
	// metric gauges) fires at least once before the context is canceled,
	// exercising the loop body as well as the exit path.
	s.cfg.HeartbeatSweepInterval = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give the loops a brief window to tick, then cancel and confirm clean exit.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestStop_NilCancel_NoPanic(_ *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")
	// s.cancel is nil before Run; Stop must be a safe no-op.
	s.Stop()
}

func TestStop_WithCancel_CancelsContext(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "")

	// White-box wire a cancel func as Run would, then verify Stop invokes it.
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.Stop()

	select {
	case <-ctx.Done():
		// canceled as expected
	default:
		t.Error("Stop did not cancel the scheduler context")
	}
}

// syncBus is a recordBus that hands the test the lease handler Run subscribes.
// Run subscribes it after it has set its context and start time and run the
// start-up repairs, so receiving the handler also orders those writes before
// anything the test does next. It counts worker.diag subscriptions, failing
// them with diagErr when set.
type syncBus struct {
	recordBus

	leaseHandler chan func(workerID, queueID string, data []byte) []byte
	diagSubs     atomic.Int32
	diagErr      error
}

func newSyncBus() *syncBus {
	return &syncBus{leaseHandler: make(chan func(string, string, []byte) []byte, 1)}
}

func (b *syncBus) SubscribeLease(h func(string, string, []byte) []byte) (*nats.Subscription, error) {
	b.leaseHandler <- h
	return nil, nil
}

func (b *syncBus) SubscribeWorkerDiag(_ func(subject string, data []byte)) (*nats.Subscription, error) {
	if b.diagErr != nil {
		return nil, b.diagErr
	}
	b.diagSubs.Add(1)
	return nil, nil
}

// startRun runs s on its own goroutine until Run has subscribed to leases and
// returns the lease handler and a stop function that stops s and waits for Run
// to return.
func startRun(t *testing.T, s *Scheduler, b *syncBus, ctx context.Context) (
	handler func(workerID, queueID string, data []byte) []byte, stop func(),
) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case handler = <-b.leaseHandler:
	case err := <-done:
		t.Fatalf("Run returned before subscribing to leases: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run never subscribed to leases")
	}
	var stopped bool
	stop = func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		s.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after Stop")
		}
	}
	t.Cleanup(stop)
	return handler, stop
}

// TestStop_ParkedLeaseAnsweredEmpty pins that a lease request parked
// waiting for work is answered, empty, as soon as the scheduler stops, rather
// than when its hold runs out (leaseHoldTimeout, 30 s), so a worker is never
// left waiting on a server that has stepped down.
func TestStop_ParkedLeaseAnsweredEmpty(t *testing.T) {
	st := newCheckedFake(t)
	w, _ := seedLeaseFixture(t, st, nil) // nothing ready: the request parks
	b := newSyncBus()
	s := newMetricsScheduler(st, b, "f1")
	handler, stop := startRun(t, s, b, t.Context())

	replies := make(chan []byte, 1)
	go func() {
		replies <- handler(w.ID, bus.WildcardQueueToken, leaseRequestBody(t, w.ID))
	}()
	waitParked(t, s, bus.WildcardQueueToken)

	stopped := time.Now()
	s.Stop()
	select {
	case raw := <-replies:
		if elapsed := time.Since(stopped); elapsed > 100*time.Millisecond {
			t.Errorf("the parked request was answered %v after Stop, want within 100ms", elapsed)
		}
		var reply leaseReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("unmarshal reply %s: %v", raw, err)
		}
		if len(reply.Assignments) != 0 {
			t.Errorf("reply carries %d assignments, want an empty batch", len(reply.Assignments))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("the parked request was not answered within 100ms of Stop")
	}
	stop()
}

func TestStartDiag_BeforeRunSubscribesOnce(t *testing.T) {
	newDiagScheduler := func(t *testing.T, b *syncBus, buf *diag.Buffer) *Scheduler {
		t.Helper()
		return New(DefaultConfig(), newCheckedFake(t), b, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, buf)
	}

	t.Run("StartDiag then Run", func(t *testing.T) {
		b := newSyncBus()
		s := newDiagScheduler(t, b, diag.NewBuffer(10, nil))
		for range 2 {
			if err := s.StartDiag(); err != nil {
				t.Fatalf("StartDiag: %v", err)
			}
		}
		if n := b.diagSubs.Load(); n != 1 {
			t.Fatalf("after StartDiag: %d worker.diag subscriptions, want 1", n)
		}
		_, stop := startRun(t, s, b, t.Context())
		stop()
		if n := b.diagSubs.Load(); n != 1 {
			t.Errorf("after Run: %d worker.diag subscriptions, want 1 (Run must not subscribe again)", n)
		}
	})

	t.Run("Run alone", func(t *testing.T) {
		b := newSyncBus()
		s := newDiagScheduler(t, b, diag.NewBuffer(10, nil))
		_, stop := startRun(t, s, b, t.Context())
		stop()
		if n := b.diagSubs.Load(); n != 1 {
			t.Errorf("%d worker.diag subscriptions, want 1", n)
		}
	})

	t.Run("diagnostics disabled", func(t *testing.T) {
		b := newSyncBus()
		s := newDiagScheduler(t, b, nil)
		if err := s.StartDiag(); err != nil {
			t.Fatalf("StartDiag: %v", err)
		}
		_, stop := startRun(t, s, b, t.Context())
		stop()
		if n := b.diagSubs.Load(); n != 0 {
			t.Errorf("%d worker.diag subscriptions, want none", n)
		}
	})

	t.Run("a failed StartDiag is retried by Run", func(t *testing.T) {
		b := newSyncBus()
		b.diagErr = errors.New("broker unreachable")
		s := newDiagScheduler(t, b, diag.NewBuffer(10, nil))
		if err := s.StartDiag(); !errors.Is(err, b.diagErr) {
			t.Fatalf("StartDiag = %v, want the subscribe error", err)
		}
		b.diagErr = nil
		_, stop := startRun(t, s, b, t.Context())
		stop()
		if n := b.diagSubs.Load(); n != 1 {
			t.Errorf("%d worker.diag subscriptions, want 1 (Run subscribes once StartDiag failed)", n)
		}
	})
}
