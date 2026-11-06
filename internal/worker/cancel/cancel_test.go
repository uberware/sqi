// SPDX-License-Identifier: AGPL-3.0-or-later

package cancel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/cancel"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── NATS helpers ──────────────────────────────────────────────────────────────

// startTestNATSServer starts a plain (no JetStream) in-process NATS server on
// a random loopback port and returns the server URL. The server is shut down
// when t.Cleanup runs.
func startTestNATSServer(t *testing.T) string {
	t.Helper()
	port := freePort(t)
	opts := &natsserver.Options{
		Host:           "127.0.0.1",
		Port:           port,
		NoLog:          true,
		NoSigs:         true,
		MaxControlLine: 4096,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("startTestNATSServer: NewServer: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("startTestNATSServer: server not ready within 5s")
	}
	t.Cleanup(func() { ns.Shutdown() })
	return fmt.Sprintf("nats://127.0.0.1:%d", port)
}

// freePort returns a free TCP port on loopback.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("freePort: unexpected addr type %T", ln.Addr())
	}
	port := tcpAddr.Port
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatalf("freePort: close: %v", closeErr)
	}
	return port
}

// connectTestNATS opens a *nats.Conn to the given URL and registers cleanup.
func connectTestNATS(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connectTestNATS: %v", err)
	}
	t.Cleanup(func() { nc.Close() })
	return nc
}

// ── Stubs ─────────────────────────────────────────────────────────────────────

// cancelCall is one Cancel call a stubCanceler saw.
type cancelCall struct{ taskID, attemptID string }

// stubCanceler records every Cancel call and always returns true (found).
type stubCanceler struct {
	mu    sync.Mutex
	calls []cancelCall
}

func (s *stubCanceler) Cancel(taskID, attemptID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, cancelCall{taskID, attemptID})
	return true
}

func (s *stubCanceler) cancelCalls() []cancelCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cancelCall(nil), s.calls...)
}

func (s *stubCanceler) canceledTasks() []string {
	var out []string
	for _, c := range s.cancelCalls() {
		out = append(out, c.taskID)
	}
	return out
}

// stubCancelerNotFound always returns false (task not found / already finished).
type stubCancelerNotFound struct {
	calls atomic.Int32
}

func (s *stubCancelerNotFound) Cancel(_, _ string) bool {
	s.calls.Add(1)
	return false
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func cancelPayload(t *testing.T, taskID, jobID string) []byte {
	t.Helper()
	return attemptCancelPayload(t, taskID, "", jobID)
}

func attemptCancelPayload(t *testing.T, taskID, attemptID, jobID string) []byte {
	t.Helper()
	b, err := json.Marshal(protocol.TaskCancelMsg{
		TaskID:     taskID,
		AttemptID:  attemptID,
		JobID:      jobID,
		CanceledAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("marshal cancel payload: %v", err)
	}
	return b
}

// waitFor polls cond up to timeout and fails the test if cond never returns true.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true within timeout")
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// subscribe subscribes canceler as workerID on nc, failing the test on error.
func subscribe(t *testing.T, nc *nats.Conn, workerID string, canceler cancel.TaskCanceler) *nats.Subscription {
	t.Helper()
	sub, err := cancel.Subscribe(nc, workerID, canceler, testLogger())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return sub
}

// publish publishes payload to workerID's cancel subject on nc and flushes.
func publish(t *testing.T, nc *nats.Conn, workerID string, payload []byte) {
	t.Helper()
	if err := nc.Publish(bus.TaskCancelSubject(workerID), payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// TestSubscribe_delivers_cancels verifies that a cancel published to the
// worker's subject reaches TaskCanceler.Cancel with the task and attempt it
// names: a task-wide cancel names no attempt, and a cancel for an attempt the
// server has no record of names no task.
func TestSubscribe_delivers_cancels(t *testing.T) {
	nc := connectTestNATS(t, startTestNATSServer(t))
	c := &stubCanceler{}
	subscribe(t, nc, "worker-1", c)

	publish(t, nc, "worker-1", cancelPayload(t, "task-1", "job-1"))
	publish(t, nc, "worker-1", attemptCancelPayload(t, "task-2", "attempt-2", "job-1"))
	publish(t, nc, "worker-1", attemptCancelPayload(t, "", "attempt-3", ""))

	waitFor(t, 2*time.Second, func() bool { return len(c.cancelCalls()) >= 3 })
	want := []cancelCall{{"task-1", ""}, {"task-2", "attempt-2"}, {"", "attempt-3"}}
	if got := c.cancelCalls(); !slices.Equal(got, want) {
		t.Errorf("Cancel calls = %+v, want %+v", got, want)
	}
}

// TestSubscribe_live_on_return verifies that the subscription is in place on
// the server when Subscribe returns: a cancel another connection publishes
// straight afterwards is delivered. That is what lets the worker subscribe
// before its first lease request and miss no cancel for a lease it takes.
func TestSubscribe_live_on_return(t *testing.T) {
	url := startTestNATSServer(t)
	nc, server := connectTestNATS(t, url), connectTestNATS(t, url)
	c := &stubCanceler{}
	subscribe(t, nc, "worker-1", c)

	publish(t, server, "worker-1", cancelPayload(t, "task-1", "job-1"))

	waitFor(t, 2*time.Second, func() bool { return len(c.cancelCalls()) == 1 })
}

// TestSubscribe_only_own_worker verifies that a worker receives cancels sent
// to it and none sent to another worker.
func TestSubscribe_only_own_worker(t *testing.T) {
	nc := connectTestNATS(t, startTestNATSServer(t))
	mine, theirs := &stubCanceler{}, &stubCanceler{}
	subscribe(t, nc, "worker-1", mine)
	subscribe(t, nc, "worker-2", theirs)

	publish(t, nc, "worker-2", cancelPayload(t, "task-B", "job-X"))

	waitFor(t, 2*time.Second, func() bool { return len(theirs.cancelCalls()) == 1 })
	time.Sleep(50 * time.Millisecond) // give a misrouted cancel time to arrive
	if got := mine.canceledTasks(); len(got) != 0 {
		t.Errorf("worker-1 canceled %v on a cancel sent to worker-2", got)
	}
}

// TestSubscribe_unsubscribe_stops_delivery verifies that nothing is delivered
// after the subscription is unsubscribed.
func TestSubscribe_unsubscribe_stops_delivery(t *testing.T) {
	nc := connectTestNATS(t, startTestNATSServer(t))
	c := &stubCanceler{}
	sub := subscribe(t, nc, "worker-1", c)
	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	publish(t, nc, "worker-1", cancelPayload(t, "task-1", "job-2"))

	time.Sleep(150 * time.Millisecond)
	if tasks := c.canceledTasks(); len(tasks) != 0 {
		t.Errorf("Cancel called %d time(s) after Unsubscribe; want 0", len(tasks))
	}
}

// TestSubscribe_task_not_found verifies that a cancel for a run that already
// finished is handled quietly.
func TestSubscribe_task_not_found(t *testing.T) {
	nc := connectTestNATS(t, startTestNATSServer(t))
	c := &stubCancelerNotFound{}
	subscribe(t, nc, "worker-1", c)

	publish(t, nc, "worker-1", cancelPayload(t, "task-done", "job-Z"))

	waitFor(t, 2*time.Second, func() bool { return c.calls.Load() > 0 })
}

// TestSubscribe_malformed_payload verifies that a cancel message that cannot
// be parsed, or names neither a task nor an attempt, never reaches
// TaskCanceler.Cancel.
func TestSubscribe_malformed_payload(t *testing.T) {
	nc := connectTestNATS(t, startTestNATSServer(t))
	c := &stubCanceler{}
	subscribe(t, nc, "worker-1", c)

	publish(t, nc, "worker-1", []byte("not json {{{"))
	publish(t, nc, "worker-1", cancelPayload(t, "", "job-1"))

	time.Sleep(150 * time.Millisecond)
	if tasks := c.canceledTasks(); len(tasks) != 0 {
		t.Errorf("Cancel called on a malformed payload; want 0 calls, got %v", tasks)
	}
}

// TestApplies pins which runs a cancel stops: each name it gives must match,
// and a cancel that names nothing stops nothing.
func TestApplies(t *testing.T) {
	tests := []struct {
		task, attempt string
		want          bool
	}{
		{"t1", "", true},
		{"t1", "a1", true},
		{"", "a1", true},
		{"t2", "", false},
		{"t1", "a2", false},
		{"", "a2", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := cancel.Applies(tt.task, tt.attempt, "t1", "a1"); got != tt.want {
			t.Errorf("Applies(%q, %q) on run t1/a1 = %v, want %v", tt.task, tt.attempt, got, tt.want)
		}
	}
}
