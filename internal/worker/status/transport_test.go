// SPDX-License-Identifier: AGPL-3.0-or-later

package status_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/worker/status"
)

// A publish on a closed connection fails with nats.ErrConnectionClosed, which
// the publisher drops at once instead of retrying until Flush gives up.
func TestJetStreamTransport_ClosedConnectionIsPermanent(t *testing.T) {
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server not ready")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tr, err := status.NewJetStreamTransport(nc)
	if err != nil {
		t.Fatalf("NewJetStreamTransport: %v", err)
	}
	nc.Close()

	err = tr.PublishAcked(t.Context(), "task.status.w1.j1", []byte("{}"), "a1:succeeded")
	if !errors.Is(err, nats.ErrConnectionClosed) {
		t.Fatalf("PublishAcked on a closed connection = %v, want nats.ErrConnectionClosed", err)
	}

	p := status.New(tr, status.Config{WorkerID: "w1"}, slog.New(slog.DiscardHandler))
	code := 0
	p.Terminal(assign("a1"), "", "succeeded", &code, "", nil, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if dropped := p.Flush(ctx); dropped != 0 {
		t.Errorf("Flush dropped %d, want 0 (the report was dropped when it failed)", dropped)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Flush took %v on a closed connection, want it prompt", took)
	}
}

// A try made while the connection is reconnecting is refused at once and
// leaves nothing in the client's reconnect buffer. A buffered try would be
// sent on reconnect with the age it was built with, the stream would store
// it, and the later tries, built with the right age, would be dropped as
// duplicates of its message ID: the retry delay would count as run time.
func TestJetStreamTransport_ReconnectingTryIsNotBuffered(t *testing.T) {
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		t.Fatal("nats-server not ready")
	}
	nc, err := nats.Connect(srv.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		srv.Shutdown()
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	tr, err := status.NewJetStreamTransport(nc)
	if err != nil {
		t.Fatalf("NewJetStreamTransport: %v", err)
	}
	srv.Shutdown()
	deadline := time.Now().Add(10 * time.Second)
	for !nc.IsReconnecting() {
		if time.Now().After(deadline) {
			t.Fatal("connection never started reconnecting")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err = tr.PublishAcked(ctx, "task.status.w1.j1", []byte("{}"), "a1:succeeded")
	if !errors.Is(err, nats.ErrConnectionReconnecting) {
		t.Errorf("PublishAcked while reconnecting = %v, want nats.ErrConnectionReconnecting", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("PublishAcked while reconnecting took %v, want it refused at once", took)
	}
	if n, err := nc.Buffered(); err != nil || n != 0 {
		t.Errorf("reconnect buffer holds %d bytes (err %v), want 0: the try would be sent on reconnect", n, err)
	}
}
