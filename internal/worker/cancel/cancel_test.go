// SPDX-License-Identifier: AGPL-3.0-or-later

package cancel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
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

// stubCanceler records every Cancel call and always returns true (found).
type stubCanceler struct {
	mu       sync.Mutex
	canceled []string
}

func (s *stubCanceler) Cancel(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled = append(s.canceled, taskID)
	return true
}

func (s *stubCanceler) canceledTasks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.canceled))
	copy(out, s.canceled)
	return out
}

// stubCancelerNotFound always returns false (task not found / already finished).
type stubCancelerNotFound struct {
	calls atomic.Int32
}

func (s *stubCancelerNotFound) Cancel(_ string) bool {
	s.calls.Add(1)
	return false
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func cancelPayload(t *testing.T, taskID, jobID string) []byte {
	t.Helper()
	b, err := json.Marshal(protocol.TaskCancelMsg{
		TaskID:     taskID,
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

// TestHandler_Register_and_receive verifies that a cancel message published to
// task.cancel.<taskID> after Register is delivered to TaskCanceler.Cancel
// (task 80, 81).
func TestHandler_Register_and_receive(t *testing.T) {
	url := startTestNATSServer(t)
	nc := connectTestNATS(t, url)

	c := &stubCanceler{}
	h := cancel.New(nc, c, testLogger())

	taskID := "task-001"
	if err := h.Register(taskID); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := nc.Publish(bus.TaskCancelSubject(taskID), cancelPayload(t, taskID, "job-1")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return len(c.canceledTasks()) > 0
	})

	tasks := c.canceledTasks()
	if len(tasks) != 1 || tasks[0] != taskID {
		t.Errorf("Cancel called with %v; want [%q]", tasks, taskID)
	}
}

// TestHandler_Deregister_stops_delivery verifies that messages published after
// Deregister are not delivered to the TaskCanceler (task 80).
func TestHandler_Deregister_stops_delivery(t *testing.T) {
	url := startTestNATSServer(t)
	nc := connectTestNATS(t, url)

	c := &stubCanceler{}
	h := cancel.New(nc, c, testLogger())

	taskID := "task-002"
	if err := h.Register(taskID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.Deregister(taskID)

	if err := nc.Publish(bus.TaskCancelSubject(taskID), cancelPayload(t, taskID, "job-2")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Wait briefly to confirm no delivery.
	time.Sleep(150 * time.Millisecond)
	if tasks := c.canceledTasks(); len(tasks) != 0 {
		t.Errorf("Cancel called %d time(s) after Deregister; want 0", len(tasks))
	}
}

// TestHandler_multiple_tasks verifies correct per-task routing when several
// tasks are registered simultaneously (task 81).
func TestHandler_multiple_tasks(t *testing.T) {
	url := startTestNATSServer(t)
	nc := connectTestNATS(t, url)

	c := &stubCanceler{}
	h := cancel.New(nc, c, testLogger())

	for _, id := range []string{"task-A", "task-B", "task-C"} {
		if err := h.Register(id); err != nil {
			t.Fatalf("Register %q: %v", id, err)
		}
	}

	// Cancel only task-B.
	if err := nc.Publish(bus.TaskCancelSubject("task-B"), cancelPayload(t, "task-B", "job-X")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return len(c.canceledTasks()) > 0
	})

	tasks := c.canceledTasks()
	if len(tasks) != 1 {
		t.Errorf("Cancel called %d time(s); want exactly 1", len(tasks))
	}
	if len(tasks) > 0 && tasks[0] != "task-B" {
		t.Errorf("Cancel called for %q; want %q", tasks[0], "task-B")
	}
}

// TestHandler_task_not_found verifies that a cancel signal for an already-
// finished task is handled silently without panicking (task 81).
func TestHandler_task_not_found(t *testing.T) {
	url := startTestNATSServer(t)
	nc := connectTestNATS(t, url)

	c := &stubCancelerNotFound{}
	h := cancel.New(nc, c, testLogger())

	taskID := "task-done"
	if err := h.Register(taskID); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := nc.Publish(bus.TaskCancelSubject(taskID), cancelPayload(t, taskID, "job-Z")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Cancel should be called (returning false) without panicking.
	waitFor(t, 2*time.Second, func() bool {
		return c.calls.Load() > 0
	})
}

// TestHandler_malformed_payload verifies that a malformed cancel message does
// not panic and does not invoke TaskCanceler.Cancel (task 80).
func TestHandler_malformed_payload(t *testing.T) {
	url := startTestNATSServer(t)
	nc := connectTestNATS(t, url)

	c := &stubCanceler{}
	h := cancel.New(nc, c, testLogger())

	taskID := "task-bad"
	if err := h.Register(taskID); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := nc.Publish(bus.TaskCancelSubject(taskID), []byte("not json {{{")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	if tasks := c.canceledTasks(); len(tasks) != 0 {
		t.Errorf("Cancel called on malformed payload; want 0 calls, got %d", len(tasks))
	}
}

// TestHandler_deregister_idempotent verifies that calling Deregister multiple
// times does not panic (task 80).
func TestHandler_deregister_idempotent(t *testing.T) {
	url := startTestNATSServer(t)
	nc := connectTestNATS(t, url)

	c := &stubCanceler{}
	h := cancel.New(nc, c, testLogger())

	taskID := "task-idem"
	if err := h.Register(taskID); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Must not panic.
	h.Deregister(taskID)
	h.Deregister(taskID)
	h.Deregister(taskID)
}
