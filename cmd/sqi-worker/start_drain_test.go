// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"sync"
	"testing"
	"time"
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

func (r *shutdownRecorder) Deregister(reason string) {
	r.mu.Lock()
	r.reason = reason
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
		c, k := shutdownSequence(rec, rec, rec, grace)
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
}
