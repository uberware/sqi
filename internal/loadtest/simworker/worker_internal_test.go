// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
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

// gatedTransport fails every report of a finished task until open is closed,
// then sends through inner, noting when the first such report was stored.
type gatedTransport struct {
	inner    status.Transport
	open     chan struct{}
	seq      *atomic.Int64
	storedAt atomic.Int64
	tries    atomic.Int64
}

func (g *gatedTransport) PublishAcked(ctx context.Context, subj string, data []byte, msgID string) error {
	if bytes.Contains(data, []byte(`"succeeded"`)) {
		g.tries.Add(1)
		select {
		case <-g.open:
		default:
			return errors.New("injected: stream ack timed out")
		}
	}
	err := g.inner.PublishAcked(ctx, subj, data, msgID)
	if err == nil && bytes.Contains(data, []byte(`"succeeded"`)) {
		g.storedAt.CompareAndSwap(0, g.seq.Add(1))
	}
	return err
}

// startGated starts a worker on srv whose reports of a finished task fail
// until the returned gate is opened, and returns once that task has finished
// and its terminal report is pending. seq orders the gate's events with a
// spy's.
func startGated(t *testing.T, srv *testserver.Server, workerID string, seq *atomic.Int64) (*Worker, *gatedTransport) {
	t.Helper()
	c := client.New(srv.HTTPURL())
	farmID, err := c.CreateFarm(t.Context(), "flush-farm")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queueID, err := c.CreateQueue(t.Context(), farmID, "q", 0)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	l := NewLedger()
	w := NewWorker(WorkerSpec{
		ID: workerID, Hostname: "sim-flush", FarmID: farmID,
		Group:   scenario.WorkerGroup{CPU: 4, OS: "linux"},
		NATSURL: srv.NATSURL(),
	}, BehaviorFunc(func(*protocol.AssignMsg, *rand.Rand) TaskPlan { return TaskPlan{Duration: time.Millisecond} }), l, slog.New(slog.DiscardHandler))

	gate := &gatedTransport{open: make(chan struct{}), seq: seq}
	wrap := func(inner status.Transport) status.Transport { gate.inner = inner; return gate }
	w.statusWrap.Store(&wrap)
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(w.Crash)
	if _, _, err := c.SubmitJob(t.Context(), client.SubmitRequest{FarmID: farmID, QueueID: queueID, Template: []byte(oneTaskTemplate)}); err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for countKindIn(l, KindTerminal) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("task never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w, gate
}

// TestWorker_StopFlushesStatusesBeforeDeregister pins that a graceful stop
// does not deregister while a finished task's terminal report is still unsent:
// the server would reclaim a task that has completed.
func TestWorker_StopFlushesStatusesBeforeDeregister(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	workerID := uuid.NewString()
	var seq atomic.Int64

	spy, err := nats.Connect(srv.NATSURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer spy.Close()
	var deregisteredAt atomic.Int64
	if _, err := spy.Subscribe(bus.WorkerDeregisterSubject(workerID), func(*nats.Msg) {
		deregisteredAt.CompareAndSwap(0, seq.Add(1))
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := spy.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	w, gate := startGated(t, srv, workerID, &seq)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	select {
	case <-done:
		t.Fatal("Stop returned while the terminal report was still unsent")
	case <-time.After(700 * time.Millisecond):
	}
	if deregisteredAt.Load() != 0 {
		t.Fatal("the worker deregistered while its terminal report was still unsent")
	}

	close(gate.open)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Stop did not return after the terminal report could be sent")
	}
	// The spy's handler runs on its own goroutine, so the deregister can still
	// be on its way to it when Stop returns.
	for deadline := time.Now().Add(5 * time.Second); deregisteredAt.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	stored, dereg := gate.storedAt.Load(), deregisteredAt.Load()
	if stored == 0 || dereg == 0 || stored > dereg {
		t.Errorf("terminal stored at %d, deregistered at %d, want both set with the terminal first", stored, dereg)
	}
}

// TestWorker_CrashDropsPendingStatusesAtOnce pins that a crash neither waits
// for an unsent terminal report nor leaves its retry running against the
// connection it closes.
func TestWorker_CrashDropsPendingStatusesAtOnce(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	var seq atomic.Int64
	w, gate := startGated(t, srv, uuid.NewString(), &seq)

	start := time.Now()
	w.Crash()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Crash took %v with a report pending, want it to drop the report at once", elapsed)
	}
	tries := gate.tries.Load()
	time.Sleep(1500 * time.Millisecond) // several retry intervals
	if got := gate.tries.Load(); got != tries {
		t.Errorf("the dropped report was tried %d more times after the crash, want none", got-tries)
	}
}
