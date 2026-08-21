// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
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

	// mu guards every field below. It exists because ReloadCredentials can
	// now be called from an HTTP handler goroutine (worker credential
	// revocation, see internal/server) concurrently with Shutdown on the
	// process's exit path — a combination that did not exist before that
	// caller, since previously only Start (single-threaded, before the
	// broker is handed to anything else) touched these fields.
	// Without it, Shutdown nilling ns/nc while ReloadCredentials or Check
	// reads them is a data race and a nil-pointer hazard, not just a
	// theoretical one.
	mu sync.Mutex

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

	var serverSeed []byte
	var serverPub string
	if b.cfg.Auth.Enabled {
		seed, pub, err := brokerauth.GenerateSeed()
		if err != nil {
			return fmt.Errorf("bus: generate server key: %w", err)
		}
		serverSeed, serverPub = seed, pub
		opts.Nkeys = buildNkeys(serverPub, b.cfg.Auth.Credentials, b.logger)
	}

	// Retain a pristine copy of the boot options for ReloadCredentials:
	// ReloadOptions documents that the Options passed to it must not be
	// reused, so credential reloads clone from this copy rather than the one
	// nats-server consumes below.
	bootOpts := opts.Clone()

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

	// Commit every field the other methods read, in one critical section, so
	// no concurrent caller can observe ns non-nil while serverSeed/
	// serverPub/bootOpts are still zero, or any other partially-updated
	// combination.
	b.mu.Lock()
	b.serverSeed, b.serverPub, b.bootOpts, b.ns = serverSeed, serverPub, bootOpts, ns
	b.mu.Unlock()

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
	b.mu.Lock()
	b.nc = nc
	b.mu.Unlock()

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
//
// It is also safe to call concurrently with [Broker.ReloadCredentials],
// [Broker.Check], [Broker.ClientURL] and [Broker.NewClient]: the fields
// those methods read are captured under mu
// and nilled here under the same lock, so a concurrent reader always sees
// either the pre-shutdown values or nil, never a torn or dangling pointer.
// The (possibly slow) nats-server calls below run after the lock is
// released, so Shutdown does not hold up an in-flight reload or health
// check any longer than it takes to copy two pointers.
func (b *Broker) Shutdown() {
	b.mu.Lock()
	nc := b.nc
	ns := b.ns
	b.nc = nil
	b.ns = nil
	b.mu.Unlock()

	if nc != nil {
		nc.Close()
	}
	if ns != nil {
		ns.Shutdown()
		ns.WaitForShutdown()
		b.logger.InfoContext(context.Background(), "bus: nats server stopped")
	}
}

// Check implements [health.Checker].  It returns nil when the embedded NATS
// server is running and the admin connection is open, or a descriptive error
// otherwise.  Registered with the health registry during server startup so
// that GET /readyz reflects broker health.
func (b *Broker) Check(_ context.Context) error {
	b.mu.Lock()
	ns, nc := b.ns, b.nc
	b.mu.Unlock()
	if ns == nil || !ns.Running() {
		return errors.New("nats server not running")
	}
	if nc == nil || nc.IsClosed() {
		return errors.New("nats admin connection closed")
	}
	return nil
}

// ClientURL returns the nats:// URL that in-process clients should connect to.
// Returns an empty string if the broker has not been started yet.
func (b *Broker) ClientURL() string {
	b.mu.Lock()
	ns := b.ns
	b.mu.Unlock()
	if ns == nil {
		return ""
	}
	return ns.ClientURL()
}

// NewClient dials the embedded NATS server and returns a connected [Client]
// configured for automatic reconnection.  It must be called after [Broker.Start]
// returns successfully.
//
// The returned Client should be shared across goroutines; create one per server
// component (or one shared instance for the whole server) rather than dialing
// repeatedly.
func (b *Broker) NewClient() (*Client, error) {
	b.mu.Lock()
	ns := b.ns
	b.mu.Unlock()
	if ns == nil {
		return nil, errors.New("bus: broker not started")
	}
	return NewClient(ns.ClientURL(), b.logger, b.adminOptions()...)
}

// buildNkeys converts the enrolled credential set into NATS nkey users, plus
// the server's own credential, identified by serverPub. It is a standalone
// function of its arguments rather than a *Broker method reading b.serverPub
// so that both Start (before it publishes serverPub to b) and
// ReloadCredentials (which reads it under b.mu itself) can call it without
// also needing to take the lock — and risk taking it twice on the same
// goroutine.
// A credential whose worker ID is not a single NATS subject token is SKIPPED
// and logged rather than installed. Both enrollment boundaries reject such an
// ID before it can be stored (internal/api's enroll handler and sqi-server's
// worker enroll command), so this is defense in depth for a row that arrived
// some other way — a hand-edited database, or a binary predating those
// checks. It matters because the worker ID becomes a subject PATTERN here:
// installing "*" would grant one credential "task.status.*.*",
// "worker.deregister.*", "work.lease.*.*" and the rest, letting it publish
// concrete subjects belonging to any worker on the farm; installing ">" (or
// an empty or whitespace-bearing ID) produces a malformed subject that
// nats-server refuses, which would fail every ReloadCredentials call — so
// revocation would return 500 forever — or stop the broker booting at all.
// Dropping the one bad row keeps every good credential working.
func buildNkeys(serverPub string, creds []WorkerCredentialRef, logger *slog.Logger) []*natsserver.NkeyUser {
	users := make([]*natsserver.NkeyUser, 0, len(creds)+1)
	users = append(users, &natsserver.NkeyUser{
		Nkey:        serverPub,
		Permissions: brokerauth.ServerPermissions(),
	})
	for _, c := range creds {
		if !brokerauth.ValidWorkerIDToken(c.WorkerID) {
			logger.WarnContext(
				context.Background(),
				"bus: skipping a worker credential whose worker id is not a valid NATS subject token",
				slog.String("worker_id", c.WorkerID),
				slog.String("impact", "this worker cannot connect; its grants would have been subject wildcards or a malformed subject"),
				slog.String("remediation", "revoke the credential and re-enroll the worker with an id containing no '.', whitespace, '*' or '>'"),
			)
			continue
		}
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
	b.mu.Lock()
	pub, seed := b.serverPub, b.serverSeed
	b.mu.Unlock()
	return []nats.Option{
		nats.Nkey(pub, func(nonce []byte) ([]byte, error) {
			kp, err := nkeys.FromSeed(seed)
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
//
// Safe to call concurrently with [Broker.Shutdown] — see its doc comment.
// nats-server's own ReloadOptions and Shutdown both take the embedded
// server's internal lock, so once this method has read a live *ns off b, the
// call into nats-server itself is safe even if Shutdown wins a concurrent
// race to nil out b.ns first: that only means this call observes "broker not
// started" instead, never a corrupted server.
func (b *Broker) ReloadCredentials(creds []WorkerCredentialRef) error {
	if !b.cfg.Auth.Enabled {
		return errors.New("bus: broker authentication is disabled")
	}
	b.mu.Lock()
	ns, bootOpts, serverPub := b.ns, b.bootOpts, b.serverPub
	b.mu.Unlock()
	if ns == nil || bootOpts == nil {
		return errors.New("bus: broker not started")
	}
	opts := bootOpts.Clone()
	opts.Nkeys = buildNkeys(serverPub, creds, b.logger)
	if err := ns.ReloadOptions(opts); err != nil {
		return fmt.Errorf("bus: reload broker credentials: %w", err)
	}
	return nil
}
