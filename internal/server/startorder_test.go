// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
)

// reconcileProbe wraps the started broker and runs onReconcile the first
// time the broker-identity reconcile asks for its identity, which is the
// first thing reconcileBrokerLayout does.
type reconcileProbe struct {
	bus.Broker

	once        sync.Once
	onReconcile func()
}

func (p *reconcileProbe) Identity() string {
	p.once.Do(p.onReconcile)
	return p.Broker.Identity()
}

// countConsumers counts the consumers on every stream of the broker at url.
func countConsumers(ctx context.Context, url string) (int, error) {
	nc, err := nats.Connect(url)
	if err != nil {
		return 0, err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return 0, err
	}
	n := 0
	streams := js.StreamNames(ctx)
	for name := range streams.Name() {
		st, err := js.Stream(ctx, name)
		if err != nil {
			return 0, err
		}
		names := st.ConsumerNames(ctx)
		for range names.Name() {
			n++
		}
		if err := names.Err(); err != nil {
			return 0, err
		}
	}
	return n, streams.Err()
}

// TestStart_ReconcilesBrokerIdentityBeforeAnyConsumer pins start's order: the
// broker-identity reconcile, and the fence it may apply, run before the bus
// client the consumers are created on exists. A consumer created first could
// apply a stale status report for an attempt the fence is about to close.
func TestStart_ReconcilesBrokerIdentityBeforeAnyConsumer(t *testing.T) {
	tmp := t.TempDir()
	cfg := Config{
		HTTPAddr: freeLoopbackAddr(t),
		NATS: config.NATSConfig{Embedded: config.NATSEmbeddedConfig{
			Addr: freeLoopbackAddr(t), DataDir: filepath.Join(tmp, "nats"), MaxStoreMB: 64,
		}},
		SQLitePath:         filepath.Join(tmp, "test.db"),
		CheckpointInterval: time.Minute,
		Scheduler:          scheduler.DefaultConfig(),
	}
	srv := New(cfg, slog.New(slog.DiscardHandler), nil)

	type atReconcile struct {
		clientExists, schedulerExists bool
		consumers                     int
		err                           error
	}
	seen := make(chan atReconcile, 1)
	var emb *bus.Embedded
	srv.wrapBroker = func(b bus.Broker) bus.Broker {
		var ok bool
		if emb, ok = b.(*bus.Embedded); !ok {
			seen <- atReconcile{err: fmt.Errorf("broker is %T, want *bus.Embedded", b)}
			return b
		}
		return &reconcileProbe{Broker: b, onReconcile: func() {
			// Runs on Run's goroutine, inside start, so reading the server's
			// fields here is not a race.
			n, err := countConsumers(context.Background(), emb.ClientURL())
			seen <- atReconcile{srv.busClient != nil, srv.sched != nil, n, err}
		}}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	var got atReconcile
	select {
	case got = <-seen:
	case err := <-done:
		t.Fatalf("Run exited before the reconcile: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the broker-identity reconcile never ran")
	}
	if got.err != nil {
		t.Fatalf("count consumers at reconcile: %v", got.err)
	}
	if got.clientExists || got.schedulerExists || got.consumers != 0 {
		t.Fatalf("at the reconcile: bus client exists = %v, scheduler exists = %v, consumers = %d; want none of them",
			got.clientExists, got.schedulerExists, got.consumers)
	}

	// The check above means something only if consumers do appear later.
	deadline := time.Now().Add(15 * time.Second)
	for {
		n, err := countConsumers(context.Background(), emb.ClientURL())
		if err == nil && n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no consumer appeared after startup (last count %d, %v)", n, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
