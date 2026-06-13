// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Lifecycle test for Run/Stop and the three background loops (runDispatchLoop,
// runHeartbeatSweep, runAssignWorker). The recordBus stub returns a nil
// jetstream.ConsumeContext with no error from every Consume* call so Run can
// start its consumers without a real NATS broker. The loops are driven to their
// ctx.Done() / channel-close exit paths by canceling the parent context — no
// shared scheduler state is read from the test goroutine, so the run is
// race-clean.

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/fake"
)

func TestRun_StartAndStop(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	// Tiny intervals so the dispatch/sweep tickers fire at least once before the
	// context is canceled, exercising the loop bodies as well as the exit paths.
	s.cfg.AssignInterval = time.Millisecond
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
	st := fake.New()
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
