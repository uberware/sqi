// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/loadtest/client"
	"github.com/uberware/sqi/internal/loadtest/internal/testserver"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// startOnServer starts a queue-less worker heartbeating every heartbeat on
// srv, in a new farm, and stops it at cleanup.
func startOnServer(t *testing.T, srv *testserver.Server, heartbeat time.Duration) (*Worker, *client.Client, string) {
	t.Helper()
	c := client.New(srv.HTTPURL())
	farmID, err := c.CreateFarm(t.Context(), "control-farm")
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	w := NewWorker(WorkerSpec{
		ID: uuid.NewString(), Hostname: "sim-control", FarmID: farmID,
		Group: scenario.WorkerGroup{
			CPU: 4, OS: "linux", Heartbeat: scenario.Duration{Duration: heartbeat},
		},
		NATSURL: srv.NATSURL(),
	}, BehaviorFunc(func(*protocol.AssignMsg, *rand.Rand) TaskPlan { return TaskPlan{Duration: time.Millisecond} }),
		NewLedger(), slog.New(slog.DiscardHandler))
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

// controlRequest sends m to workerID's control subject over nc and returns the
// decoded reply.
func controlRequest(t *testing.T, nc *nats.Conn, workerID string, m protocol.ControlMsg) protocol.ControlReplyMsg {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg, err := nc.Request(bus.WorkerControlSubject(workerID), data, 5*time.Second)
	if err != nil {
		t.Fatalf("control request %s: %v", m.Command, err)
	}
	var reply protocol.ControlReplyMsg
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		t.Fatalf("decode reply %q: %v", msg.Data, err)
	}
	return reply
}

// A simulated worker answers on its control subject: a reregister is accepted
// and registers it again through the real registrar, and every power command,
// or a message it cannot read, is refused.
func TestControl_ReregisterAcceptedPowerCommandsRefused(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{})
	w, _, _ := startOnServer(t, srv, 15*time.Second)
	nc, err := nats.Connect(srv.NATSURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	// Count registrations through the fault hook Register consults first:
	// LastRegisteredAt can read the same wall-clock tick twice on Windows.
	var registrations atomic.Int64
	count := func() error {
		registrations.Add(1)
		return nil
	}
	w.registerFault.Store(&count)

	cmd := func(c protocol.ControlCommand) protocol.ControlMsg {
		return protocol.ControlMsg{Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: c}
	}
	if r := controlRequest(t, nc, w.spec.ID, cmd(protocol.ControlReregister)); !r.Accepted {
		t.Fatalf("reregister: reply = %+v, want accepted", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for registrations.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no registration followed the accepted reregister")
		}
		time.Sleep(10 * time.Millisecond)
	}

	tests := []struct {
		name string
		msg  protocol.ControlMsg
	}{
		{"sleep", cmd(protocol.ControlSleep)},
		{"shutdown", cmd(protocol.ControlShutdown)},
		{"wake", protocol.ControlMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlWake,
			MACs: []string{"aa:bb:cc:dd:ee:ff"}, Targets: []string{"192.168.1.255:9"},
		}},
		{"other version", protocol.ControlMsg{Version: "0", Type: protocol.TypeControl, Command: protocol.ControlReregister}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := controlRequest(t, nc, w.spec.ID, tt.msg); r.Accepted || r.Reason == "" {
				t.Fatalf("reply = %+v, want a refusal with a reason", r)
			}
		})
	}
	if r := controlRequest(t, nc, w.spec.ID, cmd(protocol.ControlSleep)); r.Reason != protocol.ControlRefusalNotAllowed {
		t.Errorf("sleep refused with %q, want %q", r.Reason, protocol.ControlRefusalNotAllowed)
	}
}

// A simulated worker the server marks offline while it is alive (its
// heartbeats arrive less often than the heartbeat timeout) comes back online:
// its next heartbeat makes the server ask it to register again, and it does.
func TestWorker_MarkedOfflineComesBackOnline(t *testing.T) {
	srv := testserver.Start(t, testserver.Options{
		WorkerTimeout: 3 * time.Second, HeartbeatSweepInterval: 250 * time.Millisecond,
		BusReconnectGrace: time.Second,
	})
	w, c, farmID := startOnServer(t, srv, 6*time.Second)

	status := func() string {
		page, err := c.ListWorkers(t.Context(), farmID, 10, 0)
		if err != nil {
			t.Fatalf("ListWorkers: %v", err)
		}
		for _, got := range page.Items {
			if got.ID == w.spec.ID {
				return got.Status
			}
		}
		return ""
	}
	await := func(want string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for status() != want {
			if time.Now().After(deadline) {
				t.Fatalf("worker never %s; last status %q", want, status())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	await("offline")
	await("online")
}

// startNATS starts an in-process NATS server and returns a connection to it.
func startNATS(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// subscribeTestControl subscribes a responder for w1 that registers through
// register.
func subscribeTestControl(t *testing.T, nc *nats.Conn, register func(context.Context) error) *controlResponder {
	t.Helper()
	r, err := subscribeControl(t.Context(), nc, controlDeps{
		workerID: "w1", busy: func() int { return 0 },
		deregister: func(string, protocol.DeregisterKind) {}, register: register,
		logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("subscribeControl: %v", err)
	}
	return r
}

var reregisterMsg = protocol.ControlMsg{
	Version: protocol.ProtocolVersion, Type: protocol.TypeControl, Command: protocol.ControlReregister,
}

// awaitCount waits until n reads want.
func awaitCount(t *testing.T, n *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("count = %d, want %d", n.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// stop takes the responder off its subject before it waits for a
// re-registration still running, so a request arriving while a worker is torn
// down finds no responder, and stop returns only once the registration has.
func TestControlResponder_StopUnsubscribesBeforeWaiting(t *testing.T) {
	nc := startNATS(t)
	release := make(chan struct{})
	var registrations atomic.Int64
	r := subscribeTestControl(t, nc, func(context.Context) error {
		registrations.Add(1)
		<-release
		return nil
	})
	if reply := controlRequest(t, nc, "w1", reregisterMsg); !reply.Accepted {
		t.Fatalf("reregister reply = %+v, want accepted", reply)
	}
	awaitCount(t, &registrations, 1)

	stopped := make(chan struct{})
	go func() {
		r.stop()
		close(stopped)
	}()
	data, err := json.Marshal(reregisterMsg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := nc.Request(bus.WorkerControlSubject("w1"), data, time.Second)
		if errors.Is(err, nats.ErrNoResponders) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a request while stopping got %v, want no responders", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("stop returned while a re-registration was still running")
	default:
	}
	close(release)
	<-stopped
	if n := registrations.Load(); n != 1 {
		t.Errorf("registrations = %d, want 1", n)
	}
}

// A reregister accepted while one is running is answered but starts no second
// registration: one registration answers every request made before it lands.
func TestControlResponder_OneRegistrationAtATime(t *testing.T) {
	nc := startNATS(t)
	release := make(chan struct{})
	var registrations atomic.Int64
	r := subscribeTestControl(t, nc, func(context.Context) error {
		registrations.Add(1)
		<-release
		return nil
	})
	if reply := controlRequest(t, nc, "w1", reregisterMsg); !reply.Accepted {
		t.Fatalf("first reregister reply = %+v, want accepted", reply)
	}
	awaitCount(t, &registrations, 1)
	if reply := controlRequest(t, nc, "w1", reregisterMsg); !reply.Accepted {
		t.Fatalf("second reregister reply = %+v, want accepted", reply)
	}
	close(release)
	r.stop()
	if n := registrations.Load(); n != 1 {
		t.Errorf("registrations = %d, want 1", n)
	}
}

// A reregister that reaches the gate after stop, as one the controller
// accepted just before the unsubscribe can, starts no registration.
func TestReregisterGate_AfterStopStartsNothing(t *testing.T) {
	var registrations atomic.Int64
	g := reregisterGate{register: func(context.Context) error {
		registrations.Add(1)
		return nil
	}}
	g.stop()
	if err := g.reregister(t.Context()); err != nil {
		t.Fatalf("reregister after stop: %v", err)
	}
	if n := registrations.Load(); n != 0 {
		t.Errorf("registrations = %d, want 0", n)
	}
}
