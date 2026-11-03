// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ── Test harness ──────────────────────────────────────────────────────────────

// freePort asks the OS for an available loopback TCP port and returns it.  The
// port is released immediately, so there is a small race window before the
// broker binds it; acceptable for in-process tests.
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

// startBroker boots an embedded broker with auth off on a temp JetStream dir
// and an OS-assigned loopback port, waits for it to be ready, and registers
// cleanup.
func startBroker(t *testing.T) *Embedded {
	t.Helper()
	return startEmbedded(t, EmbeddedConfig{})
}

// startEmbedded boots an embedded broker from cfg, filling in a loopback
// address, a temp data dir and a 64 MB store cap where cfg leaves them unset.
func startEmbedded(t *testing.T, cfg EmbeddedConfig) *Embedded {
	t.Helper()
	return startEmbeddedWithLogger(t, cfg, slog.New(slog.DiscardHandler))
}

// startEmbeddedWithLogger is [startEmbedded] with the broker logging to logger.
func startEmbeddedWithLogger(t *testing.T, cfg EmbeddedConfig, logger *slog.Logger) *Embedded {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = net.JoinHostPort("127.0.0.1", itoa(freePort(t)))
	}
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir() + "/nats"
	}
	if cfg.MaxStoreMB == 0 {
		cfg.MaxStoreMB = 64
	}
	b := NewEmbedded(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startEmbedded: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// newClient builds a typed Client against the broker and registers cleanup.
func newClient(t *testing.T, b Broker) *Client {
	t.Helper()
	c, err := b.NewClient()
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func itoa(p int) string {
	// Small helper to avoid importing strconv in multiple spots.
	if p == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}

type payload struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ── Broker lifecycle ──────────────────────────────────────────────────────────

func TestBrokerLifecycle(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	dataDir := t.TempDir() + "/nats"
	b := NewEmbedded(EmbeddedConfig{
		Addr:    net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir: dataDir,
	}, logger)

	// Identity and StoreDir describe the configuration, not the running state.
	if got := b.Identity(); got != "embedded:SQI" {
		t.Errorf("Identity() = %q, want %q", got, "embedded:SQI")
	}
	if got := b.StoreDir(); got != dataDir {
		t.Errorf("StoreDir() = %q, want %q", got, dataDir)
	}

	// Before Start: not running, no URL, NewClient fails.
	if err := b.Check(context.Background()); err == nil {
		t.Fatal("Check before Start: want error, got nil")
	}
	if url := b.ClientURL(); url != "" {
		t.Fatalf("ClientURL before Start = %q, want empty", url)
	}
	if _, err := b.NewClient(); err == nil {
		t.Fatal("NewClient before Start: want error, got nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// After Start: healthy, URL present.
	if err := b.Check(context.Background()); err != nil {
		t.Fatalf("Check after Start: %v", err)
	}
	if url := b.ClientURL(); url == "" {
		t.Fatal("ClientURL after Start is empty")
	}

	b.Shutdown()

	// After Shutdown: unhealthy again, idempotent second call.
	if err := b.Check(context.Background()); err == nil {
		t.Fatal("Check after Shutdown: want error, got nil")
	}
	b.Shutdown() // second call must be a no-op, not panic
}

func TestBrokerStartInvalidAddr(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	tests := []struct {
		name string
		addr string
	}{
		{"missing port", "127.0.0.1"},
		{"non-numeric port", "127.0.0.1:abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewEmbedded(EmbeddedConfig{Addr: tt.addr, DataDir: t.TempDir()}, logger)
			if err := b.Start(context.Background()); err == nil {
				t.Fatal("Start with invalid addr: want error, got nil")
			}
		})
	}
}

// TestBrokerStart_ReportsAPortInUse pins that a broker whose port is already
// taken fails promptly with the listen error, instead of waiting out the
// readiness timeout and reporting only that it never became ready.
func TestBrokerStart_ReportsAPortInUse(t *testing.T) {
	taken, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	b := NewEmbedded(EmbeddedConfig{Addr: taken.Addr().String(), DataDir: t.TempDir() + "/nats"}, slog.New(slog.DiscardHandler))
	start := time.Now()
	err = b.Start(context.Background())
	if err == nil {
		b.Shutdown()
		t.Fatal("Start on a port in use: want an error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Start took %s to fail; want it to fail as soon as the listen fails", elapsed)
	}
	if !strings.Contains(err.Error(), "listening") {
		t.Errorf("Start error = %q; want the listen failure named", err)
	}
}

// TestBrokerStart_ReportsAnUnusableDataDir pins the same for JetStream: a store
// directory it cannot create fails Start promptly, naming JetStream.
func TestBrokerStart_ReportsAnUnusableDataDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewEmbedded(EmbeddedConfig{Addr: net.JoinHostPort("127.0.0.1", itoa(freePort(t))), DataDir: filepath.Join(file, "nats")},
		slog.New(slog.DiscardHandler))
	start := time.Now()
	err := b.Start(context.Background())
	if err == nil {
		b.Shutdown()
		t.Fatal("Start with an unusable data dir: want an error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Start took %s to fail; want it to fail as soon as JetStream does", elapsed)
	}
	if !strings.Contains(err.Error(), "JetStream") {
		t.Errorf("Start error = %q; want the JetStream failure named", err)
	}
}

func TestStreamsProvisioned(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, name := range []string{StreamTask, StreamLogs, StreamWorker, StreamCancel} {
		if _, err := c.js.Stream(ctx, name); err != nil {
			t.Errorf("stream %s not provisioned: %v", name, err)
		}
	}
}

// ── Push-consumer round trips (status / logs / worker) ────────────────────────

func TestPushConsumers(t *testing.T) {
	tests := []struct {
		name    string
		publish func(c *Client, ctx context.Context, data []byte) error
		consume func(c *Client, ctx context.Context, h jetstream.MessageHandler) (jetstream.ConsumeContext, error)
	}{
		{
			name: "task status",
			publish: func(c *Client, ctx context.Context, data []byte) error {
				return c.PublishTaskStatus(ctx, "w-1", "job-1", data)
			},
			consume: (*Client).ConsumeTaskStatus,
		},
		{
			name: "task logs",
			publish: func(c *Client, ctx context.Context, data []byte) error {
				return c.PublishTaskLog(ctx, "w-1", "task-1", data)
			},
			consume: (*Client).ConsumeTaskLogs,
		},
		{
			name: "worker heartbeat",
			publish: func(c *Client, ctx context.Context, data []byte) error {
				return c.PublishWorkerHeartbeat(ctx, "w-1", data)
			},
			consume: (*Client).ConsumeWorker,
		},
		{
			name: "worker register",
			publish: func(c *Client, ctx context.Context, data []byte) error {
				return c.PublishWorkerRegister(ctx, "w-1", data)
			},
			consume: (*Client).ConsumeWorker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := startBroker(t)
			c := newClient(t, b)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			recv := make(chan []byte, 1)
			cc, err := tt.consume(c, ctx, func(m jetstream.Msg) {
				if err := m.Ack(); err != nil {
					t.Errorf("Ack: %v", err)
				}
				select {
				case recv <- m.Data():
				default:
				}
			})
			if err != nil {
				t.Fatalf("consume: %v", err)
			}
			defer cc.Stop()

			want := payload{ID: tt.name, Body: "hello"}
			if err := tt.publish(c, ctx, mustJSON(t, want)); err != nil {
				t.Fatalf("publish: %v", err)
			}

			select {
			case data := <-recv:
				var got payload
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if got != want {
					t.Fatalf("payload = %+v, want %+v", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for delivery")
			}
		})
	}
}

// ── Cancel publish (no Ensure* helper; verify durable write) ──────────────────

func TestPublishTaskCancel(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := payload{ID: "task-c", Body: "cancel"}
	if err := c.PublishTaskCancel(ctx, "task-c", mustJSON(t, want)); err != nil {
		t.Fatalf("PublishTaskCancel: %v", err)
	}

	// Verify it landed durably in SQI_CANCEL by reading via an ephemeral consumer.
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, StreamCancel, jetstream.ConsumerConfig{
		FilterSubject: TaskCancelSubject("task-c"),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create cancel consumer: %v", err)
	}
	msg := fetchOne(t, consumer)
	var got payload
	if err := json.Unmarshal(msg.Data(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("payload = %+v, want %+v", got, want)
	}
	if err := msg.Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}
}

// ── Drain ─────────────────────────────────────────────────────────────────────

func TestClientDrain(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recv := make(chan struct{}, 1)
	_, err := c.ConsumeTaskStatus(ctx, func(m jetstream.Msg) {
		if err := m.Ack(); err != nil {
			t.Errorf("Ack: %v", err)
		}
		select {
		case recv <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("ConsumeTaskStatus: %v", err)
	}

	if err := c.PublishTaskStatus(ctx, "w-1", "job-d", mustJSON(t, payload{ID: "t", Body: "x"})); err != nil {
		t.Fatalf("PublishTaskStatus: %v", err)
	}
	select {
	case <-recv:
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive message before drain")
	}

	// Drain stops the tracked consumer and drains the connection cleanly.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := c.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// nats Drain() closes the connection asynchronously after subscriptions are
	// drained, so poll for the connection to reach the closed state.
	deadline := time.Now().Add(5 * time.Second)
	for !c.nc.IsClosed() {
		if time.Now().After(deadline) {
			t.Fatal("connection did not close within 5s after Drain")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Publishing on a closed connection must fail.
	if err := c.PublishTaskStatus(context.Background(), "w-1", "job-d", []byte("{}")); err == nil {
		t.Fatal("publish after Drain: want error, got nil")
	}
}

// TestClientDrain_WaitsForInFlightHandlers checks that Drain returns only
// once a message already being handled has finished and the connection has
// closed. nats.go's ConsumeContext.Drain and Conn.Drain only begin a drain;
// the server shuts the broker down (and gives its account guard up) as soon
// as Drain returns, so returning early would cut a handler off mid-write.
func TestClientDrain_WaitsForInFlightHandlers(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const handlerTime = 500 * time.Millisecond
	started := make(chan struct{})
	var finished atomic.Bool
	_, err := c.ConsumeTaskStatus(ctx, func(m jetstream.Msg) {
		close(started)
		time.Sleep(handlerTime)
		if err := m.Ack(); err != nil {
			t.Errorf("Ack: %v", err)
		}
		finished.Store(true)
	})
	if err != nil {
		t.Fatalf("ConsumeTaskStatus: %v", err)
	}
	if err := c.PublishTaskStatus(ctx, "w-1", "job-w", mustJSON(t, payload{ID: "t", Body: "x"})); err != nil {
		t.Fatalf("PublishTaskStatus: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := c.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Drain returned while a handler was still running")
	}
	if !c.nc.IsClosed() {
		t.Fatalf("Drain returned with the connection %v, want CLOSED", c.nc.Status())
	}
}

// TestClientDrain_BoundedByContext checks that a handler that outlives the
// context does not hold Drain: it gives up, closes the connection and says so.
func TestClientDrain_BoundedByContext(t *testing.T) {
	b := startBroker(t)
	c := newClient(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	_, err := c.ConsumeTaskStatus(ctx, func(jetstream.Msg) {
		close(started)
		<-release
	})
	if err != nil {
		t.Fatalf("ConsumeTaskStatus: %v", err)
	}
	if err := c.PublishTaskStatus(ctx, "w-1", "job-b", mustJSON(t, payload{ID: "t", Body: "x"})); err != nil {
		t.Fatalf("PublishTaskStatus: %v", err)
	}
	<-started

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer drainCancel()
	began := time.Now()
	err = c.Drain(drainCtx)
	if err == nil {
		t.Fatal("Drain succeeded though a handler outlived its context")
	}
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("Drain took %s, want it bounded by its 300ms context", elapsed)
	}
	if !c.nc.IsClosed() {
		t.Fatalf("Drain gave up with the connection %v, want it closed", c.nc.Status())
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// fetchOne pulls exactly one message from a pull consumer within a bounded
// wait, failing the test if none arrives.
func fetchOne(t *testing.T, consumer jetstream.Consumer) jetstream.Msg {
	t.Helper()
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	msg, ok := <-batch.Messages()
	if err := batch.Error(); err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if !ok {
		t.Fatal("fetchOne: no message delivered within timeout")
	}
	return msg
}

// TestNewClient_ConnectErrorRedactsCredentials checks that a failed dial
// names each URL of the comma-separated list without its credentials.
func TestNewClient_ConnectErrorRedactsCredentials(t *testing.T) {
	a := net.JoinHostPort("127.0.0.1", itoa(freePort(t)))
	b := net.JoinHostPort("127.0.0.1", itoa(freePort(t)))
	_, err := NewClient("nats://sqi:hunter2@"+a+",nats://s3cr3tt0ken@"+b, slog.New(slog.DiscardHandler),
		nats.MaxReconnects(0), nats.Timeout(200*time.Millisecond))
	if err == nil {
		t.Fatal("NewClient succeeded with no broker listening")
	}
	for _, secret := range []string{"hunter2", "s3cr3tt0ken"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("NewClient error %q repeats the credential %q", err, secret)
		}
	}
	if want := "nats://REDACTED@" + a + ",nats://REDACTED@" + b; !strings.Contains(err.Error(), want) {
		t.Fatalf("NewClient error %q does not name %s", err, want)
	}
}
