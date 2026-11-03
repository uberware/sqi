// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/tlsutil"
)

// EmbeddedConfig holds the parameters of the NATS server sqi-server hosts
// in-process.
type EmbeddedConfig struct {
	// Addr is the TCP address the embedded NATS server binds to, in
	// "host:port" form. Defaults to "0.0.0.0:4222" (all interfaces) so that
	// workers which discover the server via mDNS can reach the broker at the
	// advertised LAN host.
	//
	// Broker authentication is off by default. With it off, any host that
	// can reach this port can register as a worker and receive assignments,
	// on a LAN too, since the default binds all interfaces; sqi-server logs a
	// warning at startup when the address is not loopback. Set
	// nats.auth.enabled to require a per-worker credential, or bind this to
	// 127.0.0.1:4222 for single-machine use. See docs/auth.md.
	Addr string

	// DataDir is the directory JetStream uses for file-backed stream storage.
	// Created at startup if it does not exist.
	DataDir string

	// MaxStoreMB caps the SQI account's JetStream storage, in megabytes.
	// A value of 0 means unlimited (not recommended for production).
	MaxStoreMB int

	// TLS configures transport encryption on the client listener. Zero value
	// leaves the broker plaintext, which is the default.
	TLS BrokerTLSConfig

	// AdvertiseURL, when set, is the URL discovery advertises in place of
	// one built from the discovery host and the listener's bound port.
	AdvertiseURL string

	// Auth configures worker authentication through the auth callout. Zero
	// value leaves it off.
	Auth AuthConfig
}

// embeddedStreamReplicas is the replica count of every stream on the
// embedded broker, which is a single server.
const embeddedStreamReplicas = 1

// revokeSettleMargin is added to nats-server's auth timeout before
// [Embedded.Revoke]'s final pass, covering the moment between nats-server
// accepting a callout answer and placing the connection.
const revokeSettleMargin = 250 * time.Millisecond

// Embedded is the NATS server sqi-server hosts in-process, with JetStream in
// the SQI account. It implements [Broker].
//
// sqi's own two connections are in-process ([nats.InProcessServer]): the
// data connection in SQI, which provisions the streams and backs every
// [Client] handed out by [Embedded.NewClient], and, with auth on, the callout
// connection in AUTH, which answers authorization requests.
type Embedded struct {
	cfg    EmbeddedConfig
	logger *slog.Logger

	// mu guards every field below. Revoke and Check run on HTTP handler
	// goroutines concurrently with Shutdown on the process's exit path;
	// Shutdown nils these under mu, so a concurrent reader sees either the
	// running values or nil.
	mu sync.Mutex

	started bool

	ns      *natsserver.Server
	data    *nats.Conn // sqi's data connection (SQI account)
	callout *nats.Conn // the auth-callout responder's connection (AUTH); nil with auth off

	// responder answers authorization requests on callout; nil with auth off.
	responder *Responder

	// authTimeout is how long nats-server waits for a callout answer before
	// refusing the connection. Revoke waits it out.
	authTimeout time.Duration

	// dataSeed and dataPub are the data connection's nkey, generated at boot
	// and held only in memory. Every Client from NewClient presents it.
	dataSeed []byte
	dataPub  string
}

var _ Broker = (*Embedded)(nil)

// NewEmbedded creates an [Embedded] broker with the given configuration and
// logger. Call [Embedded.Start] to boot it.
func NewEmbedded(cfg EmbeddedConfig, logger *slog.Logger) *Embedded {
	return &Embedded{cfg: cfg, logger: logger}
}

// Identity returns "embedded:SQI": the embedded broker always keeps sqi's
// streams in the SQI account of its own data directory.
func (*Embedded) Identity() string {
	return "embedded:" + DefaultAccount
}

// embeddedKeys are the broker's own keys, generated at every boot and held
// only in memory. The issuer and callout keys exist only with auth on.
type embeddedKeys struct {
	issuer      nkeys.KeyPair
	issuerPub   string
	dataSeed    []byte
	dataPub     string
	calloutSeed []byte
	calloutPub  string
}

func newEmbeddedKeys(authEnabled bool) (embeddedKeys, error) {
	var k embeddedKeys
	var err error
	if k.dataSeed, k.dataPub, err = brokerauth.GenerateSeed(); err != nil {
		return k, fmt.Errorf("bus: generate data connection key: %w", err)
	}
	if !authEnabled {
		return k, nil
	}
	issuerSeed, issuerPub, err := brokerauth.GenerateAccountSeed()
	if err != nil {
		return k, fmt.Errorf("bus: generate callout issuer key: %w", err)
	}
	if k.issuer, err = nkeys.FromSeed(issuerSeed); err != nil {
		return k, fmt.Errorf("bus: load callout issuer key: %w", err)
	}
	k.issuerPub = issuerPub
	if k.calloutSeed, k.calloutPub, err = brokerauth.GenerateSeed(); err != nil {
		return k, fmt.Errorf("bus: generate callout connection key: %w", err)
	}
	return k, nil
}

// maxStoreBytes converts MaxStoreMB to bytes, with 0 meaning unlimited.
func (e *Embedded) maxStoreBytes() int64 {
	if e.cfg.MaxStoreMB <= 0 {
		return 0
	}
	return int64(e.cfg.MaxStoreMB) * 1024 * 1024
}

// serverOptions builds the nats-server options: listener, JetStream, the
// account layout, and TLS.
func (e *Embedded) serverOptions(layout Layout) (*natsserver.Options, error) {
	host, portStr, err := net.SplitHostPort(e.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("bus: parse addr %q: %w", e.cfg.Addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bus: parse port in %q: %w", e.cfg.Addr, err)
	}

	opts := &natsserver.Options{
		Host: host,
		Port: port,

		// JetStream is enabled for the server here and for the SQI account
		// by Layout.EnableAccountJetStream once the server is up. The
		// server-wide cap matches the account's, so nothing else can use
		// the disk sqi was given.
		JetStream:         true,
		StoreDir:          e.cfg.DataDir,
		JetStreamMaxStore: layout.maxStore(),

		// Prevent NATS from installing its own SIGINT/SIGTERM handler; sqi-server
		// owns signal handling and calls Shutdown() on the way out.
		NoSigs: true,

		// Suppress nats-server's built-in logger. startServer installs
		// natsLogger, and this file logs the broker's lifecycle events
		// through slog.
		NoLog: true,

		// Shutdown goes through lame duck mode; see stopServer. A zero grace
		// period would mean nats-server's 10s default.
		LameDuckGracePeriod: stopGrace,
		LameDuckDuration:    stopDuration,
	}
	if err := layout.Apply(opts); err != nil {
		return nil, err
	}

	if e.cfg.TLS.Enabled {
		tlsCfg, err := tlsutil.ServerConfig(e.cfg.TLS.CertFile, e.cfg.TLS.KeyFile, e.cfg.TLS.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("bus: tls: %w", err)
		}
		// Setting TLSConfig makes the broker require TLS: nats-server
		// computes `tlsReq := opts.TLSConfig != nil` and has no separate
		// flag. TLSTimeout defaults to 2s when left zero.
		opts.TLSConfig = tlsCfg
	}
	opts.AuthTimeout = defaultAuthTimeout(opts)
	return opts, nil
}

// defaultAuthTimeout is nats-server's own default auth timeout for o, in
// seconds: AUTH_TIMEOUT, or the TLS handshake timeout plus one second when
// TLS is on (server/opts.go, getDefaultAuthTimeout). sqi sets it explicitly
// because [Embedded.Revoke] relies on knowing it.
func defaultAuthTimeout(o *natsserver.Options) float64 {
	if o.TLSConfig == nil {
		return natsserver.AUTH_TIMEOUT.Seconds()
	}
	tlsTimeout := o.TLSTimeout
	if tlsTimeout == 0 {
		tlsTimeout = natsserver.TLS_TIMEOUT.Seconds()
	}
	return tlsTimeout + 1
}

// Start boots the embedded NATS server with sqi's account layout, enables
// JetStream on the SQI account, opens sqi's own connections, starts the
// auth-callout responder when auth is on, and provisions the streams.
//
// Start must be called before any other method and must not be called more
// than once.
func (e *Embedded) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("bus: broker already started")
	}
	e.started = true
	e.mu.Unlock()

	keys, err := newEmbeddedKeys(e.cfg.Auth.Enabled)
	if err != nil {
		return err
	}
	layout := Layout{
		Account:           DefaultAccount,
		AuthEnabled:       e.cfg.Auth.Enabled,
		IssuerPub:         keys.issuerPub,
		DataUserPub:       keys.dataPub,
		CalloutUserPub:    keys.calloutPub,
		JetStreamMaxStore: e.maxStoreBytes(),
	}
	opts, err := e.serverOptions(layout)
	if err != nil {
		return err
	}

	// Built before the server starts, so a bad auth configuration fails
	// Start without ever opening the listener.
	var responder *Responder
	if e.cfg.Auth.Enabled {
		responder, err = NewResponder(ResponderConfig{
			Issuer:  keys.issuer,
			Account: DefaultAccount,
			UserTTL: e.cfg.Auth.UserTTL,
			Lookup:  e.cfg.Auth.Lookup,
			Logger:  e.logger,
		})
		if err != nil {
			return err
		}
	}

	ns, err := e.startServer(ctx, opts, layout)
	if err != nil {
		return err
	}

	e.logger.InfoContext(
		ctx, "bus: nats server started",
		slog.String("addr", e.cfg.Addr),
		slog.String("data_dir", e.cfg.DataDir),
		slog.Int("max_store_mb", e.cfg.MaxStoreMB),
		slog.String("account", DefaultAccount),
	)

	own, err := connectOwn(ctx, ns, keys, responder)
	if err != nil {
		stopServer(ns)
		return err
	}

	if err := provisionStreams(ctx, own.data); err != nil {
		closeConns(own.data, own.callout)
		stopServer(ns)
		return err
	}

	e.mu.Lock()
	e.ns, e.data, e.callout = ns, own.data, own.callout
	e.responder = responder
	e.authTimeout = time.Duration(opts.AuthTimeout * float64(time.Second))
	e.dataSeed, e.dataPub = keys.dataSeed, keys.dataPub
	e.mu.Unlock()

	e.logger.InfoContext(
		ctx, "bus: jetstream streams provisioned",
		slog.String("streams", strings.Join(streamNames(), ",")),
	)
	return nil
}

// startServer creates and starts the server, waits until it accepts
// connections, and enables JetStream on the SQI account. JetStream must be on
// for the account before any client, sqi's own included, touches it.
func (e *Embedded) startServer(ctx context.Context, opts *natsserver.Options, layout Layout) (*natsserver.Server, error) {
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("bus: create nats server: %w", err)
	}

	nl := newNATSLogger(e.logger)
	ns.SetLoggerV2(nl, false, false, false)

	// Start is non-blocking; the server accepts connections asynchronously.
	ns.Start()

	if err := waitForNATS(ctx, ns, nl, 10*time.Second); err != nil {
		ns.Shutdown()
		return nil, err
	}
	// The listener is open from here. With auth off an anonymous worker can
	// connect before JetStream is on for SQI; its first registration publish
	// then gets no stream response, which the worker's registration retries.
	if err := layout.EnableAccountJetStream(ns); err != nil {
		stopServer(ns)
		return nil, err
	}
	return ns, nil
}

// ownConns are sqi's in-process connections to the embedded broker.
type ownConns struct {
	data    *nats.Conn
	callout *nats.Conn // nil with auth off
}

// connectOwn opens sqi's in-process connections: the data connection, and
// with auth on the callout connection, which starts answering authorization
// requests before connectOwn returns.
func connectOwn(ctx context.Context, ns *natsserver.Server, keys embeddedKeys, responder *Responder) (ownConns, error) {
	var own ownConns
	var err error
	own.data, err = nats.Connect(
		inProcessURL,
		nats.InProcessServer(ns),
		brokerauth.NkeyOption(keys.dataPub, keys.dataSeed),
		nats.Name("sqi-server data"),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return ownConns{}, fmt.Errorf("bus: data connection: %w", err)
	}
	if responder == nil {
		return own, nil
	}

	own.callout, err = nats.Connect(
		inProcessURL,
		nats.InProcessServer(ns),
		brokerauth.NkeyOption(keys.calloutPub, keys.calloutSeed),
		nats.Name("sqi-server auth callout"),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		own.data.Close()
		return ownConns{}, fmt.Errorf("bus: auth callout connection: %w", err)
	}
	if err := responder.Serve(ctx, own.callout); err != nil {
		closeConns(own.data, own.callout)
		return ownConns{}, err
	}
	return own, nil
}

// provisionStreams creates or updates sqi's streams over the data
// connection.
func provisionStreams(ctx context.Context, data *nats.Conn) error {
	js, err := jetstream.New(data)
	if err != nil {
		return fmt.Errorf("bus: jetstream context: %w", err)
	}
	if err := ensureStreams(ctx, js, embeddedStreamReplicas); err != nil {
		return fmt.Errorf("bus: provision streams: %w", err)
	}
	return nil
}

// stopGrace and stopDuration shape stopServer's lame duck shutdown: how long
// the server waits, with its listener closed, before closing its clients,
// and the time over which it closes them. Both are far inside the server's
// shutdown timeout.
const (
	stopGrace    = 10 * time.Millisecond
	stopDuration = 750 * time.Millisecond
)

// stopServer stops ns through lame duck mode and waits until it has exited.
//
// It works around a nats-server v2.15.2 bug: Shutdown stops the system
// account (shutdownEventing sets s.sys to nil) before it closes the clients
// it has accepted, and a CONNECT that needs the auth callout processed in
// between dereferences that nil (processClientOrLeafCallout → newRespInbox),
// crashing the process. With auth on, any connection attempt during
// shutdown, such as a worker reconnecting at its user JWT's expiry, would
// take sqi-server down before its store is closed.
//
// Lame duck mode closes the client listener and then every client before it
// shuts the server down, and a connection accepted after it began is dropped
// before it is registered or read (createClientEx checks the lame duck flag
// under the server lock). That narrows the window without closing it: a
// client whose read loop was already parsing a CONNECT when lame duck mode
// closed it can still reach the callout after the system account is gone.
// Closing it needs an upstream fix.
//
// The explicit Shutdown is required: lame duck mode returns without shutting
// down when it has nothing to do (no listener, or already in lame duck mode).
func stopServer(ns *natsserver.Server) {
	ns.LameDuckShutdown()
	ns.Shutdown()
	ns.WaitForShutdown()
}

func closeConns(conns ...*nats.Conn) {
	for _, nc := range conns {
		if nc != nil {
			nc.Close()
		}
	}
}

// Shutdown closes sqi's connections and stops the embedded NATS server,
// waiting until it has exited before returning. It is safe to call Shutdown
// more than once; later calls do nothing.
//
// With auth on, the callout subscription stops taking requests first and the
// authorizations already accepted are answered before the callout
// connection closes, for at most the responder's own per-request budget.
//
// It is also safe to call concurrently with Revoke, Check, ClientURL and
// NewClient: the fields those methods read are captured under mu and nilled
// here under the same lock. The (possibly slow) nats-server calls below run
// after the lock is released.
func (e *Embedded) Shutdown() {
	e.mu.Lock()
	ns, data, callout := e.ns, e.data, e.callout
	responder := e.responder
	e.ns, e.data, e.callout = nil, nil, nil
	e.responder = nil
	e.mu.Unlock()

	if responder != nil {
		responder.Stop()
	}
	closeConns(callout, data)
	if ns != nil {
		stopServer(ns)
		e.logger.InfoContext(context.Background(), "bus: nats server stopped")
	}
}

// Check implements [health.Checker]. It returns nil when the embedded NATS
// server is running, the data connection is connected and, with auth on, so
// is the callout connection: without it every worker connect is refused.
func (e *Embedded) Check(_ context.Context) error {
	e.mu.Lock()
	ns, data, callout := e.ns, e.data, e.callout
	e.mu.Unlock()
	if ns == nil || !ns.Running() {
		return errors.New("nats server not running")
	}
	if err := requireConnected("nats data connection", data); err != nil {
		return err
	}
	if e.cfg.Auth.Enabled {
		return requireConnected("nats auth callout connection", callout)
	}
	return nil
}

func requireConnected(name string, nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("%s not open", name)
	}
	if st := nc.Status(); st != nats.CONNECTED {
		return fmt.Errorf("%s is %s", name, st)
	}
	return nil
}

// ClientURL returns the URL network clients (workers) connect to. Returns an
// empty string if the broker has not been started yet. sqi-server's own
// connections do not use it; they are in-process.
func (e *Embedded) ClientURL() string {
	e.mu.Lock()
	ns := e.ns
	e.mu.Unlock()
	if ns == nil {
		return ""
	}
	return ns.ClientURL()
}

// AdvertiseURL returns [EmbeddedConfig.AdvertiseURL] when set, else
// nats:// (tls:// when the listener requires TLS) + host + the port the
// listener bound, read from the listener rather than the configuration.
func (e *Embedded) AdvertiseURL(host string) (string, error) {
	if e.cfg.AdvertiseURL != "" {
		return e.cfg.AdvertiseURL, nil
	}
	e.mu.Lock()
	ns := e.ns
	e.mu.Unlock()
	if ns == nil {
		return "", errors.New("bus: broker not started")
	}
	addr, ok := ns.Addr().(*net.TCPAddr)
	if !ok || addr.Port == 0 {
		return "", fmt.Errorf("bus: cannot determine the embedded broker's listener port (nats.embedded.addr %q); "+
			"set nats.embedded.advertise_url or a fixed port", e.cfg.Addr)
	}
	scheme := "nats"
	if e.cfg.TLS.Enabled {
		scheme = "tls"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(addr.Port)), nil
}

// NewClient returns a connected [Client] on the data connection's credential,
// in-process, configured for automatic reconnection. It must be called after
// [Embedded.Start] returns successfully.
//
// The returned Client should be shared across goroutines; create one per server
// component (or one shared instance for the whole server) rather than dialing
// repeatedly.
func (e *Embedded) NewClient() (*Client, error) {
	e.mu.Lock()
	ns, seed, pub := e.ns, e.dataSeed, e.dataPub
	e.mu.Unlock()
	if ns == nil {
		return nil, errors.New("bus: broker not started")
	}
	return NewClient(inProcessURL, e.logger, nats.InProcessServer(ns), brokerauth.NkeyOption(pub, seed))
}

// RevokeBudget is how long [Embedded.Revoke] may need: the auth timeout and
// the settle margin, plus slack for a slow host. Zero before Start.
func (e *Embedded) RevokeBudget() time.Duration {
	e.mu.Lock()
	authTimeout := e.authTimeout
	e.mu.Unlock()
	if authTimeout == 0 {
		return 0
	}
	return authTimeout + revokeSettleMargin + revokeBudgetSlack
}

// Revoke disconnects every connection authorized as workerID. The callout
// names each worker connection after its worker ID, so the connections are
// found by that user name in the SQI account.
//
// Call it after the credential is revoked in the store. When Revoke returns
// nil, the broker holds no connection for workerID whose authorization began
// before Revoke did:
//
//   - the first pass, at once, disconnects every connection present;
//   - the final pass runs once nats-server's auth timeout (plus
//     [revokeSettleMargin]) has elapsed since Revoke began. nats-server
//     abandons a callout after that timeout, so every authorization that was
//     already in flight, perhaps on a store read from before the revocation,
//     has by then either placed its connection, which this pass disconnects,
//     or been refused.
//
// Revoke therefore takes about the auth timeout (2s, or 3s with TLS). If ctx
// ends first it returns ctx's error after the first pass only. Any reconnect
// is authorized afresh against the store and refused.
func (e *Embedded) Revoke(ctx context.Context, workerID string) error {
	began := time.Now()
	// An empty user filter would match every connection in the account,
	// sqi's own data connection included.
	if workerID == "" {
		return errors.New("bus: revoke: empty worker id")
	}
	e.mu.Lock()
	ns, authTimeout := e.ns, e.authTimeout
	e.mu.Unlock()
	if ns == nil {
		return errors.New("bus: broker not started")
	}

	if err := disconnectUser(ns, workerID); err != nil {
		return err
	}

	if err := awaitRevokeSettle(ctx, began, authTimeout, workerID); err != nil {
		return err
	}
	return disconnectUser(ns, workerID)
}

// awaitRevokeSettle waits until authTimeout plus [revokeSettleMargin] has
// passed since a revocation of workerID began, when every authorization that
// was in flight has either placed its connection or been refused, so the
// final pass leaves no window. It returns early with ctx's error.
func awaitRevokeSettle(ctx context.Context, began time.Time, authTimeout time.Duration, workerID string) error {
	settle := time.NewTimer(time.Until(began.Add(authTimeout + revokeSettleMargin)))
	defer settle.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("bus: revoke %s: %w", workerID, ctx.Err())
	case <-settle.C:
		return nil
	}
}

// disconnectUser closes every open connection of user in the SQI account.
func disconnectUser(ns *natsserver.Server, user string) error {
	cz, err := ns.Connz(&natsserver.ConnzOptions{
		Username: true,
		User:     user,
		Account:  DefaultAccount,
		Limit:    math.MaxInt32,
	})
	if err != nil {
		return fmt.Errorf("bus: revoke %s: list connections: %w", user, err)
	}
	for _, ci := range cz.Conns {
		// An error means the connection is already gone, which is the
		// outcome wanted.
		_ = ns.DisconnectClientByID(ci.Cid) //nolint:errcheck // see above
	}
	return nil
}
