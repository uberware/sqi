// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// BrokerConfig holds the parameters needed to start the embedded NATS server.
type BrokerConfig struct {
	// Addr is the TCP address the embedded NATS server binds to, in
	// "host:port" form.  Defaults to "0.0.0.0:4222" (all interfaces) so that
	// workers which discover the server via mDNS can reach the broker at the
	// advertised LAN host. Broker authentication is not yet in place; it
	// arrives in phase 3.
	Addr string

	// DataDir is the directory JetStream uses for file-backed stream storage.
	// Created at startup if it does not exist.
	DataDir string

	// MaxStoreMB is the maximum disk space JetStream may use, in megabytes.
	// A value of 0 means unlimited (not recommended for production).
	MaxStoreMB int
}

// Broker wraps an in-process NATS server with JetStream enabled and manages
// its lifecycle (start, health check, graceful shutdown).
//
// After [Broker.Start] returns successfully, callers can obtain the server's
// client URL via [Broker.ClientURL] to establish their own NATS connections.
// The typed per-subsystem client wrapper is [Client] in client.go.
type Broker struct {
	cfg    BrokerConfig
	logger *slog.Logger

	ns *natsserver.Server // embedded NATS server process
	nc *nats.Conn         // admin connection used for stream provisioning
}

// New creates a [Broker] with the given configuration and logger.
// Call [Broker.Start] to boot the underlying NATS server.
func New(cfg BrokerConfig, logger *slog.Logger) *Broker {
	return &Broker{cfg: cfg, logger: logger}
}

// Start boots the embedded NATS server, enables JetStream, waits for the
// server to accept connections, and then provisions the sqi-server stream
// topology via [ensureStreams].  It returns an error if any step fails.
//
// Start must be called before any other [Broker] method and must not be
// called more than once.
func (b *Broker) Start(ctx context.Context) error {
	host, portStr, err := net.SplitHostPort(b.cfg.Addr)
	if err != nil {
		return fmt.Errorf("bus: parse addr %q: %w", b.cfg.Addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("bus: parse port in %q: %w", b.cfg.Addr, err)
	}

	// Convert storage cap from megabytes to bytes; treat 0 as -1 (unlimited).
	var maxStore int64 = -1
	if b.cfg.MaxStoreMB > 0 {
		maxStore = int64(b.cfg.MaxStoreMB) * 1024 * 1024
	}

	opts := &natsserver.Options{
		Host: host,
		Port: port,

		// JetStream — file-backed, bounded by the configured storage cap.
		JetStream:         true,
		StoreDir:          b.cfg.DataDir,
		JetStreamMaxStore: maxStore,

		// Prevent NATS from installing its own SIGINT/SIGTERM handler; sqi-server
		// owns signal handling and calls Broker.Shutdown() on the way out.
		NoSigs: true,

		// Suppress the NATS server's built-in logger.  All relevant lifecycle
		// events are re-emitted through slog in this file instead.
		NoLog: true,
	}

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return fmt.Errorf("bus: create nats server: %w", err)
	}

	// Start is non-blocking; the server accepts connections asynchronously.
	ns.Start()

	// Wait up to 10 s for the server to be ready to accept connections.
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		return errors.New("bus: nats server did not become ready within 10s")
	}

	b.ns = ns
	b.logger.InfoContext(
		ctx, "bus: nats server started",
		slog.String("addr", b.cfg.Addr),
		slog.String("data_dir", b.cfg.DataDir),
		slog.Int("max_store_mb", b.cfg.MaxStoreMB),
	)

	// Establish an admin connection used only for stream provisioning.
	// This is a plain TCP connection to the loopback listener; the latency
	// is negligible and avoids importing the server package into callers.
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		ns.WaitForShutdown()
		return fmt.Errorf("bus: admin connect: %w", err)
	}
	b.nc = nc

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
		return fmt.Errorf("bus: jetstream context: %w", err)
	}

	if err := ensureStreams(ctx, js); err != nil {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
		return fmt.Errorf("bus: provision streams: %w", err)
	}

	b.logger.InfoContext(
		ctx, "bus: jetstream streams provisioned",
		slog.String("streams", StreamTask+","+StreamLogs+","+StreamWorker+","+StreamCancel),
	)
	return nil
}

// Shutdown drains the admin connection and stops the embedded NATS server,
// waiting until it has fully exited before returning.  It is safe to call
// Shutdown more than once; subsequent calls are no-ops.
func (b *Broker) Shutdown() {
	if b.nc != nil {
		b.nc.Close()
		b.nc = nil
	}
	if b.ns != nil {
		b.ns.Shutdown()
		b.ns.WaitForShutdown()
		b.ns = nil
		b.logger.InfoContext(context.Background(), "bus: nats server stopped")
	}
}

// Check implements [health.Checker].  It returns nil when the embedded NATS
// server is running and the admin connection is open, or a descriptive error
// otherwise.  Registered with the health registry during server startup so
// that GET /readyz reflects broker health.
func (b *Broker) Check(_ context.Context) error {
	if b.ns == nil || !b.ns.Running() {
		return errors.New("nats server not running")
	}
	if b.nc == nil || b.nc.IsClosed() {
		return errors.New("nats admin connection closed")
	}
	return nil
}

// ClientURL returns the nats:// URL that in-process clients should connect to.
// Returns an empty string if the broker has not been started yet.
func (b *Broker) ClientURL() string {
	if b.ns == nil {
		return ""
	}
	return b.ns.ClientURL()
}

// NewClient dials the embedded NATS server and returns a connected [Client]
// configured for automatic reconnection.  It must be called after [Broker.Start]
// returns successfully.
//
// The returned Client should be shared across goroutines; create one per server
// component (or one shared instance for the whole server) rather than dialing
// repeatedly.
func (b *Broker) NewClient() (*Client, error) {
	if b.ns == nil {
		return nil, errors.New("bus: broker not started")
	}
	return NewClient(b.ns.ClientURL(), b.logger)
}
