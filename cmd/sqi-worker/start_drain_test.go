// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// shutdownRecorder is the three collaborators of shutdownSequence in one
// value, so a test reads the order they were called in from a single log.
type shutdownRecorder struct {
	mu      sync.Mutex
	events  []string
	release chan struct{} // closed to let DrainAndShutdown return
	started chan struct{} // closed when DrainAndShutdown is entered
	grace   time.Duration
	reason  string
	kind    protocol.DeregisterKind
}

func newShutdownRecorder() *shutdownRecorder {
	return &shutdownRecorder{release: make(chan struct{}), started: make(chan struct{})}
}

func (r *shutdownRecorder) record(ev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *shutdownRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *shutdownRecorder) DrainAndShutdown(grace time.Duration) (completed, killed int) {
	r.mu.Lock()
	r.grace = grace
	r.mu.Unlock()
	close(r.started)
	r.record("drain-start")
	<-r.release
	r.record("drain-end")
	return 3, 1
}

func (r *shutdownRecorder) Stop() { r.record("heartbeat-stop") }

func (r *shutdownRecorder) Deregister(reason string, kind protocol.DeregisterKind) {
	r.mu.Lock()
	r.reason = reason
	r.kind = kind
	r.mu.Unlock()
	r.record("deregister")
}

// TestShutdownSequence_HeartbeatsUntilDrainEnds pins the shutdown order: the
// heartbeat keeps running for the whole drain and stops only after the drain
// returns, then the worker deregisters. A heartbeat stopped when the drain
// begins lets the server declare a draining worker offline and reclaim the
// tasks it is still running.
func TestShutdownSequence_HeartbeatsUntilDrainEnds(t *testing.T) {
	const grace = 20 * time.Second
	rec := newShutdownRecorder()

	type result struct{ completed, killed int }
	done := make(chan result, 1)
	go func() {
		c, k := shutdownSequence(rec, rec, rec, grace, "")
		done <- result{c, k}
	}()

	select {
	case <-rec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdownSequence never started the drain")
	}

	// While the drain is blocked, nothing after it may have happened.
	time.Sleep(50 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 1 || got[0] != "drain-start" {
		t.Fatalf("events during the drain = %v, want only [drain-start]: the heartbeat must keep running and the worker must not deregister", got)
	}

	close(rec.release)
	select {
	case res := <-done:
		if res.completed != 3 || res.killed != 1 {
			t.Errorf("shutdownSequence returned (%d, %d), want the drain's (3, 1)", res.completed, res.killed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdownSequence did not return after the drain was released")
	}

	want := []string{"drain-start", "drain-end", "heartbeat-stop", "deregister"}
	got := rec.snapshot()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	if rec.grace != grace {
		t.Errorf("drain grace = %s, want %s", rec.grace, grace)
	}
	if rec.reason != "graceful shutdown" {
		t.Errorf("deregister reason = %q, want %q", rec.reason, "graceful shutdown")
	}
	if rec.kind != "" {
		t.Errorf("deregister kind = %q, want empty: a plain shutdown is a restart", rec.kind)
	}
}

// TestShutdownSequence_PassesKindToDeregister pins that the departure kind
// reaches the deregistration, after the drain like any other shutdown.
func TestShutdownSequence_PassesKindToDeregister(t *testing.T) {
	rec := newShutdownRecorder()
	close(rec.release)
	shutdownSequence(rec, rec, rec, 0, protocol.DeregisterPreempted)
	if rec.kind != protocol.DeregisterPreempted {
		t.Errorf("deregister kind = %q, want %q", rec.kind, protocol.DeregisterPreempted)
	}
	if rec.grace != 0 {
		t.Errorf("drain grace = %s, want 0", rec.grace)
	}
}

// TestShutdownPlan pins what each shutdown trigger drains with and departs as.
// Only a provider's preemption notice skips the grace period and tells the
// server the machine is gone; a signal, a service stop and a lost NATS
// connection are all restarts.
func TestShutdownPlan(t *testing.T) {
	const configured = 45 * time.Second
	tests := []struct {
		name      string
		cause     error
		wantGrace time.Duration
		wantKind  protocol.DeregisterKind
	}{
		{"preemption notice", errPreempted, 0, protocol.DeregisterPreempted},
		{"wrapped preemption", fmt.Errorf("watcher: %w", errPreempted), 0, protocol.DeregisterPreempted},
		{"signal", errors.New("interrupt"), configured, ""},
		{"service stop", errors.New("service stop"), configured, ""},
		{"nats loss (no cause)", context.Canceled, configured, ""},
		{"nil cause", nil, configured, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grace, kind := shutdownPlan(tt.cause, configured)
			if grace != tt.wantGrace || kind != tt.wantKind {
				t.Errorf("shutdownPlan(%v) = (%s, %q), want (%s, %q)", tt.cause, grace, kind, tt.wantGrace, tt.wantKind)
			}
		})
	}
}

// TestPreemptionCause_FirstTriggerWins pins that shutdown is idempotent
// across triggers: the cause a worker's context carries is whichever canceled
// it first, so a signal arriving after a preemption notice does not turn it
// into a restart, and a notice arriving mid-restart does not turn the restart
// into a preemption.
func TestPreemptionCause_FirstTriggerWins(t *testing.T) {
	signal := errors.New("terminated")

	t.Run("notice then signal", func(t *testing.T) {
		parent, signalCancel := context.WithCancelCause(context.Background())
		ctx, cancel := context.WithCancelCause(parent)
		cancel(errPreempted)
		signalCancel(signal)
		if got := context.Cause(ctx); !errors.Is(got, errPreempted) {
			t.Errorf("cause = %v, want preempted", got)
		}
	})
	t.Run("signal then notice", func(t *testing.T) {
		parent, signalCancel := context.WithCancelCause(context.Background())
		ctx, cancel := context.WithCancelCause(parent)
		signalCancel(signal)
		cancel(errPreempted)
		if got := context.Cause(ctx); !errors.Is(got, signal) {
			t.Errorf("cause = %v, want the signal", got)
		}
	})
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestStartPreemptionWatch_NoticeCancelsWithCause drives the watcher wiring
// end to end against a fake metadata server: a notice cancels the worker's
// context with the preempted cause, which shutdownPlan turns into grace 0.
func TestStartPreemptionWatch_NoticeCancelsWithCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "TRUE"); err != nil {
			return
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cfg := workerconfig.PreemptionConfig{Watch: "gcp", PollInterval: time.Second, Endpoint: srv.URL}
	if err := startPreemptionWatch(ctx, cfg, discardLogger(), cancel); err != nil {
		t.Fatalf("startPreemptionWatch: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a preemption notice did not cancel the worker context")
	}
	grace, kind := shutdownPlan(context.Cause(ctx), 30*time.Second)
	if grace != 0 || kind != protocol.DeregisterPreempted {
		t.Errorf("plan after a notice = (%s, %q), want (0, preempted)", grace, kind)
	}
}

// TestStartPreemptionWatch_NoneReachesNoEndpoint pins that with the default
// watch: none nothing is created and no metadata endpoint is ever contacted.
func TestStartPreemptionWatch_NoneReachesNoEndpoint(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cfg := workerconfig.Default().Worker.Preemption
	cfg.Endpoint = srv.URL
	if err := startPreemptionWatch(ctx, cfg, discardLogger(), cancel); err != nil {
		t.Fatalf("startPreemptionWatch: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if hits.Load() != 0 || ctx.Err() != nil {
		t.Errorf("watch none contacted the endpoint %d times (ctx err %v), want no activity", hits.Load(), ctx.Err())
	}
}
