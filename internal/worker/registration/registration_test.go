// SPDX-License-Identifier: AGPL-3.0-only

package registration_test

// Tests for registration.go, specifically the LastRegisteredAt behavior
// introduced in the heartbeat tasks (30-33).
//
// These tests start a lightweight embedded NATS server (no JetStream streams
// required) so that Register can complete a real Publish. This matches the
// pattern used in internal/bus where nats-server/v2 is already a dep.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/registration"
)

// ── embedded NATS helper ──────────────────────────────────────────────────────

// startTestNATS starts a plain (no JetStream) embedded NATS server on a random
// loopback port and returns the client URL. The server is shut down when tb
// calls t.Cleanup.
func startTestNATS(tb testing.TB) string {
	tb.Helper()

	port := freePort(tb)
	opts := &natsserver.Options{
		Host:           "127.0.0.1",
		Port:           port,
		NoLog:          true,
		NoSigs:         true,
		MaxControlLine: 4096,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		tb.Fatalf("startTestNATS: NewServer: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		tb.Fatal("startTestNATS: server did not become ready within 5s")
	}
	tb.Cleanup(func() { ns.Shutdown() })
	return fmt.Sprintf("nats://127.0.0.1:%d", port)
}

// freePort asks the OS for an available TCP port on loopback.
func freePort(tb testing.TB) int {
	tb.Helper()
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("freePort: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if closeErr := ln.Close(); closeErr != nil {
		tb.Fatalf("freePort: close listener: %v", closeErr)
	}
	if !ok {
		tb.Fatalf("freePort: unexpected address type %T", ln.Addr())
	}
	return tcpAddr.Port
}

// connectNATS opens a *nats.Conn to url and registers cleanup.
func connectNATS(tb testing.TB, url string) *nats.Conn {
	tb.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		tb.Fatalf("connectNATS: %v", err)
	}
	tb.Cleanup(func() { nc.Close() })
	return nc
}

// minimalCfg returns a WorkerSettings with sensible defaults for tests.
func minimalCfg() workerconfig.WorkerSettings {
	return workerconfig.WorkerSettings{
		Name:               "test-worker",
		MaxConcurrentTasks: 1,
		HeartbeatInterval:  15 * time.Second,
	}
}

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 99}))
}

// ── LastRegisteredAt ──────────────────────────────────────────────────────────

// TestLastRegisteredAt_ZeroBeforeRegister checks that LastRegisteredAt returns
// the zero time when Register has never been called.
func TestLastRegisteredAt_ZeroBeforeRegister(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := registration.New(nc, "worker-1", minimalCfg(), capabilities.Capabilities{OS: "linux"}, discardLogger())

	if !reg.LastRegisteredAt().IsZero() {
		t.Errorf("LastRegisteredAt() = %v before any Register call; want zero time", reg.LastRegisteredAt())
	}
}

// TestLastRegisteredAt_UpdatedAfterRegister checks that a successful Register
// call updates LastRegisteredAt to a recent timestamp.
func TestLastRegisteredAt_UpdatedAfterRegister(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := registration.New(nc, "worker-1", minimalCfg(), capabilities.Capabilities{OS: "linux"}, discardLogger())

	before := time.Now()
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	after := time.Now()

	got := reg.LastRegisteredAt()
	if got.IsZero() {
		t.Fatal("LastRegisteredAt() is zero after successful Register; want a recent time")
	}
	if got.Before(before) || got.After(after) {
		t.Errorf("LastRegisteredAt() = %v, want between %v and %v", got, before, after)
	}
}

// TestLastRegisteredAt_MonotonicallyIncreases checks that successive Register
// calls advance LastRegisteredAt.
func TestLastRegisteredAt_MonotonicallyIncreases(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := registration.New(nc, "worker-1", minimalCfg(), capabilities.Capabilities{OS: "linux"}, discardLogger())

	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	first := reg.LastRegisteredAt()

	// Small sleep to ensure the clock advances.
	time.Sleep(2 * time.Millisecond)

	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	second := reg.LastRegisteredAt()

	if !second.After(first) {
		t.Errorf("second LastRegisteredAt (%v) not after first (%v)", second, first)
	}
}
