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
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

// BrokerConfig holds the parameters needed to start the embedded NATS server.
type BrokerConfig struct {
	// Addr is the TCP address the embedded NATS server binds to, in
	// "host:port" form.  Defaults to "0.0.0.0:4222" (all interfaces) so that
	// workers which discover the server via mDNS can reach the broker at the
	// advertised LAN host.
	//
	// Broker authentication is OPT-IN and off by default. With it off, any
	// host that can reach this port can register as a worker and receive
	// assignments — including on a LAN, since the default binds all
	// interfaces. sqi-server emits a startup WARN in exactly that case.
	// Set nats.auth.enabled to require a per-worker credential, or bind this
	// to 127.0.0.1:4222 for single-machine use. See docs/auth.md.
	Addr string

	// DataDir is the directory JetStream uses for file-backed stream storage.
	// Created at startup if it does not exist.
	DataDir string

	// MaxStoreMB is the maximum disk space JetStream may use, in megabytes.
	// A value of 0 means unlimited (not recommended for production).
	MaxStoreMB int

	// Auth configures per-worker nkey authorization on the broker. Zero value
	// leaves authorization disabled.
	Auth BrokerAuthConfig
}

// BrokerAuthConfig controls per-worker nkey authorization on the broker.
type BrokerAuthConfig struct {
	// Enabled requires every connection to present a credential for an
	// enrolled nkey. When false, the broker accepts anonymous connections.
	Enabled bool

	// Credentials is the initial enrolled set, loaded from the store at boot.
	Credentials []WorkerCredentialRef
}

// WorkerCredentialRef is bus's own minimal view of an enrolled worker
// credential, carrying only what the broker needs to authorize a connection.
// It exists so that internal/bus does not need to import internal/store;
// callers map from store.WorkerCredential to WorkerCredentialRef themselves.
type WorkerCredentialRef struct {
	WorkerID  string
	PublicKey string
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

	// serverSeed and serverPub are the broker's own nkey, generated at boot
	// and held only in memory, so that the broker's own connections (stream
	// provisioning, the scheduler's in-process client) can authenticate once
	// authorization is enabled. Empty when authorization is disabled.
	serverSeed []byte
	serverPub  string

	// bootOpts is a pristine copy of the options the server was started
	// with, retained so ReloadCredentials can clone from it rather than
	// reusing options nats-server has already consumed.
	bootOpts *natsserver.Options
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

	if b.cfg.Auth.Enabled {
		seed, pub, err := brokerauth.GenerateSeed()
		if err != nil {
			return fmt.Errorf("bus: generate server key: %w", err)
		}
		b.serverSeed, b.serverPub = seed, pub
		opts.Nkeys = b.buildNkeys(b.cfg.Auth.Credentials)
	}

	// Retain a pristine copy of the boot options for ReloadCredentials:
	// ReloadOptions documents that the Options passed to it must not be
	// reused, so credential reloads clone from this copy rather than the one
	// nats-server consumes below.
	b.bootOpts = opts.Clone()

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
	nc, err := nats.Connect(ns.ClientURL(), b.adminOptions()...)
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
	return NewClient(b.ns.ClientURL(), b.logger, b.adminOptions()...)
}

// buildNkeys converts the enrolled credential set into NATS nkey users, plus
// the server's own credential. The server key is generated at boot and lives
// only in memory: nothing else needs it, and persisting it would create a
// standing secret for no benefit.
func (b *Broker) buildNkeys(creds []WorkerCredentialRef) []*natsserver.NkeyUser {
	users := make([]*natsserver.NkeyUser, 0, len(creds)+1)
	users = append(users, &natsserver.NkeyUser{
		Nkey:        b.serverPub,
		Permissions: brokerauth.ServerPermissions(),
	})
	for _, c := range creds {
		users = append(users, &natsserver.NkeyUser{
			Nkey:        c.PublicKey,
			Permissions: brokerauth.WorkerPermissions(c.WorkerID),
		})
	}
	return users
}

// adminOptions returns the connect options for the broker's own admin
// connection, which provisions streams. Empty when auth is disabled.
func (b *Broker) adminOptions() []nats.Option {
	if !b.cfg.Auth.Enabled {
		return nil
	}
	return []nats.Option{
		nats.Nkey(b.serverPub, func(nonce []byte) ([]byte, error) {
			kp, err := nkeys.FromSeed(b.serverSeed)
			if err != nil {
				return nil, err
			}
			return kp.Sign(nonce)
		}),
	}
}

// ReloadCredentials replaces the enrolled worker set on a running broker.
//
// Revocation is synchronous, not eventually-consistent: nats-server's
// reloadAuthorization re-runs isClientAuthorized over every connected client
// and calls authViolation() on any that no longer pass, so a worker removed
// from creds is disconnected inside this call.
//
// ReloadOptions rejects changes to options that cannot be hot-swapped and
// documents that the Options passed to it must not be reused, so this clones
// the pristine boot options and mutates only Nkeys.
func (b *Broker) ReloadCredentials(creds []WorkerCredentialRef) error {
	if !b.cfg.Auth.Enabled {
		return errors.New("bus: broker authentication is disabled")
	}
	if b.ns == nil || b.bootOpts == nil {
		return errors.New("bus: broker not started")
	}
	opts := b.bootOpts.Clone()
	opts.Nkeys = b.buildNkeys(creds)
	if err := b.ns.ReloadOptions(opts); err != nil {
		return fmt.Errorf("bus: reload broker credentials: %w", err)
	}
	return nil
}

// ServerSeed returns the broker's own in-memory nkey seed so that in-process
// clients (the scheduler's bus.Client) can authenticate. Empty when auth is
// disabled.
func (b *Broker) ServerSeed() []byte { return b.serverSeed }
