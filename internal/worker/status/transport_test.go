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
