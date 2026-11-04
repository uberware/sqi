// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/worker/protocol"
)

const oneTaskTemplate = `specificationVersion: "jobtemplate-2023-09"
name: x-1
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: noop
`

// running reports whether the worker has a live process.
func (w *Worker) running() bool {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	return w.cur != nil
}

// startInternal starts a queue-less worker on a fresh in-process server and
// returns it with the server's client and farm.
func startInternal(t *testing.T, plan TaskPlan, l *Ledger) (*Worker, *client.Client, string) {
	t.Helper()
	srv := testserver.Start(t, testserver.Options{})
	c := client.New(srv.HTTPURL())
	farmID, err := c.CreateFarm(t.Context(), "internal-farm")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	w := NewWorker(WorkerSpec{
		ID: uuid.NewString(), Hostname: "sim-internal", FarmID: farmID,
		Group:   scenario.WorkerGroup{CPU: 4, OS: "linux"},
		NATSURL: srv.NATSURL(),
	}, BehaviorFunc(func(*protocol.AssignMsg, *rand.Rand) TaskPlan { return plan }), l, slog.New(slog.DiscardHandler))
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return w, c, farmID
}

func TestDisconnect_RecoversFromAFailedReregistration(t *testing.T) {
	w, _, _ := startInternal(t, TaskPlan{Duration: time.Millisecond}, NewLedger())

	// Fail the next two registrations: the reconnect handler's, and whichever
	// comes next (the heartbeat watchdog's fallback or Disconnect's own).
	var calls, failures atomic.Int64
	fault := func() error {
		if calls.Add(1) <= 2 {
			failures.Add(1)
			return errors.New("injected: stream ack timed out")
		}
		return nil
	}
	w.registerFault.Store(&fault)

	before := w.InstanceID()
	start := time.Now()
	if err := w.Disconnect(t.Context()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if elapsed := time.Since(start); elapsed > disconnectBound {
		t.Errorf("Disconnect took %v, more than its bound %v", elapsed, disconnectBound)
	}
	if failures.Load() < 1 {
		t.Error("no registration failed: the fault was never exercised")
	}
	if w.InstanceID() != before {
		t.Errorf("InstanceID changed across Disconnect: %q -> %q", before, w.InstanceID())
	}
	// The worker is not wedged: the lifecycle lock is free and it still works.
	if !w.running() {
		t.Error("worker is not running after Disconnect")
	}
}

func TestDisconnect_GivesUpAtItsBound(t *testing.T) {
	w, _, _ := startInternal(t, TaskPlan{Duration: time.Millisecond}, NewLedger())
	fault := func() error { return errors.New("injected: always fails") }
	w.registerFault.Store(&fault)

	// A short caller deadline stands in for disconnectBound, which shares the
	// same path.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := w.Disconnect(ctx)
	if err == nil {
		t.Fatal("Disconnect succeeded with every registration failing")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("Disconnect returned after %v, want about its 3s deadline", elapsed)
	}
	w.registerFault.Store(nil)
	if !w.running() {
		t.Error("a failed Disconnect stopped the worker; it should keep running")
	}
}

func TestWatchClosed_PermanentCloseIsACrash(t *testing.T) {
	l := NewLedger()
	w, c, farmID := startInternal(t, TaskPlan{Outcome: OutcomeHang, LogLines: 1}, l)
	queueID, err := c.CreateQueue(t.Context(), farmID, "q", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	// Not driver.BuildTemplate: driver imports this package, so an in-package
	// test cannot import it back.
	tmpl := []byte(oneTaskTemplate)
	if _, _, err := c.SubmitJob(t.Context(), client.SubmitRequest{FarmID: farmID, QueueID: queueID, Template: tmpl}); err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for countKindIn(l, KindRunning) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("task never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Close the connection behind the worker's back, as natsclient does when
	// it gives up on the broker.
	w.opMu.Lock()
	nc := w.cur.nc
	w.opMu.Unlock()
	nc.Close()

	for w.running() {
		if time.Now().After(deadline) {
			t.Fatal("worker still running on a closed connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if countKindIn(l, KindCrashed) != 1 {
		t.Errorf("crashed events = %d, want 1 for the abandoned attempt", countKindIn(l, KindCrashed))
	}
	if err := w.Disconnect(t.Context()); !errors.Is(err, errNotRunning) {
		t.Errorf("Disconnect after the close = %v, want errNotRunning", err)
	}
	if err := w.Restart(t.Context()); err != nil {
		t.Fatalf("Restart after the close: %v", err)
	}
}
