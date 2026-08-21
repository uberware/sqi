// SPDX-License-Identifier: AGPL-3.0-or-later

package registration_test

// Additional registration tests covering Deregister, the reconnect
// re-registration hook, the single-queue Register branch, capability storage,
// and the publish-error paths. These complement registration_test.go and reuse
// its helpers (startTestNATS, freePort, connectNATS, minimalCfg, discardLogger).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/capabilities"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// startNATSOnPort starts an embedded JetStream-enabled NATS server bound to the
// given loopback port, with no streams provisioned. Unlike startTestNATS it does
// not register cleanup — the caller controls the server's lifecycle so it can be
// bounced to simulate a dropped/restored connection.
//
// Each call gets its own JetStream store directory, so a server started on the
// port of a shut-down predecessor comes up with no streams (as a fresh server
// would) rather than recovering the old one's.
func startNATSOnPort(tb testing.TB, port int) *natsserver.Server {
	tb.Helper()
	opts := &natsserver.Options{
		Host:           "127.0.0.1",
		Port:           port,
		NoLog:          true,
		NoSigs:         true,
		MaxControlLine: 4096,
		JetStream:      true,
		StoreDir:       tb.TempDir(),
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		tb.Fatalf("startNATSOnPort: NewServer: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		tb.Fatalf("startNATSOnPort: server did not become ready within 5s")
	}
	return ns
}

// connectReconnect opens a *nats.Conn configured for rapid automatic
// reconnection so tests can bounce the server and observe the reconnect hook.
func connectReconnect(tb testing.TB, url string) *nats.Conn {
	tb.Helper()
	nc, err := nats.Connect(
		url,
		nats.ReconnectWait(50*time.Millisecond),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
	)
	if err != nil {
		tb.Fatalf("connectReconnect: %v", err)
	}
	tb.Cleanup(nc.Close)
	return nc
}

// ── Deregister ──────────────────────────────────────────────────────────────

// TestDeregister_PublishesMessage asserts the worker publishes a well-formed
// DeregisterMsg to worker.deregister.<worker>.
func TestDeregister_PublishesMessage(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	sub, err := nc.SubscribeSync(bus.WorkerDeregisterSubject("worker-bye"))
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reg := newRegistrar(t, nc, "worker-bye", minimalCfg(), capabilities.Capabilities{})
	reg.Deregister("graceful shutdown")

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected a deregister message: %v", err)
	}

	var dm struct {
		Version  string `json:"version"`
		Type     string `json:"type"`
		WorkerID string `json:"worker_id"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(msg.Data, &dm); err != nil {
		t.Fatalf("unmarshal DeregisterMsg: %v", err)
	}
	if dm.WorkerID != "worker-bye" {
		t.Errorf("WorkerID = %q, want %q", dm.WorkerID, "worker-bye")
	}
	if dm.Reason != "graceful shutdown" {
		t.Errorf("Reason = %q, want %q", dm.Reason, "graceful shutdown")
	}
	if dm.Type != "deregister" {
		t.Errorf("Type = %q, want %q", dm.Type, "deregister")
	}
}

// TestDeregister_AfterClose_NoPanic asserts Deregister is best-effort: a publish
// failure on a closed connection is logged, not panicked or returned.
func TestDeregister_AfterClose_NoPanic(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-bye", minimalCfg(), capabilities.Capabilities{})
	nc.Close()

	// Must not panic even though the publish will fail.
	reg.Deregister("shutdown after close")
}

// ── Register: single-queue branch + capability propagation ──────────────────

// TestRegister_SingleQueueAndCapabilities asserts the QueueID is set only when
// exactly one queue is configured, and that capability fields are propagated
// into the RegisterMsg.
func TestRegister_SingleQueueAndCapabilities(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	cfg := minimalCfg()
	cfg.QueueIDs = []string{"render-q"}
	cfg.FarmID = "farm-1"
	cfg.ComputeLocation = "onprem"

	caps := capabilities.Capabilities{
		OS:        "linux",
		OSVersion: "6.1",
		CPUCount:  16,
		RAMMb:     32000,
		GPU:       capabilities.GPUInfo{Vendor: "nvidia", Model: "L40", VRAMMb: 49152, Count: 2},
		Tags:      map[string]string{"role": "gpu"},
	}

	sub, err := nc.SubscribeSync(bus.WorkerRegisterSubject("worker-q"))
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reg := newRegistrar(t, nc, "worker-q", cfg, caps)
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected a register message: %v", err)
	}

	var rm struct {
		WorkerID        string            `json:"worker_id"`
		FarmID          string            `json:"farm_id"`
		QueueID         string            `json:"queue_id"`
		ComputeLocation string            `json:"compute_location"`
		OS              string            `json:"os"`
		OSVersion       string            `json:"os_version"`
		CPUCount        int               `json:"cpu_count"`
		RAMMb           int               `json:"ram_mb"`
		Tags            map[string]string `json:"tags"`
		GPUInfo         struct {
			Vendor string `json:"vendor"`
			Model  string `json:"model"`
			VRAMMb int    `json:"vram_mb"`
			Count  int    `json:"count"`
		} `json:"gpu_info"`
	}
	if err := json.Unmarshal(msg.Data, &rm); err != nil {
		t.Fatalf("unmarshal RegisterMsg: %v", err)
	}

	if rm.WorkerID != "worker-q" {
		t.Errorf("WorkerID = %q, want %q", rm.WorkerID, "worker-q")
	}
	if rm.QueueID != "render-q" {
		t.Errorf("QueueID = %q, want %q (single queue should populate QueueID)", rm.QueueID, "render-q")
	}
	if rm.FarmID != "farm-1" {
		t.Errorf("FarmID = %q, want %q", rm.FarmID, "farm-1")
	}
	if rm.ComputeLocation != "onprem" {
		t.Errorf("ComputeLocation = %q, want %q", rm.ComputeLocation, "onprem")
	}
	if rm.OS != "linux" || rm.OSVersion != "6.1" {
		t.Errorf("OS/OSVersion = %q/%q, want linux/6.1", rm.OS, rm.OSVersion)
	}
	if rm.CPUCount != 16 || rm.RAMMb != 32000 {
		t.Errorf("CPUCount/RAMMb = %d/%d, want 16/32000", rm.CPUCount, rm.RAMMb)
	}
	if rm.Tags["role"] != "gpu" {
		t.Errorf("Tags[role] = %q, want %q", rm.Tags["role"], "gpu")
	}
	if rm.GPUInfo.Vendor != "nvidia" || rm.GPUInfo.Count != 2 || rm.GPUInfo.VRAMMb != 49152 {
		t.Errorf("GPUInfo = %+v, want vendor=nvidia count=2 vram=49152", rm.GPUInfo)
	}
}

// TestRegister_MultiQueue_LeavesQueueIDEmpty asserts the QueueID stays empty
// when the worker serves more than one queue (scheduler treats it as "any").
func TestRegister_MultiQueue_LeavesQueueIDEmpty(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	cfg := minimalCfg()
	cfg.QueueIDs = []string{"q1", "q2"}

	sub, err := nc.SubscribeSync(bus.WorkerRegisterSubject("worker-multi"))
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reg := newRegistrar(t, nc, "worker-multi", cfg, capabilities.Capabilities{OS: "linux"})
	if err := reg.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected a register message: %v", err)
	}
	var rm struct {
		QueueID string `json:"queue_id"`
	}
	if err := json.Unmarshal(msg.Data, &rm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rm.QueueID != "" {
		t.Errorf("QueueID = %q, want empty for multi-queue worker", rm.QueueID)
	}
}

// TestRegister_AfterClose_ReturnsError asserts Register surfaces a publish error
// when the connection is closed.
func TestRegister_AfterClose_ReturnsError(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	reg := newRegistrar(t, nc, "worker-x", minimalCfg(), capabilities.Capabilities{OS: "linux"})
	nc.Close()

	if err := reg.Register(context.Background()); err == nil {
		t.Fatal("expected Register to return an error on a closed connection")
	}
}

// ── Capabilities ────────────────────────────────────────────────────────────

// TestCapabilities_ReturnsStored asserts Capabilities returns the value provided
// at construction time.
func TestCapabilities_ReturnsStored(t *testing.T) {
	url := startTestNATS(t)
	nc := connectNATS(t, url)

	caps := capabilities.Capabilities{OS: "darwin", CPUCount: 10, RAMMb: 16000}
	reg := newRegistrar(t, nc, "worker-caps", minimalCfg(), caps)

	got := reg.Capabilities()
	if got.OS != "darwin" || got.CPUCount != 10 || got.RAMMb != 16000 {
		t.Errorf("Capabilities() = %+v, want OS=darwin CPUCount=10 RAMMb=16000", got)
	}
}

// ── Reconnect hook ──────────────────────────────────────────────────────────

// TestSetupReconnectHook_ReregistersOnReconnect installs the reconnect hook,
// bounces the NATS server, and asserts the worker re-publishes its registration
// once the connection is restored.
//
// The verifying subscription is created on the worker's own connection: the
// NATS client restores subscriptions during reconnect before invoking the
// ReconnectHandler, so the re-registration publish is reliably observed.
func TestSetupReconnectHook_ReregistersOnReconnect(t *testing.T) {
	port := freePort(t)
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)

	ns := startNATSOnPort(t, port)
	provisionWorkerStream(t, url)

	wnc := connectReconnect(t, url)

	reg := newRegistrar(t, wnc, "worker-recon", minimalCfg(),
		capabilities.Capabilities{OS: "linux", CPUCount: 8})

	sub, err := wnc.SubscribeSync(bus.WorkerRegisterSubject("worker-recon"))
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	if err := wnc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reg.SetupReconnectHook(context.Background())

	// Bounce the server: shut down S1, bring up S2 on the same port. The worker
	// connection reconnects automatically and its ReconnectHandler re-registers.
	ns.Shutdown()
	ns.WaitForShutdown()
	ns2 := startNATSOnPort(t, port)
	t.Cleanup(ns2.Shutdown)
	provisionWorkerStream(t, url)

	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("expected a re-registration message after reconnect: %v", err)
	}
	var rm struct {
		WorkerID string `json:"worker_id"`
		Type     string `json:"type"`
	}
	if err := json.Unmarshal(msg.Data, &rm); err != nil {
		t.Fatalf("unmarshal RegisterMsg: %v", err)
	}
	if rm.WorkerID != "worker-recon" {
		t.Errorf("re-registered WorkerID = %q, want %q", rm.WorkerID, "worker-recon")
	}
	if rm.Type != "register" {
		t.Errorf("re-registered Type = %q, want %q", rm.Type, "register")
	}

	// LastRegisteredAt must reflect the re-registration. The publish is observed
	// on the subscription before the stream acks it, so poll rather than read
	// once: the hook's Register call is still in flight at this point.
	deadline := time.Now().Add(5 * time.Second)
	for reg.LastRegisteredAt().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("LastRegisteredAt still zero 5s after reconnect re-registration")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
