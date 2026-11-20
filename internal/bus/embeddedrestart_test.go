// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// boundPort is the port b's current server listens on, 0 when it has none.
func boundPort(b *Embedded) int {
	b.mu.Lock()
	ns := b.ns
	b.mu.Unlock()
	if ns == nil {
		return 0
	}
	return boundPortOf(ns, 0)
}

// pauseAfterStop makes b's restart wait, once it has stopped the old server,
// until release is closed, closing reached when it gets there.
func pauseAfterStop(b *Embedded) (reached, release chan struct{}) {
	reached, release = make(chan struct{}), make(chan struct{})
	b.afterStopHook = func() {
		close(reached)
		<-release
	}
	return reached, release
}

// eventuallyNil waits up to within for f to return nil.
func eventuallyNil(t *testing.T, within time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still %v after %s", what, err, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// restartWorker connects as a worker that reconnects at once and forever.
func restartWorker(t *testing.T, b *Embedded, opts ...nats.Option) *nats.Conn {
	t.Helper()
	opts = append(opts, nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	nc, err := nats.Connect(b.ClientURL(), opts...)
	if err != nil {
		t.Fatalf("connect as a worker: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// fetchDurable fetches one message from the durable named durable on SQI_TASK and
// acks it, returning its data.
func fetchDurable(t *testing.T, js jetstream.JetStream, durable string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var cons jetstream.Consumer
	eventuallyNil(t, 15*time.Second, "look the durable up", func() error {
		var err error
		cons, err = js.Consumer(ctx, StreamTask, durable)
		return err
	})
	for {
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("fetch from %s: %v", durable, err)
			}
			continue
		}
		for m := range batch.Messages() {
			if err := m.Ack(); err != nil {
				t.Fatalf("ack: %v", err)
			}
			return string(m.Data())
		}
		if ctx.Err() != nil {
			t.Fatalf("fetch from %s: nothing within 15s (%v)", durable, batch.Error())
		}
	}
}

// TestRestart_FollowsCurrentServer pins the restart: the broker comes back on
// the same port, a worker and sqi's own in-process connections reconnect to
// it, and what the stream had stored, and the durable's position, survive.
// With auth on the worker's reconnect is authorized through the callout,
// whose connection must follow the restart too.
func TestRestart_FollowsCurrentServer(t *testing.T) {
	for _, auth := range []bool{false, true} {
		name := "auth off"
		if auth {
			name = "auth on"
		}
		t.Run(name, func(t *testing.T) {
			var b *Embedded
			var workerOpts []nats.Option
			if auth {
				creds := newCredentialMap()
				seed, pub := creds.enroll(t, "worker-a")
				b = startAuthEmbedded(t, creds)
				workerOpts = append(workerOpts, nkeyOption(t, seed, pub))
			} else {
				b = startEmbedded(t, EmbeddedConfig{})
			}
			worker := restartWorker(t, b, workerOpts...)
			data, err := b.NewClient()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(data.Close)

			ctx := t.Context()
			if _, err := data.js.CreateOrUpdateConsumer(ctx, StreamTask, jetstream.ConsumerConfig{
				Durable: "restart-test", FilterSubject: "task.status.worker-a.>", AckPolicy: jetstream.AckExplicitPolicy,
			}); err != nil {
				t.Fatal(err)
			}
			for _, m := range []string{"one", "two", "three"} {
				if err := worker.Publish("task.status.worker-a.job", []byte(m)); err != nil {
					t.Fatal(err)
				}
			}
			if err := worker.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := fetchDurable(t, data.js, "restart-test"); got != "one" {
				t.Fatalf("first message %q, want one", got)
			}

			port := boundPort(b)
			if err := b.restart(ctx); err != nil {
				t.Fatalf("restart: %v", err)
			}
			if got := boundPort(b); got != port {
				t.Errorf("port %d after the restart, was %d", got, port)
			}
			waitForStatus(t, worker, nats.CONNECTED, 15*time.Second)
			waitForStatus(t, data.nc, nats.CONNECTED, 15*time.Second)
			eventuallyNil(t, 15*time.Second, "Check", func() error { return b.Check(ctx) })

			for _, want := range []string{"two", "three"} {
				if got := fetchDurable(t, data.js, "restart-test"); got != want {
					t.Errorf("message %q after the restart, want %q", got, want)
				}
			}
			if err := worker.Publish("task.status.worker-a.job", []byte("four")); err != nil {
				t.Fatal(err)
			}
			if err := worker.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := fetchDurable(t, data.js, "restart-test"); got != "four" {
				t.Errorf("message %q published after the restart, want four", got)
			}
		})
	}
}

// TestRestart_RevokeWaitsForRestart pins that a Revoke begun while the broker
// restarts returns only once the new server is up, and that the revoked
// worker cannot reconnect to it.
func TestRestart_RevokeWaitsForRestart(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds)
	restartWorker(t, b, nkeyOption(t, seed, pub)) // the connection the revoke must not let back

	reached, release := pauseAfterStop(b)
	restarted := make(chan error, 1)
	go func() { restarted <- b.restart(context.Background()) }()
	<-reached

	creds.remove(pub)
	revoked := make(chan error, 1)
	go func() { revoked <- b.Revoke(context.Background(), "worker-a") }()
	select {
	case err := <-revoked:
		t.Fatalf("Revoke returned %v while the broker was restarting", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-restarted; err != nil {
		t.Fatalf("restart: %v", err)
	}
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatalf("Revoke: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Revoke did not return after the restart")
	}
	time.Sleep(time.Second) // time for a reconnect that should be refused to land
	if conns := connzForUser(t, b, "worker-a"); len(conns) != 0 {
		t.Errorf("the revoked worker has %d connections on the new server", len(conns))
	}
}

// TestRestart_ShutdownDuringRestart pins that a Shutdown arriving mid-restart
// waits it out and leaves no server running.
func TestRestart_ShutdownDuringRestart(t *testing.T) {
	b := startEmbedded(t, EmbeddedConfig{})
	port := boundPort(b)
	reached, release := pauseAfterStop(b)
	go func() { _ = b.restart(context.Background()) }() //nolint:errcheck // either outcome; Shutdown is what is judged
	<-reached

	shut := make(chan struct{})
	go func() { b.Shutdown(); close(shut) }()
	select {
	case <-shut:
		t.Fatal("Shutdown returned while the restart held the broker")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-shut:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return within 10s of the restart")
	}
	if u := b.ClientURL(); u != "" {
		t.Errorf("ClientURL = %q after Shutdown, want empty", u)
	}
	d := net.Dialer{Timeout: time.Second}
	if c, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", itoa(port))); err == nil {
		_ = c.Close()
		t.Error("the broker's port still accepts connections after Shutdown")
	}
	if err := b.restart(context.Background()); err == nil {
		t.Error("restart after Shutdown succeeded")
	}
}

// TestRestart_StartFailureLeavesNoServer pins a restart whose new server cannot
// start: there is then no server, Check says so, and a later restart works.
func TestRestart_StartFailureLeavesNoServer(t *testing.T) {
	b := startEmbedded(t, EmbeddedConfig{})
	worker := restartWorker(t, b)
	port := boundPort(b)

	var hog net.Listener
	b.afterStopHook = func() {
		var lc net.ListenConfig
		var err error
		if hog, err = lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(port))); err != nil {
			t.Errorf("occupy the broker's port: %v", err)
		}
	}
	if err := b.restart(t.Context()); err == nil {
		t.Fatal("restart with the port taken succeeded")
	}
	if err := b.Check(t.Context()); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("Check = %v, want nats server not running", err)
	}
	if hog != nil {
		_ = hog.Close()
	}

	b.afterStopHook = nil
	if err := b.restart(t.Context()); err != nil {
		t.Fatalf("second restart: %v", err)
	}
	waitForStatus(t, worker, nats.CONNECTED, 15*time.Second)
	if err := b.Revoke(t.Context(), "nobody"); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Revoke after the restart: %v", err)
	}
}
