// SPDX-License-Identifier: AGPL-3.0-or-later

package natsclient

import (
	"log/slog"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
)

// startDrainTestServer starts a plain in-process NATS server on a random
// loopback port and returns its client URL.
func startDrainTestServer(t *testing.T) string {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: natsserver.RANDOM_PORT, NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("NATS server not ready within 5s")
	}
	t.Cleanup(ns.Shutdown)
	return ns.ClientURL()
}

// TestDrain_ReturnsOnlyOnceTheConnectionHasClosed pins that Drain blocks until
// the drain has finished: nats.Conn.Drain only starts one and returns at once.
// The worker returns from its run, and exits, right after Drain, so a Drain
// that returned early let the process exit with its last core-NATS publish (the
// deregister) still in the client's buffer, and the server never heard that
// the worker had left.
func TestDrain_ReturnsOnlyOnceTheConnectionHasClosed(t *testing.T) {
	url := startDrainTestServer(t)

	listener, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect listener: %v", err)
	}
	t.Cleanup(listener.Close)
	got := make(chan *nats.Msg, 1)
	if _, err := listener.ChanSubscribe("worker.deregister.w1", got); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := listener.Flush(); err != nil {
		t.Fatalf("flush listener: %v", err)
	}

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	// A subscription whose handler is still busy keeps the drain going for a
	// while, as a worker's in-flight subscriptions do.
	busy := make(chan struct{})
	if _, err := nc.Subscribe("busy", func(*nats.Msg) { <-busy }); err != nil {
		t.Fatalf("subscribe busy: %v", err)
	}
	if err := nc.Publish("busy", nil); err != nil {
		t.Fatalf("publish busy: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.AfterFunc(300*time.Millisecond, func() { close(busy) })

	if err := nc.Publish("worker.deregister.w1", []byte("bye")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	Drain(nc, 5*time.Second, slog.New(slog.DiscardHandler))

	if !nc.IsClosed() {
		t.Fatalf("Drain returned with the connection %v, want closed", nc.Status())
	}
	select {
	case m := <-got:
		if string(m.Data) != "bye" {
			t.Fatalf("received %q, want %q", m.Data, "bye")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the publish made before Drain never reached the server")
	}
}
