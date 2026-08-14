// SPDX-License-Identifier: AGPL-3.0-or-later

package registration_test

// Tests for registration.go, specifically the LastRegisteredAt behavior
// introduced in the heartbeat tasks (30-33).
//
// These tests start a lightweight embedded NATS server with JetStream enabled
// so that Register can complete a real acked publish. This matches the pattern
// used in internal/bus where nats-server/v2 is already a dep.
//
// Register publishes with a JetStream ack, so the SQI_WORKER stream must exist
// before it can succeed: startTestNATS provisions it. Tests that exercise the
// boot race (worker publishing before the server has provisioned JetStream)
// use startTestNATSNoStream and create the stream themselves.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/registration"
)

// ── embedded NATS helpers ─────────────────────────────────────────────────────

// startTestNATS starts an embedded JetStream-enabled NATS server on a random
// loopback port, provisions the SQI_WORKER stream, and returns the client URL.
// The server is shut down when tb calls t.Cleanup.
func startTestNATS(tb testing.TB) string {
	tb.Helper()
	url := startTestNATSNoStream(tb)
	provisionWorkerStream(tb, url)
	return url
}

// startTestNATSNoStream is startTestNATS without the SQI_WORKER stream: it
// models a server whose JetStream streams are not provisioned yet.
func startTestNATSNoStream(tb testing.TB) string {
	tb.Helper()

	port := freePort(tb)
	ns := startNATSOnPort(tb, port)
	tb.Cleanup(ns.Shutdown)
	return fmt.Sprintf("nats://127.0.0.1:%d", port)
}

// provisionWorkerStream creates the SQI_WORKER stream on the server at url and
// fails the test if it cannot. Call it from the test goroutine only; a test that
// provisions the stream concurrently should use createWorkerStream.
func provisionWorkerStream(tb testing.TB, url string) {
	tb.Helper()
	if err := createWorkerStream(url); err != nil {
		tb.Fatalf("provisionWorkerStream: %v", err)
	}
}

// createWorkerStream creates the SQI_WORKER stream on the server at url,
// mirroring the subjects and retention policy the real server provisions in
// internal/bus. It uses its own short-lived connection so it is safe to call
// from a goroutine.
func createWorkerStream(url string) error {
	nc, err := nats.Connect(url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream.New: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: bus.StreamWorker,
		Subjects: []string{
			bus.SubjectWorkerRegister,
			bus.SubjectWorkerHeartbeat,
			bus.SubjectWorkerDeregister,
		},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
		Replicas:  1,
	}); err != nil {
		return fmt.Errorf("CreateStream: %w", err)
	}
	return nil
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
		Name:              "test-worker",
		HeartbeatInterval: 15 * time.Second,
	}
}

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 99}))
}

// newRegistrar builds a Registrar over nc, failing the test if construction
// errors (which only happens if the JetStream context cannot be created).
func newRegistrar(
	tb testing.TB,
	nc *nats.Conn,
	workerID string,
	cfg workerconfig.WorkerSettings,
	caps capabilities.Capabilities,
) *registration.Registrar {
	tb.Helper()
	// The zero ExprLimits normalizes to the worker's defaults, which is what
	// every test here that does not care about them wants advertised.
	// TestRegister_AdvertisesConfiguredExprLimits is the one that does care.
	reg, err := registration.New(nc, workerID, cfg, fmtres.ExprLimits{}, caps, discardLogger())
	if err != nil {
		tb.Fatalf("registration.New: %v", err)
	}
	return reg
}

// ── Durable registration ──────────────────────────────────────────────────────

// TestRegister_NoStream_ReturnsError asserts Register fails when the SQI_WORKER
// stream never appears, rather than reporting success for a message that no
// stream captured.
//
// A core-NATS publish to a JetStream subject with no stream behind it is
// silently discarded, which stranded workers permanently: the server never
// learned of them and their heartbeats were NAKed forever.
func TestRegister_NoStream_ReturnsError(t *testing.T) {
	url := startTestNATSNoStream(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-nostream", minimalCfg(), capabilities.Capabilities{OS: "linux"})

	// A deadline bounds the retry loop; without it Register would keep waiting
	// for the stream for its full internal budget.
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()

	if err := reg.Register(ctx); err == nil {
		t.Fatal("Register succeeded with no SQI_WORKER stream; want an error (the message would be silently dropped)")
	}
	if !reg.LastRegisteredAt().IsZero() {
		t.Error("LastRegisteredAt is set after a failed Register; want zero time")
	}
}

// TestRegister_RetriesUntilStreamProvisioned asserts Register rides out the boot
// race: a worker that connects while the server is still provisioning JetStream
// retries until the stream exists instead of dropping its registration.
func TestRegister_RetriesUntilStreamProvisioned(t *testing.T) {
	url := startTestNATSNoStream(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-race", minimalCfg(), capabilities.Capabilities{OS: "linux"})

	// Provision the stream shortly after Register starts, mirroring a server
	// that finishes JetStream setup just after the worker connects.
	streamErr := make(chan error, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		streamErr <- createWorkerStream(url)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := reg.Register(ctx); err != nil {
		t.Fatalf("Register did not retry until the stream was provisioned: %v", err)
	}
	if err := <-streamErr; err != nil {
		t.Fatalf("createWorkerStream: %v", err)
	}

	// The retried registration must be in the stream. A publish that raced the
	// stream's creation and was dropped would leave it empty.
	if got := streamMsgCount(t, nc); got != 1 {
		t.Errorf("SQI_WORKER holds %d messages after Register; want the retried registration (1)", got)
	}
	if reg.LastRegisteredAt().IsZero() {
		t.Error("LastRegisteredAt is zero after a successful retried Register")
	}
}

// streamMsgCount returns the number of messages held by the SQI_WORKER stream.
func streamMsgCount(tb testing.TB, nc *nats.Conn) uint64 {
	tb.Helper()

	js, err := jetstream.New(nc)
	if err != nil {
		tb.Fatalf("streamMsgCount: jetstream.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := js.Stream(ctx, bus.StreamWorker)
	if err != nil {
		tb.Fatalf("streamMsgCount: Stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		tb.Fatalf("streamMsgCount: Stream.Info: %v", err)
	}
	return info.State.Msgs
}

// TestRegister_StoresMessageInStream asserts a registration published before any
// consumer exists is durably retained by the stream, so a server that starts its
// worker consumer later still receives it.
func TestRegister_StoresMessageInStream(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-durable", minimalCfg(), capabilities.Capabilities{OS: "linux"})
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := streamMsgCount(t, nc); got != 1 {
		t.Errorf("stream holds %d messages after Register; want 1", got)
	}
}

// TestRegister_AdvertisesConfiguredExprLimits pins the worker half of EXPR
// sub-project E4d Task 3's cross-binary gate: the caps a worker will ENFORCE
// have to reach the server, or the server's dispatch gate has nothing to
// compare against and silently falls back to assuming the defaults.
//
// The five values are deliberately distinct and none is a default: five
// same-typed int64 fields copied across two struct boundaries (config ->
// fmtres.ExprLimits -> protocol.ExprLimits) is exactly the shape where a
// transposition compiles, runs, and advertises the wrong bound.
func TestRegister_AdvertisesConfiguredExprLimits(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	want := fmtres.ExprLimits{
		OperationLimit:          11_111,
		MemoryLimit:             2_222_222,
		AssignmentPositions:     3_333,
		AssignmentRetainedBytes: 4_444_444,
		LetRetainedBytes:        5_555_555,
	}
	reg, err := registration.New(
		nc, "worker-expr", minimalCfg(), want,
		capabilities.Capabilities{OS: "linux"}, discardLogger(),
	)
	if err != nil {
		t.Fatalf("registration.New: %v", err)
	}
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var got protocol.RegisterMsg
	if err := json.Unmarshal(firstWorkerStreamMsg(t, nc), &got); err != nil {
		t.Fatalf("unmarshal RegisterMsg: %v", err)
	}
	if got.ExprLimits.OperationLimit != want.OperationLimit ||
		got.ExprLimits.MemoryLimit != want.MemoryLimit ||
		got.ExprLimits.AssignmentPositions != want.AssignmentPositions ||
		got.ExprLimits.AssignmentRetainedBytes != want.AssignmentRetainedBytes ||
		got.ExprLimits.LetRetainedBytes != want.LetRetainedBytes {
		t.Fatalf("advertised ExprLimits = %+v, want operations=%d memory=%d positions=%d "+
			"retained=%d let_retained=%d",
			got.ExprLimits, want.OperationLimit, want.MemoryLimit,
			want.AssignmentPositions, want.AssignmentRetainedBytes, want.LetRetainedBytes)
	}
}

// TestRegister_UnsetExprLimitsAdvertiseTheDefaults pins that a Registrar built
// with the zero value advertises the caps the worker would actually enforce.
// Publishing zeroes would be read by the server as "not advertised".
func TestRegister_UnsetExprLimitsAdvertiseTheDefaults(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-expr-default", minimalCfg(), capabilities.Capabilities{OS: "linux"})
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var got protocol.RegisterMsg
	if err := json.Unmarshal(firstWorkerStreamMsg(t, nc), &got); err != nil {
		t.Fatalf("unmarshal RegisterMsg: %v", err)
	}
	d := fmtres.DefaultExprLimits()
	if got.ExprLimits.OperationLimit != d.OperationLimit ||
		got.ExprLimits.MemoryLimit != d.MemoryLimit ||
		got.ExprLimits.AssignmentPositions != d.AssignmentPositions ||
		got.ExprLimits.AssignmentRetainedBytes != d.AssignmentRetainedBytes ||
		got.ExprLimits.LetRetainedBytes != d.LetRetainedBytes {
		t.Fatalf("advertised ExprLimits = %+v, want the fmtres defaults %+v", got.ExprLimits, d)
	}
}

// firstWorkerStreamMsg returns the payload of the first message retained by the
// SQI_WORKER stream.
func firstWorkerStreamMsg(tb testing.TB, nc *nats.Conn) []byte {
	tb.Helper()

	js, err := jetstream.New(nc)
	if err != nil {
		tb.Fatalf("firstWorkerStreamMsg: jetstream.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := js.Stream(ctx, bus.StreamWorker)
	if err != nil {
		tb.Fatalf("firstWorkerStreamMsg: Stream: %v", err)
	}
	msg, err := stream.GetMsg(ctx, 1)
	if err != nil {
		tb.Fatalf("firstWorkerStreamMsg: GetMsg: %v", err)
	}
	return msg.Data
}

// ── LastRegisteredAt ──────────────────────────────────────────────────────────

// TestLastRegisteredAt_ZeroBeforeRegister checks that LastRegisteredAt returns
// the zero time when Register has never been called.
func TestLastRegisteredAt_ZeroBeforeRegister(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-1", minimalCfg(), capabilities.Capabilities{OS: "linux"})

	if !reg.LastRegisteredAt().IsZero() {
		t.Errorf("LastRegisteredAt() = %v before any Register call; want zero time", reg.LastRegisteredAt())
	}
}

// TestLastRegisteredAt_UpdatedAfterRegister checks that a successful Register
// call updates LastRegisteredAt to a recent timestamp.
func TestLastRegisteredAt_UpdatedAfterRegister(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-1", minimalCfg(), capabilities.Capabilities{OS: "linux"})

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

	reg := newRegistrar(t, nc, "worker-1", minimalCfg(), capabilities.Capabilities{OS: "linux"})

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
