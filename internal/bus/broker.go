// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"crypto/tls"
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

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/tlsutil"
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

	// TLS configures transport encryption on the client listener. Zero value
	// leaves the broker plaintext, which is the default.
	TLS BrokerTLSConfig
}

// buildBrokerTLS assembles the broker's server-side TLS configuration.
// The paths have already been validated at config load, so a failure here
// means the files changed underneath a running process.
//
// Note what is NOT here: the server's own connections need no client-side TLS
// configuration at all, because they never speak TLS. See adminOptions.
func buildBrokerTLS(cfg BrokerTLSConfig) (*tls.Config, error) {
	tlsCfg, err := tlsutil.ServerConfig(cfg.CertFile, cfg.KeyFile, cfg.ClientCAFile)
	if err != nil {
		return nil, err
	}
	return tlsCfg, nil
}

// BrokerTLSConfig controls TLS on the embedded broker's client listener.
// Zero value leaves TLS off, which is the default.
//
// This is bus's own view of the operator's nats.tls block, mapped by the
// caller, so that internal/bus does not import internal/config — the same
// boundary WorkerCredentialRef draws against internal/store.
type BrokerTLSConfig struct {
	// Enabled makes the broker require TLS on every client connection.
	Enabled bool

	// CertFile and KeyFile are the PEM certificate and key the broker
	// presents. Both are validated at config load.
	CertFile string
	KeyFile  string

	// ClientCAFile, when set, requires each connecting worker to present a
	// client certificate signed by this CA. It is a transport control
	// layered on the nkey credential, never a substitute: the certificate
	// decides who may open a connection, the nkey decides which worker they
	// are.
	ClientCAFile string
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

	if b.cfg.TLS.Enabled {
		tlsCfg, err := buildBrokerTLS(b.cfg.TLS)
		if err != nil {
			return fmt.Errorf("bus: tls: %w", err)
		}
		// Setting TLSConfig is what makes the broker REQUIRE TLS:
		// nats-server computes `tlsReq := opts.TLSConfig != nil`. There is no
		// separate flag. TLSTimeout self-defaults to 2s when left zero.
		//
		// This must happen BEFORE the bootOpts clone below: ReloadCredentials
		// rebuilds from that clone, so a TLSConfig set afterwards would be
		// dropped the first time a worker is revoked — silently taking the
		// whole broker back to plaintext.
		opts.TLSConfig = tlsCfg
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
	nc, err := nats.Connect(inProcessURL, b.adminOptions(ns)...)
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
	return NewClient(inProcessURL, b.logger, b.adminOptions(ns)...)
}

// buildNkeys converts the enrolled credential set into NATS nkey users, plus
// the server's own credential, identified by serverPub. It is a standalone
// function of its arguments rather than a *Broker method reading b.serverPub
// so that both Start (before it publishes serverPub to b) and
// ReloadCredentials (which reads it under b.mu itself) can call it without
// also needing to take the lock — and risk taking it twice on the same
// goroutine.
// A credential whose worker ID is not a single NATS subject token, or whose
// public key is not a valid nkey, is SKIPPED and logged rather than
// installed. Both enrollment boundaries reject such rows before they can be
// stored (internal/api's enroll handler and sqi-server's worker enroll
// command validate both the worker ID and the public key), so this is
// defense in depth for a row that arrived some other way — a hand-edited
// database, or a binary predating those checks. The worker-ID case matters
// because the worker ID becomes a subject PATTERN here: installing "*" would
// grant one credential "task.status.*.*", "worker.deregister.*",
// "work.lease.*.*" and the rest, letting it publish concrete subjects
// belonging to any worker on the farm; installing ">" (or an empty or
// whitespace-bearing ID) produces a malformed subject that nats-server
// refuses. The public-key case matters because an nkey user with a
// malformed key is itself an option natsserver.NewServer rejects outright —
// without this guard, one bad row takes the whole broker down rather than
// costing the one worker it belongs to; with it, every good credential keeps
// working.
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
		if err := brokerauth.ValidatePublicKey(c.PublicKey); err != nil {
			logger.WarnContext(
				context.Background(),
				"bus: skipping a worker credential whose public key is not a valid nkey",
				slog.String("worker_id", c.WorkerID),
				slog.Any("error", err),
				slog.String("impact", "this worker cannot connect; an invalid key here would otherwise stop the whole broker from starting"),
				slog.String("remediation", "revoke the credential and re-enroll the worker with a key generated by sqi-worker keygen"),
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

// adminOptions returns the connect options the server's OWN connections use —
// the admin connection that provisions streams, and every Client handed out by
// [Broker.NewClient].
//
// These connections go in-process (nats.InProcessServer) rather than over the
// loopback TCP listener. That is what lets broker TLS be turned on without the
// server having to trust its own certificate to talk to itself: nats-server
// clears info.TLSRequired for in-process connections (server/server.go:3296).
//
// Authentication is unaffected — an in-process connection still traverses the
// normal client auth path, so the server nkey is presented and checked exactly
// as before.
func (b *Broker) adminOptions(ns *natsserver.Server) []nats.Option {
	opts := []nats.Option{nats.InProcessServer(ns)}

	b.mu.Lock()
	pub, seed := b.serverPub, b.serverSeed
	b.mu.Unlock()

	if !b.cfg.Auth.Enabled {
		return opts
	}
	return append(opts, brokerauth.NkeyOption(pub, seed))
}

// inProcessURL is the URL the server's own connections dial: none.
//
// It is deliberately empty even though nats.InProcessServer makes the URL
// unused for transport, because nats.go still PARSES it — and
// Server.ClientURL() returns a "tls://" scheme once TLS is enabled, which makes
// the client negotiate TLS over what is only a net.Pipe inside this process.
// That in turn would demand the server trust its own certificate, for a
// connection that never touches a socket.
//
// Passing no URL avoids all of it: nats-server clears info.TLSRequired for
// in-process connections (server/server.go:3296), so the connection is plain
// and authentication still applies — an in-process client presents the server
// nkey and is checked exactly like any other. Verified against a broker with
// TLS, mutual TLS and nkey auth all enabled.
const inProcessURL = ""

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
