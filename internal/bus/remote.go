// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/redact"
	"github.com/uberware/sqi/internal/tlsutil"
)

// RemoteConfig holds the parameters of an operator-run NATS server or
// cluster sqi-server connects to.
//
// It is bus's own view of the operator's nats.external block, which
// internal/config validates. The caller maps it, so that internal/bus does not
// import internal/config.
type RemoteConfig struct {
	// URLs are the broker URLs, all tls:// or all nats://.
	URLs []string

	// AdvertiseURLs, when set, are what discovery tells workers to use in
	// place of URLs; only the first is advertised.
	AdvertiseURLs []string

	// Account is the dedicated account sqi's streams and workers live in.
	// The data connection's credential must place it there.
	Account string

	// NkeySeedFile and CredsFile are the data connection's credential, at
	// most one of them. Neither means an unauthenticated account.
	NkeySeedFile string
	CredsFile    string

	// StreamReplicas is the replica count of every stream: 1, 3 or 5.
	StreamReplicas int

	// ConnectTimeout bounds startup: reaching a broker and finding JetStream
	// on Account. Reconnects after startup are unbounded.
	ConnectTimeout time.Duration

	// TLS is the client-side TLS configuration of every connection sqi makes.
	TLS ClientTLSConfig

	// Auth configures worker authentication through the auth callout.
	Auth RemoteAuthConfig

	// InstanceID is this server's member instance id in an HA group, the one
	// it records as a member of its database. It names the server in the
	// account guard, so that the next leader recognizes a key a crashed
	// leader left as a member's. [Remote.StartStandby] requires it. Empty
	// gives every guard acquisition a fresh id.
	InstanceID string
}

// ClientTLSConfig is the client side of TLS to an external broker. The zero
// value adds nothing: a tls:// URL then verifies the broker against the
// system roots.
type ClientTLSConfig struct {
	// CAFile verifies the broker's certificate in place of the system roots.
	CAFile string
	// CertFile and KeyFile are a client certificate, for brokers that
	// require mutual TLS. Both or neither.
	CertFile string
	KeyFile  string
	// ServerName overrides the name the broker's certificate is checked
	// against, which is otherwise the host of the URL dialed.
	ServerName string
}

// RemoteAuthConfig is [AuthConfig] plus the keys the auth callout needs on
// an external broker.
type RemoteAuthConfig struct {
	AuthConfig

	// CalloutNkeySeedFile is the callout connection's user, in the AUTH
	// account.
	CalloutNkeySeedFile string
	// IssuerSeedFile is the account nkey seed callout responses are signed
	// with.
	IssuerSeedFile string
	// SystemCredsFile is an optional system-account user's nkey seed. With
	// it, [Remote.Revoke] finds a worker's connections on every node and
	// kicks them; without it, a revoked worker stays connected until its
	// user JWT expires, UserTTL at the most.
	//
	// It is a raw nkey seed, not a .creds JWT file: in the non-operator
	// account layout sqi uses, nats-server authenticates nkey users by their
	// signature alone and ignores a user JWT.
	SystemCredsFile string
}

// remoteReconnectWait is the pause between reconnect rounds on the data
// connection. Each round tries every known server before pausing.
const remoteReconnectWait = time.Second

// startupRetryWait is the pause between JetStream requests at startup while a
// broker that has just started is still bringing JetStream up.
const startupRetryWait = 250 * time.Millisecond

// brokerVersion reads the nats-server version of the server nc is connected
// to. It is a variable so a test can report a version the in-process broker
// is not.
var brokerVersion = func(nc *nats.Conn) string { return nc.ConnectedServerVersion() }

// systemReplyWindow is how long one request to every node of the cluster
// (a $SYS.REQ.SERVER.PING) collects replies. Every node answers, and nothing
// says how many nodes there are, so the window is what ends the collection.
const systemReplyWindow = time.Second

// kickTimeout bounds one $SYS.REQ.SERVER.<id>.KICK request.
const kickTimeout = time.Second

// minReplyBuffer is the smallest reply buffer of a request to every node.
// The buffer holds four replies per node known, so a cluster that has grown
// since sqi last counted it still fits.
const minReplyBuffer = 64

// System request subjects (nats-server server/events.go).
const (
	connzPingSubject = "$SYS.REQ.SERVER.PING.CONNZ"
	varzPingSubject  = "$SYS.REQ.SERVER.PING.VARZ"
	kickSubjectFmt   = "$SYS.REQ.SERVER.%s.KICK"
)

// Remote is an operator-run NATS server or cluster sqi-server connects to. It
// implements [Broker].
//
// sqi holds one data connection, in the configured account, which provisions
// the streams and whose dial options every [Client] from [Remote.NewClient]
// reuses. With worker authentication on it also holds the callout
// connection, in the AUTH account, which answers the cluster's authorization
// requests, and, when a system credential is configured, a system-account
// connection that revocation kicks workers through.
type Remote struct {
	cfg    RemoteConfig
	logger *slog.Logger

	// mu guards every field below. Check runs on HTTP handler goroutines
	// concurrently with Shutdown; Shutdown nils data under mu.
	mu sync.Mutex

	started bool

	data    *nats.Conn // sqi's data connection
	callout *nats.Conn // the auth-callout responder's connection (AUTH); nil with auth off
	system  *nats.Conn // the system-account connection; nil without a system credential

	// responder answers authorization requests on callout; nil with auth off.
	responder *Responder

	// cluster is what the nodes have reported through the system
	// connection: at Start, then raised by every Revoke. Zero without a
	// system credential.
	cluster clusterView

	// connzSubject and varzSubject are the system requests Revoke sends.
	// Tests replace them to simulate nodes that do not answer.
	connzSubject, varzSubject string

	// dialOpts are the credential and TLS options, built once at Start and
	// shared by the data connection and every Client.
	dialOpts []nats.Option

	// identity is the broker's identity, set once at Start.
	identity string

	// guard is the one-server-per-account lease taken at Start; nil before it.
	guard *Guard

	// noGuard skips the guard at Start. It is a test seam, set only through
	// DisableRemoteGuard in export_test.go by tests that run two servers'
	// responders on one cluster; running without the guard is not a supported
	// mode.
	noGuard bool

	// standby is set by StartStandby; leading is set while TakeLeadership
	// runs and stays set once it succeeds.
	standby, leading bool

	// dataUp records when the data connection last came up.
	dataUp connState

	// standbyErr is the outcome of a standby's broker checks:
	// ErrStandbyUnchecked until they have run, then nil or why they failed.
	// standbyChecked is closed when it is final.
	standbyErr     error
	standbyChecked chan struct{}

	// calloutOpen is set once the responder may answer: at Start, or when a
	// standby's checks pass. calloutWanted is what SetCalloutActive last
	// asked for. calloutMu orders SetCalloutActive with the opening.
	calloutOpen, calloutWanted bool
	calloutMu                  sync.Mutex

	// checkCancel stops a standby's background check, and checkDone is
	// closed when it has returned; both nil when there is none.
	checkCancel context.CancelFunc
	checkDone   chan struct{}
}

// connState records when a connection was last established. Its handlers run
// in the order nats.go reports the events, on its callback goroutine. Every
// method but get does nothing on a nil *connState, which a connection whose
// state nobody reads is given.
type connState struct {
	mu sync.Mutex
	// since is when the connection was last established or re-established;
	// zero while it is down.
	since time.Time
	// events counts the handlers that have run, so that a connect recorded
	// after the fact does not undo one of them.
	events int
}

// up records a reconnect.
func (s *connState) up() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	s.since = time.Now().UTC()
}

// connected records the first connect, reported by nats.go's connect
// handler. The connect may already be recorded, by [connState.initialUp].
func (s *connState) connected() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	if s.since.IsZero() {
		s.since = time.Now().UTC()
	}
}

func (s *connState) down() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	s.since = time.Time{}
}

// initialUp records the first connect, found connected when nats.Connect
// returned, unless a handler has run: what it recorded is newer.
func (s *connState) initialUp() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == 0 && s.since.IsZero() {
		s.since = time.Now().UTC()
	}
}

func (s *connState) get() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.since
}

var _ Broker = (*Remote)(nil)

// NewRemote creates a [Remote] broker with the given configuration and
// logger. Call [Remote.Start] to connect.
func NewRemote(cfg RemoteConfig, logger *slog.Logger) *Remote {
	cfg.URLs = slices.Clone(cfg.URLs)
	cfg.AdvertiseURLs = slices.Clone(cfg.AdvertiseURLs)
	return &Remote{
		cfg: cfg, logger: logger, connzSubject: connzPingSubject, varzSubject: varzPingSubject,
		standbyErr: ErrStandbyUnchecked, standbyChecked: make(chan struct{}), calloutWanted: true,
	}
}

// AdvertiseURL returns the first [RemoteConfig.AdvertiseURLs] entry, else
// the first of [RemoteConfig.URLs]. host is unused: the cluster's URLs are
// its own.
func (r *Remote) AdvertiseURL(string) (string, error) {
	if len(r.cfg.AdvertiseURLs) > 0 {
		return r.cfg.AdvertiseURLs[0], nil
	}
	if len(r.cfg.URLs) > 0 {
		return r.cfg.URLs[0], nil
	}
	return "", errors.New("bus: external broker has no URL to advertise")
}

// Identity returns "external:<account>@<cluster>", the cluster name being
// the one the broker reports. A broker that is not clustered reports none, so
// its server name stands in when the operator set one. Before Start, and for
// a standalone broker with no server name, it is "external:<account>".
func (r *Remote) Identity() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.identity != "" {
		return r.identity
	}
	return remoteIdentity(r.cfg.Account, "", "", "")
}

// remoteIdentity names the broker an account lives on: its cluster, or for a
// standalone server its configured name. nats-server reports a server with no
// configured name under its ID, which is regenerated at every start, so that
// name is not used.
func remoteIdentity(account, cluster, serverName, serverID string) string {
	id := "external:" + account
	switch {
	case cluster != "":
		return id + "@" + cluster
	case serverName != "" && serverName != serverID:
		return id + "@" + serverName
	default:
		return id
	}
}

func (cfg RemoteConfig) validate() error {
	switch {
	case len(cfg.URLs) == 0:
		return errors.New("bus: external broker: no URLs")
	case cfg.Account == "":
		return errors.New("bus: external broker: account is empty")
	case cfg.StreamReplicas < 1:
		return fmt.Errorf("bus: external broker: stream replicas %d is not positive", cfg.StreamReplicas)
	case cfg.ConnectTimeout <= 0:
		return fmt.Errorf("bus: external broker: connect timeout %s is not positive", cfg.ConnectTimeout)
	case cfg.NkeySeedFile != "" && cfg.CredsFile != "":
		return errors.New("bus: external broker: set an nkey seed file or a creds file, not both")
	}
	return cfg.Auth.validate()
}

// validate checks what worker authentication needs before anything is
// dialed. The responder checks the issuer key itself.
func (a RemoteAuthConfig) validate() error {
	switch {
	case !a.Enabled:
		return nil
	case a.CalloutNkeySeedFile == "":
		return errors.New("bus: external broker: worker authentication needs the callout connection's nkey seed file")
	case a.IssuerSeedFile == "":
		return errors.New("bus: external broker: worker authentication needs the callout issuer's account seed file")
	}
	return nil
}

// dialOptions builds the data connection's credential and TLS options,
// which every Client shares.
func (cfg RemoteConfig) dialOptions() ([]nats.Option, error) {
	switch {
	case cfg.NkeySeedFile != "":
		return cfg.seedDialOptions("data", cfg.NkeySeedFile)
	case cfg.CredsFile != "":
		return cfg.withTLS([]nats.Option{nats.UserCredentials(cfg.CredsFile)})
	}
	return cfg.withTLS(nil)
}

// seedDialOptions builds the options of one of sqi's other connections: the
// nkey user in seedFile, and the shared TLS options.
func (cfg RemoteConfig) seedDialOptions(role, seedFile string) ([]nats.Option, error) {
	opt, err := nats.NkeyOptionFromSeed(seedFile)
	if err != nil {
		return nil, fmt.Errorf("bus: external broker: %s connection nkey seed: %w", role, err)
	}
	return cfg.withTLS([]nats.Option{opt})
}

// withTLS appends the client TLS options to opts.
func (cfg RemoteConfig) withTLS(opts []nats.Option) ([]nats.Option, error) {
	tlsCfg, err := cfg.TLS.build()
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		opts = append(opts, nats.Secure(tlsCfg))
	}
	return opts, nil
}

// loadIssuer reads the account seed callout answers are signed with.
func loadIssuer(path string) (nkeys.KeyPair, error) {
	seed, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bus: external broker: callout issuer seed: %w", err)
	}
	kp, err := nkeys.FromSeed([]byte(strings.TrimSpace(string(seed))))
	if err != nil {
		return nil, fmt.Errorf("bus: external broker: callout issuer seed %s: %w", path, err)
	}
	return kp, nil
}

// build returns the client TLS configuration, or nil when nothing is set, so
// that the URL scheme alone decides.
func (c ClientTLSConfig) build() (*tls.Config, error) {
	if c == (ClientTLSConfig{}) {
		return nil, nil //nolint:nilnil // nil means "no TLS options", which is not an error
	}
	cfg, err := tlsutil.ClientConfig(c.CAFile, c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("bus: external broker tls: %w", err)
	}
	cfg.ServerName = c.ServerName
	return cfg, nil
}

// urlList joins the URLs for nats.Connect, which splits on commas.
func (cfg RemoteConfig) urlList() string {
	return strings.Join(cfg.URLs, ",")
}

// redactedURLs lists the URLs with any credentials replaced, for messages.
func (cfg RemoteConfig) redactedURLs() string {
	return redact.URLs(cfg.URLs, ", ")
}

// Start connects to the broker, checks its version, checks that JetStream is
// on for the account, and takes the account's one-server guard, so a second
// sqi-server on the same account fails Start with [ErrGuardHeld] before it
// touches anything the first one owns. It then creates or updates sqi's
// streams with the configured replica count. With worker authentication on it
// then starts answering the cluster's authorization requests and, when a
// system credential is configured, opens the system connection revocation
// uses. It must be called before any other method and must not be called more
// than once. An HA server calls [Remote.StartStandby] instead.
func (r *Remote) Start(ctx context.Context) error {
	if err := r.markStarted(); err != nil {
		return err
	}
	if err := r.cfg.validate(); err != nil {
		return err
	}
	r.warnLooseSeedFiles(ctx)
	dialOpts, err := r.cfg.dialOptions()
	if err != nil {
		return err
	}
	// Built before anything is dialed, so a bad auth configuration fails
	// Start without touching the cluster.
	responder, err := r.newResponder()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(r.cfg.ConnectTimeout)

	data, err := r.dial(ctx, roleData, &r.dataUp, dialOpts, deadline)
	if err != nil {
		return err
	}
	identity, guard, err := r.provision(ctx, data, deadline)
	if err != nil {
		data.Close()
		return err
	}
	auth, err := r.startAuth(ctx, responder, deadline)
	if err != nil {
		r.releaseGuard(guard)
		data.Close()
		return err
	}

	r.mu.Lock()
	r.data, r.dialOpts, r.identity, r.guard = data, dialOpts, identity, guard
	r.callout, r.system = auth.callout, auth.system
	r.responder = responder
	r.cluster = auth.cluster
	r.calloutOpen = true
	r.mu.Unlock()
	r.finishStandbyCheck(nil)

	r.logger.InfoContext(
		ctx, "bus: connected to external nats",
		slog.String("url", redact.URL(data.ConnectedUrl())),
		slog.String("server_version", brokerVersion(data)),
		slog.String("cluster", data.ConnectedClusterName()),
		slog.String("account", r.cfg.Account),
		slog.Int("stream_replicas", r.cfg.StreamReplicas),
		slog.String("identity", identity),
		slog.Bool("worker_auth", r.cfg.Auth.Enabled),
		slog.Bool("immediate_revocation", auth.system != nil),
	)
	return nil
}

// markStarted records that Start or StartStandby has been called, and fails
// on a second call.
func (r *Remote) markStarted() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return errors.New("bus: broker already started")
	}
	r.started = true
	return nil
}

// seedFile is one of the private-key files sqi-server reads to reach an
// external broker, with what a reader of it could do.
type seedFile struct {
	path, holds, impact string
}

// seedFiles lists the private-key files this configuration reads: the data
// connection's credential, and with worker authentication on the callout's,
// the issuer's and the system account's.
func (cfg RemoteConfig) seedFiles() []seedFile {
	const asServer = "anyone who can read it can connect to the broker as sqi-server, in sqi's account"
	files := []seedFile{
		{cfg.NkeySeedFile, "the data connection's nkey seed", asServer},
		{cfg.CredsFile, "the data connection's credentials", asServer},
	}
	if cfg.Auth.Enabled {
		files = append(
			files,
			seedFile{
				cfg.Auth.CalloutNkeySeedFile, "the auth callout connection's nkey seed",
				"anyone who can read it can answer the cluster's authorization requests",
			},
			seedFile{
				cfg.Auth.IssuerSeedFile, "the callout issuer's account seed",
				"anyone who can read it can sign a broker credential for any worker",
			},
			seedFile{
				cfg.Auth.SystemCredsFile, "the system account user's nkey seed",
				"anyone who can read it can act as a system-account user on the cluster",
			},
		)
	}
	return files
}

// warnLooseSeedFiles logs a WARN for each seed file that accounts other than
// its owner can read, as [brokerauth.LoadSeed] checks a worker's seed. It
// warns rather than refuses: these files are provisioned and managed by the
// operator, often by a secrets mount sqi does not control. A symlink is
// judged by the file it points to. A file that cannot be checked is left to
// the read that fails Start.
func (r *Remote) warnLooseSeedFiles(ctx context.Context) {
	for _, f := range r.cfg.seedFiles() {
		if f.path == "" {
			continue
		}
		target, err := filepath.EvalSymlinks(f.path)
		if err != nil {
			continue
		}
		if restricted, err := fsutil.IsRestricted(target); err != nil || restricted {
			continue
		}
		r.logger.WarnContext(ctx, "bus: an external broker seed file is readable beyond its owner",
			slog.String("file", f.path),
			slog.String("holds", f.holds),
			slog.String("impact", f.impact),
			slog.String("remediation", brokerauth.RestrictHint(f.path)))
	}
}

// acquireGuard takes the broker account's one-server guard, so that a second
// sqi-server pointed at the same account is refused. purpose is the holder's
// [GuardHolder.Purpose], "" for a server. Only a server brings the guard
// bucket's replica count to its configuration; any other holder leaves the
// bucket as the server last configured it. A server holds the guard under
// [RemoteConfig.InstanceID] when one is set.
func (r *Remote) acquireGuard(ctx context.Context, js jetstream.JetStream, deadline time.Time, purpose string, opts GuardOptions) (*Guard, error) {
	if r.noGuard {
		return nil, nil
	}
	host, _ := os.Hostname() //nolint:errcheck // the host is only a label in the holder's identity
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	id := NewInstanceID()
	if purpose == "" && r.cfg.InstanceID != "" {
		id = r.cfg.InstanceID
	}
	h := GuardHolder{InstanceID: id, Host: host, PID: os.Getpid(), StartedAt: time.Now(), Purpose: purpose}
	return acquireGuard(ctx, js, r.cfg.StreamReplicas, h, r.logger, purpose == "", opts)
}

// releaseGuard gives the account's guard up, within a bounded time.
func (r *Remote) releaseGuard(g *Guard) {
	if g == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), guardOpTimeout)
	defer cancel()
	if err := g.Release(ctx); err != nil {
		r.logger.WarnContext(ctx, "bus: could not release the broker account guard",
			slog.Any("error", err),
			slog.String("impact", "another sqi-server cannot start on this account until the guard expires"))
	}
}

// newResponder builds the callout responder, or returns nil with auth off.
func (r *Remote) newResponder() (*Responder, error) {
	if !r.cfg.Auth.Enabled {
		return nil, nil //nolint:nilnil // no responder with auth off is not an error
	}
	issuer, err := loadIssuer(r.cfg.Auth.IssuerSeedFile)
	if err != nil {
		return nil, err
	}
	return NewResponder(ResponderConfig{
		Issuer:  issuer,
		Account: r.cfg.Account,
		UserTTL: r.cfg.Auth.UserTTL,
		Lookup:  r.cfg.Auth.Lookup,
		Logger:  r.logger,
	})
}

// remoteAuth is what [Remote.startAuth] opened.
type remoteAuth struct {
	callout *nats.Conn
	system  *nats.Conn
	cluster clusterView
}

func (a remoteAuth) close() {
	closeConns(a.system, a.callout)
}

// startAuth opens the callout connection and starts the responder on it,
// then opens the system connection when one is configured. With auth off it
// opens nothing.
func (r *Remote) startAuth(ctx context.Context, responder *Responder, deadline time.Time) (remoteAuth, error) {
	var a remoteAuth
	if responder == nil {
		return a, nil
	}
	opts, err := r.cfg.seedDialOptions("auth callout", r.cfg.Auth.CalloutNkeySeedFile)
	if err != nil {
		return a, err
	}
	if a.callout, err = r.dial(ctx, roleCallout, nil, opts, deadline); err != nil {
		return a, err
	}
	if err := responder.Serve(ctx, a.callout); err != nil {
		a.close()
		return remoteAuth{}, err
	}

	if r.cfg.Auth.SystemCredsFile == "" {
		return a, nil
	}
	if opts, err = r.cfg.seedDialOptions("system", r.cfg.Auth.SystemCredsFile); err != nil {
		a.close()
		return remoteAuth{}, err
	}
	if a.system, err = r.dial(ctx, roleSystem, nil, opts, deadline); err != nil {
		a.close()
		return remoteAuth{}, err
	}
	if a.cluster, err = askCluster(ctx, a.system, varzPingSubject, clusterView{}); err != nil {
		a.close()
		return remoteAuth{}, err
	}
	return a, nil
}

// clusterView is what sqi knows of the cluster's nodes.
type clusterView struct {
	// nodes is how many nodes answered a request to every node.
	nodes int
	// authTimeout is the longest auth timeout any node reported: how long
	// a node waits for a callout answer before refusing the connection.
	authTimeout time.Duration
}

// merge keeps the larger of each, so a node that stops answering never
// lowers what Revoke waits for or how many answers it requires.
func (v clusterView) merge(o clusterView) clusterView {
	return clusterView{nodes: max(v.nodes, o.nodes), authTimeout: max(v.authTimeout, o.authTimeout)}
}

// replyBuffer is the reply buffer for a request to every node of v.
func (v clusterView) replyBuffer() int {
	return max(minReplyBuffer, 4*v.nodes)
}

// askCluster asks every node for its VARZ, sized from known, and returns how
// many answered and the longest auth timeout among them. It is also what
// proves the system credential is a system-account user: no other account
// can reach the request.
func askCluster(ctx context.Context, sys *nats.Conn, subject string, known clusterView) (clusterView, error) {
	var v clusterView
	err := pingServers(ctx, sys, subject, nil, known.replyBuffer(), func(m *nats.Msg) {
		var resp natsserver.ServerAPIVarzResponse
		if json.Unmarshal(m.Data, &resp) != nil || resp.Error != nil || resp.Data == nil {
			return
		}
		v.nodes++
		v.authTimeout = max(v.authTimeout, time.Duration(resp.Data.AuthTimeout*float64(time.Second)))
	})
	if err != nil {
		return clusterView{}, fmt.Errorf("bus: external broker: ask the cluster for its auth timeout: %w", err)
	}
	if v.nodes == 0 || v.authTimeout <= 0 {
		return clusterView{}, fmt.Errorf(
			"bus: external broker: no server answered %s within %s; the system credential (system_creds_file) "+
				"must be a user in the cluster's system account, listed in the callout's auth_users",
			subject, systemReplyWindow,
		)
	}
	return v, nil
}

// pingServers publishes a request to every node of the cluster and passes
// each reply to handle until [systemReplyWindow] has passed or ctx ends.
// Up to buffer replies may wait for handle, which runs on the calling
// goroutine.
func pingServers(ctx context.Context, sys *nats.Conn, subject string, body []byte, buffer int, handle func(*nats.Msg)) error {
	replies := make(chan *nats.Msg, buffer)
	inbox := sys.NewInbox()
	sub, err := sys.ChanSubscribe(inbox, replies)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe() //nolint:errcheck // the inbox is this call's alone; a failed unsubscribe leaks nothing that matters
	if err := sys.PublishRequest(subject, inbox, body); err != nil {
		return err
	}
	window := time.NewTimer(systemReplyWindow)
	defer window.Stop()
	for {
		select {
		case m := <-replies:
			if !isNoResponders(m) {
				handle(m)
			}
		case <-window.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// isNoResponders reports whether m is the status nats-server sends to a
// request's reply subject when nothing subscribes to the request: no node
// answered, which is not an answer.
func isNoResponders(m *nats.Msg) bool {
	return len(m.Data) == 0 && m.Header.Get("Status") == "503"
}

// provision checks the broker sqi reached, takes the account's guard and
// then creates or updates the streams, returning the broker's identity and
// the guard. The guard comes before the streams so that a server refused as a
// second one on the account has not changed anything the first one owns.
func (r *Remote) provision(ctx context.Context, data *nats.Conn, deadline time.Time) (string, *Guard, error) {
	return r.provisionWith(ctx, data, deadline, GuardOptions{})
}

// provisionWith is provision with the guard taken under opts.
func (r *Remote) provisionWith(ctx context.Context, data *nats.Conn, deadline time.Time, opts GuardOptions) (string, *Guard, error) {
	js, guard, err := r.checkAndGuard(ctx, data, deadline, "", opts)
	if err != nil {
		return "", nil, err
	}
	if err := provisionRemoteStreams(ctx, js, r.cfg.StreamReplicas, deadline, r.logger); err != nil {
		r.releaseGuard(guard)
		return "", nil, err
	}
	identity := remoteIdentity(r.cfg.Account, data.ConnectedClusterName(), data.ConnectedServerName(), data.ConnectedServerId())
	return identity, guard, nil
}

// checkAndGuard checks the broker nc reached (its version, and that the
// account has JetStream) and takes the account's guard for purpose. Both the
// server and a restore pass through it, so neither skips a precondition the
// other enforces.
func (r *Remote) checkAndGuard(ctx context.Context, nc *nats.Conn, deadline time.Time, purpose string, opts GuardOptions) (jetstream.JetStream, *Guard, error) {
	js, err := r.checkBroker(ctx, nc, deadline)
	if err != nil {
		return nil, nil, err
	}
	guard, err := r.acquireGuard(ctx, js, deadline, purpose, opts)
	if err != nil {
		return nil, nil, err
	}
	return js, guard, nil
}

// checkBroker checks the broker nc reached: its version, and that the
// account has JetStream.
func (r *Remote) checkBroker(ctx context.Context, nc *nats.Conn, deadline time.Time) (jetstream.JetStream, error) {
	if err := CheckBrokerVersion(brokerVersion(nc)); err != nil {
		return nil, fmt.Errorf("%w (broker at %s)", err, redact.URL(nc.ConnectedUrl()))
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("bus: jetstream context: %w", err)
	}
	if err := r.requireAccountJetStream(ctx, js, deadline); err != nil {
		return nil, err
	}
	return js, nil
}

// connRole names one of sqi's own connections to the broker.
type connRole struct {
	// name is the client name the broker shows for the connection.
	name string
	// label names the connection in log lines and errors.
	label string
	// checkVersion warns when a reconnect lands on a node below the
	// version floor. One connection checking is enough.
	checkVersion bool
}

var (
	roleData    = connRole{name: "sqi-server data", label: "data", checkVersion: true}
	roleCallout = connRole{name: "sqi-server auth callout", label: "auth callout"}
	roleSystem  = connRole{name: "sqi-server system", label: "system"}
	roleRestore = connRole{name: "sqi-server restore", label: "restore", checkVersion: true}
)

// dial connects one of sqi's connections, retrying across every URL until a
// broker answers or deadline passes. After that, the connection reconnects
// for as long as it lives. state is as for [Remote.open].
func (r *Remote) dial(ctx context.Context, role connRole, state *connState, dialOpts []nats.Option, deadline time.Time) (*nats.Conn, error) {
	rc, err := r.open(role, state, dialOpts, false)
	if err != nil {
		return nil, err
	}
	nc := rc.nc

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for !nc.IsConnected() {
		select {
		case <-rc.connected:
		case <-rc.closed:
			nc.Close()
			return nil, r.dialError(role, "the connection was closed", rc.lastErr.get())
		case <-timer.C:
			nc.Close()
			return nil, r.dialError(role, "no broker answered within "+r.cfg.ConnectTimeout.String(), rc.lastErr.get())
		case <-ctx.Done():
			nc.Close()
			return nil, r.dialError(role, "startup was canceled", ctx.Err())
		}
	}
	return nc, nil
}

// roleConn is a connection [Remote.open] made, with what its handlers report.
type roleConn struct {
	nc *nats.Conn
	// connected is signaled on every connect, closed when the connection
	// closes for good.
	connected, closed chan struct{}
	lastErr           *lastError
}

// open makes one of sqi's connections without waiting for it: nats.Connect
// tries every URL once and, when none answers, returns a connection that
// keeps trying in the background (nats.RetryOnFailedConnect). It fails only
// on options nats.go rejects. With announceLate, a connect that follows a
// failed first attempt is logged as a reconnect is; [Remote.dial] passes
// false, since it waits for that connect itself. The connection's ups and
// downs are recorded in state, [Remote.dataUp] for the data connection that
// [Remote.Connected] reports; nil records nothing.
func (r *Remote) open(role connRole, state *connState, dialOpts []nats.Option, announceLate bool) (roleConn, error) {
	rc := roleConn{connected: make(chan struct{}, 1), closed: make(chan struct{}, 1), lastErr: &lastError{}}
	notify := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	// deferred is set when the first attempt found no broker and
	// announceLate asks for the connect that follows to be logged.
	var deferred atomic.Bool

	opts := append(
		slices.Clone(dialOpts),
		nats.Name(role.name),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(remoteReconnectWait),
		nats.ConnectHandler(func(nc *nats.Conn) {
			state.connected()
			notify(rc.connected)
			if deferred.Load() {
				r.onConnect(nc, role, "established")
			}
		}),
		nats.ClosedHandler(func(*nats.Conn) {
			state.down()
			notify(rc.closed)
		}),
		nats.ReconnectErrHandler(func(_ *nats.Conn, err error) { rc.lastErr.set(err) }),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			state.down()
			rc.lastErr.set(err)
			if nc.IsReconnecting() && err != nil {
				r.logger.WarnContext(context.Background(), "bus: external nats "+role.label+" connection lost; reconnecting",
					slog.Any("error", err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			state.up()
			// A connect that drops and comes back inside the startup
			// window arrives here, not at the ConnectHandler.
			notify(rc.connected)
			r.onConnect(nc, role, "restored")
		}),
	)
	nc, err := nats.Connect(r.cfg.urlList(), opts...)
	if err != nil {
		return roleConn{}, fmt.Errorf("bus: connect the %s connection to external nats (%s): %w", role.label, r.cfg.redactedURLs(), err)
	}
	if nc.IsConnected() {
		state.initialUp()
	} else {
		deferred.Store(announceLate)
	}
	rc.nc = nc
	return rc, nil
}

// onConnect logs a connection restored, or established after startup went
// on without it, and warns when it landed on a node older than the version
// floor: startup checks only the node it first reaches, and a cluster can be
// upgraded or downgraded node by node.
func (r *Remote) onConnect(nc *nats.Conn, role connRole, what string) {
	ctx := context.Background()
	r.logger.InfoContext(ctx, "bus: external nats "+role.label+" connection "+what,
		slog.String("url", redact.URL(nc.ConnectedUrl())))
	if !role.checkVersion {
		return
	}
	if err := CheckBrokerVersion(brokerVersion(nc)); err != nil {
		r.logger.WarnContext(ctx, "bus: reconnected to a nats-server below the supported version",
			slog.String("url", redact.URL(nc.ConnectedUrl())),
			slog.Any("error", err),
			slog.String("remediation", "upgrade every node of the cluster to a supported nats-server release"))
	}
}

func (r *Remote) dialError(role connRole, what string, last error) error {
	prefix := "bus: connect the " + role.label + " connection to external nats"
	if last == nil {
		return fmt.Errorf("%s: %s; tried %s", prefix, what, r.cfg.redactedURLs())
	}
	if errors.Is(last, nats.ErrNkeysNotSupported) {
		// nats-server issues a nonce only when it has nkey users of its own;
		// without one no nkey can sign in, sqi's or any worker's.
		return fmt.Errorf("%s: %s; tried %s; the cluster issued no nonce; define sqi's own users as nkey users, "+
			"as \"sqi-server nats external init\" generates them: %w", prefix, what, r.cfg.redactedURLs(), last)
	}
	return fmt.Errorf("%s: %s; tried %s; last error: %w", prefix, what, r.cfg.redactedURLs(), last)
}

// lastError holds the most recent connection error reported by a nats.go
// callback goroutine.
type lastError struct {
	mu  sync.Mutex
	err error
}

func (l *lastError) set(err error) {
	if err == nil {
		return
	}
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

func (l *lastError) get() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// requireAccountJetStream fails with an actionable message when the account
// has no JetStream. A broker that is still bringing JetStream up answers the
// lookup with other errors, which are retried until deadline.
func (r *Remote) requireAccountJetStream(ctx context.Context, js jetstream.JetStream, deadline time.Time) error {
	err := retryUntil(ctx, deadline, func(ctx context.Context) error {
		_, err := js.AccountInfo(ctx)
		return err
	}, func(err error) bool { return !errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount) })
	switch {
	case err == nil:
		return nil
	case errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount), errors.Is(err, jetstream.ErrJetStreamNotEnabled):
		return r.noJetStreamError(err)
	default:
		return fmt.Errorf("bus: jetstream on account %q: %w", r.cfg.Account, err)
	}
}

// retryUntil runs op until it succeeds, fails with an error transient does
// not accept, or deadline passes, and returns op's last error. ctx ending
// stops it early with ctx's error. op runs on a context that ends at
// deadline, since the caller's ctx may carry none, and no attempt starts
// after deadline, so a failing cluster cannot hold startup past it.
func retryUntil(ctx context.Context, deadline time.Time, op func(context.Context) error, transient func(error) bool) error {
	opCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var last error
	for {
		err := op(opCtx)
		if err != nil && last != nil && opCtx.Err() != nil {
			// The deadline cut this attempt off; the one before says why
			// the retries were needed.
			err = fmt.Errorf("%w (previous attempt: %w)", err, last)
		}
		if err == nil || !transient(err) {
			return err
		}
		last = err
		wait := min(startupRetryWait, time.Until(deadline))
		if wait <= 0 {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w (last error: %w)", ctx.Err(), err)
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			return err
		}
	}
}

// transientJetStreamError reports whether a JetStream request failed because
// the cluster is not ready yet: no meta leader or peers yet, or nobody
// answering. Anything else, such as an invalid stream configuration or an
// account without JetStream, is definite.
func transientJetStreamError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoResponders) || errors.Is(err, jetstream.ErrJetStreamNotEnabled) {
		return true
	}
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode {
	case jsErrClusterNoPeers, jsErrClusterNotAvailable, jsErrClusterNotLeader, jsErrInsufficientResources:
		return true
	}
	return false
}

// nats-server's error codes for a cluster that cannot place or answer a
// request yet (server/jetstream_errors_generated.go).
const (
	jsErrClusterNoPeers        jetstream.ErrorCode = 10005
	jsErrClusterNotAvailable   jetstream.ErrorCode = 10008
	jsErrClusterNotLeader      jetstream.ErrorCode = 10009
	jsErrInsufficientResources jetstream.ErrorCode = 10023
)

func (r *Remote) noJetStreamError(err error) error {
	return fmt.Errorf(
		"bus: account %q on the external broker has no JetStream; add \"jetstream: enabled\" to that account "+
			"in the cluster configuration (the fragment \"sqi-server nats external init\" writes has it): %w",
		r.cfg.Account, err,
	)
}

// provisionRemoteStreams creates or updates the streams and logs, at INFO,
// every replica count it changed. Requests the cluster cannot serve yet are
// retried until deadline; a definite error fails at once.
func provisionRemoteStreams(ctx context.Context, js jetstream.JetStream, replicas int, deadline time.Time, logger *slog.Logger) error {
	var before map[string]int
	err := retryUntil(ctx, deadline, func(ctx context.Context) error {
		var err error
		before, err = streamReplicas(ctx, js, replicas)
		return err
	}, transientJetStreamError)
	if err != nil {
		return fmt.Errorf("bus: provision streams: %w", err)
	}
	warned := false
	err = retryUntil(ctx, deadline, func(ctx context.Context) error {
		err := ensureStreams(ctx, js, replicas)
		if err != nil && !warned && transientJetStreamError(err) {
			warned = true
			logger.WarnContext(ctx, "bus: the external cluster cannot place sqi's streams yet; retrying until the connect timeout",
				slog.Int("replicas", replicas), slog.Any("error", err))
		}
		return err
	}, transientJetStreamError)
	if err != nil {
		return fmt.Errorf("bus: provision streams: %w", err)
	}
	logReplicaChanges(ctx, logger, before, replicas)
	logger.InfoContext(
		ctx, "bus: jetstream streams provisioned",
		slog.String("streams", strings.Join(StreamNames(), ",")),
		slog.Int("replicas", replicas),
	)
	return nil
}

// streamReplicas returns the current replica count of each of sqi's streams
// that already exists.
func streamReplicas(ctx context.Context, js jetstream.JetStream, replicas int) (map[string]int, error) {
	out := map[string]int{}
	for _, def := range streamDefs(replicas) {
		s, err := js.Stream(ctx, def.Name)
		switch {
		case err == nil:
			out[def.Name] = s.CachedInfo().Config.Replicas
		case errors.Is(err, jetstream.ErrStreamNotFound):
		default:
			return nil, fmt.Errorf("stream %s: %w", def.Name, err)
		}
	}
	return out, nil
}

// logReplicaChanges logs one INFO line per previous replica count that
// differs from replicas, naming the streams that had it.
func logReplicaChanges(ctx context.Context, logger *slog.Logger, before map[string]int, replicas int) {
	byOld := map[int][]string{}
	for name, old := range before {
		if old != replicas {
			byOld[old] = append(byOld[old], name)
		}
	}
	olds := make([]int, 0, len(byOld))
	for old := range byOld {
		olds = append(olds, old)
	}
	sort.Ints(olds)
	for _, old := range olds {
		names := byOld[old]
		sort.Strings(names)
		attrs := []any{
			slog.String("streams", strings.Join(names, ",")),
			slog.String("replicas", strconv.Itoa(old)+" → "+strconv.Itoa(replicas)),
		}
		if replicas < old {
			attrs = append(attrs, slog.String("impact", "fewer copies of each message survive a node loss"))
		}
		logger.InfoContext(ctx, "bus: stream replica count changed", attrs...)
	}
}

// Shutdown closes sqi's connections to the broker. The broker itself is the
// operator's and keeps running. It is safe to call more than once and
// concurrently with the other methods.
//
// With auth on, the callout subscription stops taking requests first and the
// authorizations already accepted are answered before the callout
// connection closes, for at most the responder's own per-request budget. The
// account's guard is released before the data connection closes.
func (r *Remote) Shutdown() {
	r.mu.Lock()
	cancelCheck, checkDone := r.checkCancel, r.checkDone
	r.mu.Unlock()
	if cancelCheck != nil {
		cancelCheck()
		<-checkDone
	}

	r.mu.Lock()
	data, callout, system := r.data, r.callout, r.system
	responder, guard := r.responder, r.guard
	r.data, r.callout, r.system = nil, nil, nil
	r.guard = nil
	r.responder = nil
	r.mu.Unlock()

	if responder != nil {
		responder.Stop()
	}
	r.releaseGuard(guard)
	closeConns(system, callout, data)
	if data != nil {
		r.logger.InfoContext(context.Background(), "bus: external nats connection closed")
	}
}

// Check implements [health.Checker]. It returns nil while the data
// connection is connected and, with auth on, so is the callout connection:
// without it every worker connect is refused. The system connection is not
// checked: without it revocation waits for the user JWT's expiry, and Revoke
// reports that, but workers keep working. Check also returns the guard's
// [Guard.Healthy] error, so a server that lost its account reports degraded.
// A standby is not ready until its broker checks have passed
// ([Remote.StandbyReady]).
// It makes no round trip, so it answers within the health budget during a
// partition.
func (r *Remote) Check(_ context.Context) error {
	r.mu.Lock()
	data, callout, guard := r.data, r.callout, r.guard
	standby, standbyErr := r.standby, r.standbyErr
	r.mu.Unlock()
	if err := requireConnected("nats data connection", data); err != nil {
		return err
	}
	if standby && standbyErr != nil {
		return standbyErr
	}
	if r.cfg.Auth.Enabled {
		if err := requireConnected("nats auth callout connection", callout); err != nil {
			return err
		}
	}
	if guard != nil {
		return guard.Healthy()
	}
	return nil
}

// NewClient returns a connected [Client] on the data connection's credential
// and TLS settings, reconnecting without limit. It must be called after
// [Remote.Start] returns successfully.
func (r *Remote) NewClient() (*Client, error) {
	r.mu.Lock()
	data, dialOpts := r.data, slices.Clone(r.dialOpts)
	r.mu.Unlock()
	if data == nil {
		return nil, errors.New("bus: broker not started")
	}
	return NewClient(r.cfg.urlList(), r.logger, append(dialOpts, nats.Name("sqi-server client"))...)
}

// revokeBudgetSlack is added to the time Revoke needs in RevokeBudget, for a
// slow host or a slow cluster.
const revokeBudgetSlack = 2 * time.Second

// RevokeBudget is how long [Remote.Revoke] may need: the cluster's auth
// timeout, the settle margin, the final pass's reply window and its kicks,
// and some slack. Zero when Revoke does not wait (auth off, or no system
// credential).
func (r *Remote) RevokeBudget() time.Duration {
	if !r.cfg.Auth.Enabled || r.cfg.Auth.SystemCredsFile == "" {
		return 0
	}
	r.mu.Lock()
	authTimeout := r.cluster.authTimeout
	r.mu.Unlock()
	return authTimeout + revokeSettleMargin + systemReplyWindow + kickTimeout + revokeBudgetSlack
}

// Revoke disconnects every connection authorized as workerID, on every node
// of the cluster. The callout names each worker connection after its worker
// ID, so the connections are found by that user name in sqi's account. With
// worker authentication off there is nothing to revoke: any client may
// connect, so it returns nil.
//
// Call it after the credential is revoked in the store. Without a system
// credential sqi cannot reach another client's connection, so Revoke only
// logs: the worker stays connected until its user JWT expires, at most
// UserTTL, and its reconnect is then refused.
//
// With a system credential, when Revoke returns nil every node of the
// cluster has been asked and none holds a connection for workerID whose
// authorization began before Revoke did. As in [Embedded.Revoke] there are
// two passes, each a CONNZ request to every node followed by a KICK for each
// connection found:
//
//   - the first, at once, kicks every connection present;
//   - the final one runs once the cluster's auth timeout (the longest any
//     node has reported, at Start or alongside an earlier pass, plus
//     [revokeSettleMargin]) has elapsed since Revoke began. A node abandons
//     a callout after that timeout, so every authorization that was already
//     in flight, perhaps on a store read from before the revocation, has by
//     then either placed its connection, which this pass kicks, or been
//     refused.
//
// A pass fails, and so does Revoke, when fewer nodes answer than sqi knows
// the cluster has, or when a kick is not confirmed: the worker may then
// still be connected. A cluster that has shrunk for good keeps failing
// revocations until sqi-server restarts and counts it afresh.
//
// Each pass collects replies for [systemReplyWindow], so Revoke takes about
// the auth timeout plus that window (3.25s at nats-server's defaults, 4.25s
// with TLS); [Remote.RevokeBudget] says how long to allow. If ctx ends first
// it returns ctx's error, after the first pass at the most. Any reconnect is
// authorized afresh against the store and refused.
func (r *Remote) Revoke(ctx context.Context, workerID string) error {
	began := time.Now()
	if !r.cfg.Auth.Enabled {
		return nil
	}
	// An empty user filter would match every connection in the account,
	// sqi's own data connection included.
	if workerID == "" {
		return errors.New("bus: revoke: empty worker id")
	}
	if r.cfg.Auth.SystemCredsFile == "" {
		r.logger.InfoContext(ctx, "bus: worker credential revoked; with no system credential its live connection lasts until its user JWT expires",
			slog.String("worker_id", workerID),
			slog.Duration("within", r.cfg.Auth.UserTTL))
		return nil
	}
	r.mu.Lock()
	system, known, connzSubject, varzSubject := r.system, r.cluster, r.connzSubject, r.varzSubject
	r.mu.Unlock()
	if system == nil {
		return errors.New("bus: broker not started")
	}

	// A node may have joined, or had its auth timeout raised, since sqi last
	// asked. The answer arrives with the first pass's, well before the wait
	// ends. If it fails, what sqi already knew stands.
	fresh := make(chan clusterView, 1)
	go func() {
		v, err := askCluster(ctx, system, varzSubject, known)
		if err != nil {
			v = clusterView{}
		}
		fresh <- v
	}()
	answered, err := r.kickPass(ctx, system, connzSubject, workerID, known)
	known = known.merge(<-fresh)
	r.mu.Lock()
	r.cluster = r.cluster.merge(known)
	r.mu.Unlock()
	if err == nil {
		err = requireAllAnswered(workerID, answered, known)
	}
	if err != nil {
		return err
	}

	if err := awaitRevokeSettle(ctx, began, known.authTimeout, workerID); err != nil {
		return err
	}
	if answered, err = r.kickPass(ctx, system, connzSubject, workerID, known); err != nil {
		return err
	}
	return requireAllAnswered(workerID, answered, known)
}

// requireAllAnswered fails a pass that fewer nodes answered than the
// cluster is known to have: the nodes that did not answer may hold a
// connection for workerID.
func requireAllAnswered(workerID string, answered int, known clusterView) error {
	if answered >= known.nodes {
		return nil
	}
	return fmt.Errorf("bus: revoke %s: only %d of the cluster's %d nodes listed their connections within %s; "+
		"the worker may stay connected until its user JWT expires", workerID, answered, known.nodes, systemReplyWindow)
}

// kickPass asks every node for workerID's connections in sqi's account and
// kicks each one as its node's answer arrives, returning how many nodes
// answered. It fails when none did, since then nothing was checked.
func (r *Remote) kickPass(ctx context.Context, sys *nats.Conn, subject, workerID string, known clusterView) (int, error) {
	if st := sys.Status(); st != nats.CONNECTED {
		return 0, fmt.Errorf("bus: revoke %s: the system connection is %s; the worker stays connected until its user JWT expires",
			workerID, st)
	}
	req, err := json.Marshal(natsserver.ConnzEventOptions{ConnzOptions: natsserver.ConnzOptions{
		// Username fills in each connection's authorized_user, which the
		// check below reads; the user and account filters apply either way.
		Username: true,
		User:     workerID,
		Account:  r.cfg.Account,
		Limit:    math.MaxInt32,
	}})
	if err != nil {
		return 0, fmt.Errorf("bus: revoke %s: %w", workerID, err)
	}

	var kicks sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	answered := 0
	pingErr := pingServers(ctx, sys, subject, req, known.replyBuffer(), func(m *nats.Msg) {
		var resp natsserver.ServerAPIConnzResponse
		if err := json.Unmarshal(m.Data, &resp); err != nil {
			fail(fmt.Errorf("read a connection list: %w", err))
			return
		}
		if resp.Error != nil {
			fail(fmt.Errorf("a node could not list its connections: %s", resp.Error.Description))
			return
		}
		if resp.Server == nil || resp.Data == nil {
			fail(errors.New("a node answered with no connection list"))
			return
		}
		answered++
		for _, ci := range resp.Data.Conns {
			// The node was asked for workerID's connections only. Anything
			// else means it did not apply the filter, and kicking it could
			// disconnect anyone, sqi's own connections included.
			if ci.AuthorizedUser != workerID {
				fail(fmt.Errorf("node %s listed connection %d of user %q when asked for %q; that connection was not kicked",
					resp.Server.Name, ci.Cid, ci.AuthorizedUser, workerID))
				continue
			}
			kicks.Go(func() {
				if err := kick(ctx, sys, resp.Server.ID, ci.Cid); err != nil {
					fail(fmt.Errorf("kick connection %d on %s: %w", ci.Cid, resp.Server.Name, err))
				}
			})
		}
	})
	kicks.Wait()
	switch {
	case pingErr != nil:
		return answered, fmt.Errorf("bus: revoke %s: %w", workerID, pingErr)
	case len(errs) > 0:
		return answered, fmt.Errorf("bus: revoke %s: %w", workerID, errors.Join(errs...))
	case answered == 0:
		return 0, fmt.Errorf("bus: revoke %s: no node answered %s within %s; the worker stays connected until its user JWT expires",
			workerID, subject, systemReplyWindow)
	}
	return answered, nil
}

// kick disconnects connection cid on the node serverID. A connection that is
// already gone is the outcome wanted. A node that does not answer is not:
// that is also what a route to it being down looks like, with the
// connection still in place.
func kick(ctx context.Context, sys *nats.Conn, serverID string, cid uint64) error {
	req, err := json.Marshal(natsserver.KickClientReq{CID: cid})
	if err != nil {
		return err
	}
	// Detached from ctx: a kick under way finishes even if the caller has
	// gone, bounded by its own timeout.
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kickTimeout)
	defer cancel()
	m, err := sys.RequestWithContext(kctx, fmt.Sprintf(kickSubjectFmt, serverID), req)
	if err != nil {
		return err
	}
	var resp struct {
		Error *natsserver.ApiError `json:"error"`
	}
	if err := json.Unmarshal(m.Data, &resp); err != nil {
		return fmt.Errorf("read the kick reply: %w", err)
	}
	if resp.Error != nil && !strings.Contains(resp.Error.Description, "no such client") {
		return errors.New(resp.Error.Description)
	}
	return nil
}
