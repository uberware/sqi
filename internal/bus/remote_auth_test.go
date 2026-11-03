// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// testCreds is a [bus.CredentialLookup] over an in-memory map, standing in for
// the store's active credentials. It counts the lookups it answers.
type testCreds struct {
	mu      sync.Mutex
	byKey   map[string]string
	err     error
	lookups atomic.Int64
}

func newTestCreds() *testCreds {
	return &testCreds{byKey: map[string]string{}}
}

func (c *testCreds) enroll(t *testing.T, workerID string) (seed []byte, pub string) {
	t.Helper()
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.byKey[pub] = workerID
	c.mu.Unlock()
	return seed, pub
}

func (c *testCreds) remove(pub string) {
	c.mu.Lock()
	delete(c.byKey, pub)
	c.mu.Unlock()
}

func (c *testCreds) fail(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

func (c *testCreds) lookup(_ context.Context, pub string) (string, error) {
	c.lookups.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	id, ok := c.byKey[pub]
	if !ok {
		return "", bus.ErrUnknownCredential
	}
	return id, nil
}

// authRemoteConfig points a remote broker with worker authentication at c,
// an auth-enabled cluster, answering from lookup. It has no system
// credential.
func authRemoteConfig(c *bustest.Cluster, lookup bus.CredentialLookup) bus.RemoteConfig {
	cfg := remoteConfig(c, bus.DefaultAccount, 1)
	cfg.Auth = bus.RemoteAuthConfig{
		AuthConfig:          bus.AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: lookup},
		CalloutNkeySeedFile: c.CalloutSeed,
		IssuerSeedFile:      c.IssuerSeed,
	}
	return cfg
}

// connectWorker connects as a worker through urls, in order, and closes the
// connection when the test ends.
func connectWorker(t *testing.T, urls []string, seed []byte, pub string, opts ...nats.Option) *nats.Conn {
	t.Helper()
	opts = append([]nats.Option{brokerauth.NkeyOption(pub, seed), nats.DontRandomize()}, opts...)
	nc, err := nats.Connect(strings.Join(urls, ","), opts...)
	if err != nil {
		t.Fatalf("connect as %s: %v", pub, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// refusedEverywhere asserts that a fresh connection as the worker key pub is
// refused through every node of c.
func refusedEverywhere(t *testing.T, c *bustest.Cluster, seed []byte, pub string) {
	t.Helper()
	for i, u := range c.URLs {
		nc, err := nats.Connect(u, nats.NoReconnect(), brokerauth.NkeyOption(pub, seed))
		if err == nil {
			nc.Close()
			t.Fatalf("node %d accepted the connection", i)
		}
		if !errors.Is(err, nats.ErrAuthorization) {
			t.Errorf("node %d: error = %v, want nats.ErrAuthorization", i, err)
		}
	}
}

// staysDown asserts that nc does not return to CONNECTED within d, and does
// not give up and close either.
func staysDown(t *testing.T, nc *nats.Conn, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		switch st := nc.Status(); st {
		case nats.CONNECTED:
			t.Fatalf("the connection was authorized again through %s", nc.ConnectedUrlRedacted())
		case nats.CLOSED:
			t.Fatalf("the connection closed: %v", nc.LastError())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func awaitStatus(t *testing.T, nc *nats.Conn, want nats.Status, within time.Duration) {
	t.Helper()
	eventually(t, within, "connection "+want.String(), func() error {
		if st := nc.Status(); st != want {
			return fmt.Errorf("status %s, last error %w", st, nc.LastError())
		}
		return nil
	})
}

// TestRemote_EnrolledWorkerGetsExactlyItsGrants runs the forgery checks the
// embedded broker runs against a three-node cluster, connecting through each
// node in turn: a worker on a node other than the responder's is authorized
// over the cluster's routes.
func TestRemote_EnrolledWorkerGetsExactlyItsGrants(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	logger, _ := bufLogger()
	r := startRemote(t, authRemoteConfig(c, creds.lookup), logger)
	if err := r.Check(t.Context()); err != nil {
		t.Fatalf("Check with auth on = %v, want nil", err)
	}

	for i, u := range c.URLs {
		t.Run(fmt.Sprintf("node %d", i), func(t *testing.T) {
			nc := connectWorker(t, []string{u}, seed, pub)
			bus.AssertWorkerGrants(t, nc, "worker-a")
		})
	}
}

func TestRemote_UnknownRevokedAndEmptyNonceRefused(t *testing.T) {
	t.Parallel()
	t.Run("nkey cluster", func(t *testing.T) {
		t.Parallel()
		c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
		creds := newTestCreds()
		seed, pub := creds.enroll(t, "worker-a")
		logger, _ := bufLogger()
		startRemote(t, authRemoteConfig(c, creds.lookup), logger)

		connect := func(opts ...nats.Option) error {
			nc, err := nats.Connect(strings.Join(c.URLs, ","), append([]nats.Option{nats.NoReconnect()}, opts...)...)
			if err == nil {
				nc.Close()
			}
			return err
		}
		if err := connect(brokerauth.NkeyOption(pub, seed)); err != nil {
			t.Fatalf("the enrolled worker was refused: %v", err)
		}
		if err := connect(); err == nil {
			t.Error("an anonymous connection was accepted")
		}
		strangerSeed, strangerPub, err := brokerauth.GenerateSeed()
		if err != nil {
			t.Fatal(err)
		}
		if err := connect(brokerauth.NkeyOption(strangerPub, strangerSeed)); !errors.Is(err, nats.ErrAuthorization) {
			t.Errorf("a key never enrolled: %v, want nats.ErrAuthorization", err)
		}
		if err := connect(brokerauth.NkeyOption(pub, strangerSeed)); !errors.Is(err, nats.ErrAuthorization) {
			t.Errorf("the enrolled key signed by another seed: %v, want nats.ErrAuthorization", err)
		}

		creds.fail(errors.New("database is down"))
		if err := connect(brokerauth.NkeyOption(pub, seed)); !errors.Is(err, nats.ErrAuthorization) {
			t.Errorf("connect during a store error: %v, want nats.ErrAuthorization", err)
		}
		creds.fail(nil)

		creds.remove(pub)
		refusedEverywhere(t, c, seed, pub)
	})

	t.Run("password cluster issues no nonce", func(t *testing.T) {
		t.Parallel()
		c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true, PasswordUsers: true})
		creds := newTestCreds()
		seed, pub := creds.enroll(t, "worker-a")
		testNoNonceCluster(t, c, creds, seed, pub)
	})
}

// testNoNonceCluster checks a cluster whose own users are all passwords, so
// that it issues no nonce. sqi refuses to start on it, naming the cause and
// the fix, and a responder that does answer refuses every worker, logging the
// ERROR that names the cluster's misconfiguration.
func testNoNonceCluster(t *testing.T, c *bustest.Cluster, creds *testCreds, seed []byte, pub string) {
	t.Helper()
	logger, logs := bufLogger()

	// sqi-server: its data connection authenticates by password in the URL,
	// its callout connection by nkey, which the cluster cannot verify.
	calloutSeed := writeSeed(t, "callout.nk")
	cfg := authRemoteConfig(c, creds.lookup)
	cfg.URLs = withUserInfo(t, c.URLs, c.DataUser, c.DataPassword)
	cfg.NkeySeedFile = ""
	cfg.Auth.CalloutNkeySeedFile = calloutSeed
	cfg.ConnectTimeout = 2 * time.Second
	r := bus.NewRemote(cfg, logger)
	t.Cleanup(r.Shutdown)
	err := r.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded on a cluster that issues no nonce")
	}
	if !strings.Contains(err.Error(), "define sqi's own users as nkey users") {
		t.Errorf("Start error %q does not name the fix", err)
	}

	// A responder the cluster can reach, through a callout connection that
	// authenticates by password.
	serveResponder(t, c, creds.lookup, logger)

	for i, u := range c.URLs {
		// nats.go refuses on the client side before any callout runs.
		if nc, err := nats.Connect(u, nats.NoReconnect(), brokerauth.NkeyOption(pub, seed)); err == nil {
			nc.Close()
			t.Fatalf("node %d accepted an enrolled worker with no nonce issued", i)
		}
		// A client that signs anyway reaches the responder with no nonce.
		if line := rawSignedConnect(t, u, seed, pub); !strings.Contains(line, "Authorization Violation") {
			t.Fatalf("node %d answered a signed connect with no nonce with %q, want an authorization violation", i, line)
		}
	}
	eventually(t, 5*time.Second, "the no-nonce ERROR", func() error {
		for line := range strings.SplitSeq(logs.String(), "\n") {
			if strings.Contains(line, "level=ERROR") && strings.Contains(line, "the cluster issued no nonce; define sqi's own users as nkey users") {
				return nil
			}
		}
		return errors.New("not logged")
	})
	if n := creds.lookups.Load(); n != 0 {
		t.Errorf("the credential store was consulted %d times for connects with no nonce", n)
	}
}

func writeSeed(t *testing.T, name string) string {
	t.Helper()
	seed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withUserInfo(t *testing.T, urls []string, user, password string) []string {
	t.Helper()
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = strings.Replace(u, "nats://", "nats://"+user+":"+password+"@", 1)
	}
	return out
}

// serveResponder answers c's callouts from lookup through a callout
// connection that authenticates with the password user of a
// [bustest.Options.PasswordUsers] cluster.
func serveResponder(t *testing.T, c *bustest.Cluster, lookup bus.CredentialLookup, logger *slog.Logger) {
	t.Helper()
	seed, err := os.ReadFile(c.IssuerSeed)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := nkeys.FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := bus.NewResponder(bus.ResponderConfig{
		Issuer: issuer, Account: bus.DefaultAccount, UserTTL: time.Hour, Lookup: lookup, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(strings.Join(c.URLs, ","), nats.UserInfo(c.CalloutUser, c.CalloutPassword))
	if err != nil {
		t.Fatalf("callout connection: %v", err)
	}
	t.Cleanup(nc.Close)
	if err := resp.Serve(context.Background(), nc); err != nil {
		t.Fatal(err)
	}
}

// rawSignedConnect speaks the client protocol by hand: it connects to rawURL
// presenting the worker key pub and a signature over the empty nonce, which
// nats.go refuses to send, and returns the server's first reply line.
func rawSignedConnect(t *testing.T, rawURL string, seed []byte, pub string) string {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", strings.TrimPrefix(rawURL, "nats://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(conn)
	info, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read INFO: %v", err)
	}
	if strings.Contains(info, `"nonce"`) {
		t.Fatalf("the cluster issued a nonce: %s", info)
	}
	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := kp.Sign(nil)
	if err != nil {
		t.Fatal(err)
	}
	connect, err := json.Marshal(map[string]any{
		"verbose": false, "pedantic": false, "protocol": 1, "lang": "go", "version": "test",
		"nkey": pub, "sig": base64.RawURLEncoding.EncodeToString(sig),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s\r\nPING\r\n", connect); err != nil {
		t.Fatal(err)
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read the reply to CONNECT: %v", err)
	}
	return strings.TrimSpace(line)
}

// TestRemote_RevokeWithSystemCredsKicksWithinOneSecond revokes a worker
// connected through node 1. With the system credential, the broker finds the
// connection with CONNZ and kicks it; the reconnect, through any node, goes
// back through the callout and is refused.
func TestRemote_RevokeWithSystemCredsKicksWithinOneSecond(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seedA, pubA := creds.enroll(t, "worker-a")
	seedB, pubB := creds.enroll(t, "worker-b")
	cfg := authRemoteConfig(c, creds.lookup)
	cfg.Auth.SystemCredsFile = c.SystemCreds
	// sqi's connections, the system one included, on node 0 and the worker
	// on node 1: the kick has to cross the cluster.
	cfg.URLs = c.URLs[:1]
	logger, _ := bufLogger()
	r := startRemote(t, cfg, logger)

	disconnected := make(chan time.Time, 1)
	var reconnects atomic.Int32
	ncA := connectWorker(t, []string{c.URLs[1], c.URLs[0], c.URLs[2]}, seedA, pubA,
		nats.IgnoreAuthErrorAbort(), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond),
		nats.DisconnectErrHandler(func(*nats.Conn, error) {
			select {
			case disconnected <- time.Now():
			default:
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { reconnects.Add(1) }))
	if got := ncA.ConnectedUrl(); got != c.URLs[1] {
		t.Fatalf("worker-a connected through %s, want node 1 (%s)", got, c.URLs[1])
	}
	ncB := connectWorker(t, []string{c.URLs[2]}, seedB, pubB)

	creds.remove(pubA)
	began := time.Now()
	revoked := make(chan error, 1)
	go func() { revoked <- r.Revoke(context.Background(), "worker-a") }()

	select {
	case at := <-disconnected:
		t.Logf("worker-a disconnected %s after Revoke began", at.Sub(began).Round(time.Millisecond))
		if at.Sub(began) > time.Second {
			t.Errorf("worker-a disconnected %s after Revoke began, want within 1s", at.Sub(began))
		}
	case <-time.After(time.Second):
		t.Fatalf("worker-a still %s 1s after Revoke began", ncA.Status())
	}
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		t.Logf("Revoke returned after %s", time.Since(began).Round(time.Millisecond))
	case <-time.After(10 * time.Second):
		t.Fatal("Revoke did not return within 10s")
	}

	staysDown(t, ncA, 2*time.Second)
	if n := reconnects.Load(); n != 0 {
		t.Errorf("worker-a reconnected %d times after revocation", n)
	}
	refusedEverywhere(t, c, seedA, pubA)

	if st := ncB.Status(); st != nats.CONNECTED {
		t.Fatalf("worker-b is %s after revoking worker-a", st)
	}
	if err := ncB.Flush(); err != nil {
		t.Fatalf("worker-b unusable after revoking worker-a: %v", err)
	}
}

// TestRemote_RevokeWithoutSystemCredsBoundedByTTL revokes a worker on a
// broker with no system credential. Revoke cannot reach the connection, so
// the worker stays connected until its user JWT expires, and its reconnect is
// then refused.
func TestRemote_RevokeWithoutSystemCredsBoundedByTTL(t *testing.T) {
	t.Parallel()
	const ttl = 3 * time.Second
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	cfg := authRemoteConfig(c, creds.lookup)
	cfg.Auth.UserTTL = ttl
	logger, _ := bufLogger()
	r := startRemote(t, cfg, logger)

	disconnected := make(chan time.Time, 1)
	issued := time.Now()
	nc := connectWorker(t, c.URLs, seed, pub,
		nats.IgnoreAuthErrorAbort(), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond),
		nats.DisconnectErrHandler(func(*nats.Conn, error) {
			select {
			case disconnected <- time.Now():
			default:
			}
		}))

	creds.remove(pub)
	if err := r.Revoke(context.Background(), "worker-a"); err != nil {
		t.Fatalf("Revoke without a system credential = %v, want nil", err)
	}
	// exp is whole seconds, so the JWT lives at least ttl-1s.
	time.Sleep(time.Until(issued.Add(ttl - 1500*time.Millisecond)))
	if err := nc.Flush(); err != nil || nc.Status() != nats.CONNECTED {
		t.Fatalf("the revoked worker was disconnected before its JWT expired: %s, %v", nc.Status(), err)
	}

	select {
	case at := <-disconnected:
		t.Logf("disconnected %s after the JWT was issued", at.Sub(issued).Round(time.Millisecond))
		if at.Before(issued.Add(ttl - time.Second)) {
			t.Errorf("disconnected %s after issue, before the JWT expired", at.Sub(issued))
		}
	case <-time.After(time.Until(issued.Add(ttl + 2*time.Second))):
		t.Fatalf("still %s %s after issue; the user JWT expiry did not end the connection", nc.Status(), ttl+2*time.Second)
	}
	staysDown(t, nc, 2*time.Second)
	refusedEverywhere(t, c, seed, pub)
}

// TestRemote_ExpiryReconnectsThreeTimes checks that a user JWT's expiry
// costs a reconnect and nothing more on a cluster: the worker goes back
// through the callout and keeps working, expiry after expiry.
func TestRemote_ExpiryReconnectsThreeTimes(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	cfg := authRemoteConfig(c, creds.lookup)
	cfg.Auth.UserTTL = 2 * time.Second
	logger, _ := bufLogger()
	startRemote(t, cfg, logger)

	var reconnects, expiries atomic.Int32
	nc := connectWorker(t, c.URLs, seed, pub,
		nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { reconnects.Add(1) }),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if errors.Is(err, nats.ErrAuthExpired) {
				expiries.Add(1)
			}
		}))

	eventually(t, 20*time.Second, "three reconnects", func() error {
		if st := nc.Status(); st == nats.CLOSED {
			t.Fatalf("connection CLOSED after %d reconnects: %v", reconnects.Load(), nc.LastError())
		}
		if n := reconnects.Load(); n < 3 {
			return fmt.Errorf("%d reconnects", n)
		}
		return nil
	})
	if expiries.Load() == 0 {
		t.Error("no disconnect was an authentication expiry; the reconnects had some other cause")
	}
	awaitStatus(t, nc, nats.CONNECTED, 5*time.Second)
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush after three expiries: %v", err)
	}
}

// TestRemote_CalloutQueueGroup runs two sqi-servers' responders on one
// cluster. They share the callout queue group, so each answers some of the
// connects and every connect is answered exactly once: what a standby
// sqi-server needs.
func TestRemote_CalloutQueueGroup(t *testing.T) {
	t.Parallel()
	const connects = 20
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	var answeredA, answeredB atomic.Int64
	counting := func(n *atomic.Int64) bus.CredentialLookup {
		return func(ctx context.Context, key string) (string, error) {
			n.Add(1)
			return creds.lookup(ctx, key)
		}
	}
	logger, _ := bufLogger()
	startRemote(t, authRemoteConfig(c, counting(&answeredA)), logger)
	// A second server on the account is what the guard refuses; this test
	// is about the responders alone.
	second := bus.NewRemote(authRemoteConfig(c, counting(&answeredB)), logger)
	bus.DisableRemoteGuard(second)
	if err := second.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(second.Shutdown)

	for i := range connects {
		nc, err := nats.Connect(c.URLs[i%len(c.URLs)], nats.NoReconnect(), brokerauth.NkeyOption(pub, seed))
		if err != nil {
			t.Fatalf("connect %d through node %d: %v", i, i%len(c.URLs), err)
		}
		nc.Close()
	}
	a, b := answeredA.Load(), answeredB.Load()
	t.Logf("responder A answered %d, responder B answered %d", a, b)
	if a+b != connects {
		t.Errorf("the responders answered %d connects between them, want %d", a+b, connects)
	}
	if a == 0 || b == 0 {
		t.Errorf("one responder answered every connect (A %d, B %d); they do not share a queue group", a, b)
	}
}

// TestRemote_ReconnectStormAllWorkersReturn restarts every node of a cluster
// under 200 connected workers whose credentials live in a SQLite store. Every
// worker reconnects at once through the callout and the store's read pool;
// all must be back within 10s, and none may be closed for good by nats.go's
// repeated-authorization-error rule. Not parallel, so that its timing is its
// own.
func TestRemote_ReconnectStormAllWorkersReturn(t *testing.T) {
	const (
		workers = 200
		budget  = 10 * time.Second
	)
	ctx := t.Context()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	st, err := sqlite.Open(ctx, t.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	type worker struct {
		seed []byte
		pub  string
	}
	ws := make([]worker, workers)
	for i := range ws {
		seed, pub, err := brokerauth.GenerateSeed()
		if err != nil {
			t.Fatal(err)
		}
		ws[i] = worker{seed, pub}
		if _, err := st.CreateWorkerCredential(ctx, store.WorkerCredential{
			ID: fmt.Sprintf("cred-%03d", i), WorkerID: fmt.Sprintf("worker-%03d", i), PublicKey: pub,
			Name: fmt.Sprintf("worker %d", i), EnrolledAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	lookups := &lookupStats{}
	lookup := lookups.wrap(func(ctx context.Context, key string) (string, error) {
		cred, err := st.GetActiveWorkerCredentialByPublicKey(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			return "", bus.ErrUnknownCredential
		}
		if err != nil {
			return "", err
		}
		return cred.WorkerID, nil
	})
	logger, _ := bufLogger()
	startRemote(t, authRemoteConfig(c, lookup), logger)

	var closed, refusals atomic.Int32
	conns := make([]*nats.Conn, workers)
	reconnectedAt := make([]atomic.Int64, workers)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	connected := time.Now()
	for i, w := range ws {
		wg.Go(func() {
			// The options a worker uses that matter here: it retries a
			// refusal rather than closing, and reconnects without limit.
			nc, err := nats.Connect(strings.Join(c.URLs, ","), brokerauth.NkeyOption(w.pub, w.seed),
				nats.IgnoreAuthErrorAbort(), nats.MaxReconnects(-1),
				nats.ClosedHandler(func(*nats.Conn) { closed.Add(1) }),
				nats.ReconnectHandler(func(*nats.Conn) { reconnectedAt[i].Store(time.Now().UnixNano()) }),
				nats.ReconnectErrHandler(func(_ *nats.Conn, err error) {
					if errors.Is(err, nats.ErrAuthorization) {
						refusals.Add(1)
					}
				}))
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
				return
			}
			conns[i] = nc
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("initial connect: %v", err)
	}
	t.Cleanup(func() {
		for _, nc := range conns {
			nc.Close()
		}
	})
	t.Logf("%d workers connected in %s; %s", workers, time.Since(connected).Round(time.Millisecond), lookups.reset())

	stopping := time.Now()
	c.StopAll()
	t.Logf("cluster stopped in %s", time.Since(stopping).Round(time.Millisecond))
	eventually(t, 10*time.Second, "every worker notices the outage", func() error {
		for i, nc := range conns {
			if nc.IsConnected() {
				return fmt.Errorf("worker %d still connected", i)
			}
		}
		return nil
	})
	// The storm starts as soon as the first node accepts connections; the
	// budget runs from then. Every worker may first land on that one node,
	// before sqi's callout connection is back.
	c.RestartNode(0)
	firstUp := time.Now()
	for i := 1; i < len(c.URLs); i++ {
		c.RestartNode(i)
	}
	t.Logf("cluster fully back %s after the first node", time.Since(firstUp).Round(time.Millisecond))

	deadline := firstUp.Add(budget)
	for {
		if n := closed.Load(); n != 0 {
			t.Fatalf("%d workers closed for good", n)
		}
		down := 0
		for i := range conns {
			if reconnectedAt[i].Load() == 0 || !conns[i].IsConnected() {
				down++
			}
		}
		if down == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d workers not reconnected %s after the first node was back; %d refusals; %s",
				down, workers, budget, refusals.Load(), lookups.reset())
		}
		time.Sleep(20 * time.Millisecond)
	}
	var latest int64
	for i := range reconnectedAt {
		latest = max(latest, reconnectedAt[i].Load())
	}
	took := time.Unix(0, latest).Sub(firstUp)
	t.Logf("all %d workers reconnected within %s of the first node being back; %d refusals on the way; %s",
		workers, took.Round(time.Millisecond), refusals.Load(), lookups.reset())
	if took > budget {
		t.Errorf("the last worker reconnected %s after the first node was back, want within %s", took, budget)
	}
	if n := closed.Load(); n != 0 {
		t.Fatalf("%d workers closed for good", n)
	}
}

// lookupStats wraps a lookup, recording how many calls it served, their mean
// duration and the slowest.
type lookupStats struct {
	mu    sync.Mutex
	calls int
	total time.Duration
	max   time.Duration
}

func (s *lookupStats) wrap(next bus.CredentialLookup) bus.CredentialLookup {
	return func(ctx context.Context, key string) (string, error) {
		began := time.Now()
		id, err := next(ctx, key)
		d := time.Since(began)
		s.mu.Lock()
		s.calls++
		s.total += d
		s.max = max(s.max, d)
		s.mu.Unlock()
		return id, err
	}
}

// reset reports the calls since the last reset and starts counting afresh.
func (s *lookupStats) reset() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mean time.Duration
	if s.calls > 0 {
		mean = s.total / time.Duration(s.calls)
	}
	out := fmt.Sprintf("%d store lookups, mean %s, max %s", s.calls, mean, s.max)
	s.calls, s.total, s.max = 0, 0, 0
	return out
}

// TestRemote_AuthOnRefusesIncompleteConfig checks that a remote broker with
// auth on and a key missing fails at Start, before it dials anything.
func TestRemote_AuthOnRefusesIncompleteConfig(t *testing.T) {
	t.Parallel()
	base := bus.RemoteConfig{
		URLs: closedPorts(t, 1), Account: bus.DefaultAccount, StreamReplicas: 1, ConnectTimeout: time.Minute,
		Auth: bus.RemoteAuthConfig{
			AuthConfig:          bus.AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: newTestCreds().lookup},
			CalloutNkeySeedFile: writeSeed(t, "callout.nk"),
			IssuerSeedFile:      writeAccountSeed(t),
		},
	}
	tests := []struct {
		name   string
		mutate func(*bus.RemoteConfig)
	}{
		{"no callout seed", func(c *bus.RemoteConfig) { c.Auth.CalloutNkeySeedFile = "" }},
		{"no issuer seed", func(c *bus.RemoteConfig) { c.Auth.IssuerSeedFile = "" }},
		{"issuer is a user seed", func(c *bus.RemoteConfig) { c.Auth.IssuerSeedFile = writeSeed(t, "user.nk") }},
		{"no lookup", func(c *bus.RemoteConfig) { c.Auth.Lookup = nil }},
		{"no user ttl", func(c *bus.RemoteConfig) { c.Auth.UserTTL = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			logger, _ := bufLogger()
			r := bus.NewRemote(cfg, logger)
			defer r.Shutdown()
			began := time.Now()
			if err := r.Start(t.Context()); err == nil {
				t.Fatal("Start succeeded")
			} else {
				t.Logf("Start: %v", err)
			}
			if time.Since(began) > 5*time.Second {
				t.Errorf("Start took %s; it must fail before dialing", time.Since(began))
			}
		})
	}
}

func writeAccountSeed(t *testing.T) string {
	t.Helper()
	seed, _, err := brokerauth.GenerateAccountSeed()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "issuer.nk")
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRemote_SystemCredsMustBeASystemUser starts with a system credential
// that is not a system-account user. Revocation through it could never kick
// anything, so Start refuses, naming the setting.
func TestRemote_SystemCredsMustBeASystemUser(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{AuthEnabled: true})
	cfg := authRemoteConfig(c, newTestCreds().lookup)
	cfg.Auth.SystemCredsFile = c.DataSeed
	logger, _ := bufLogger()
	r := bus.NewRemote(cfg, logger)
	defer r.Shutdown()
	err := r.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded with the data user as the system credential")
	}
	t.Logf("Start: %v", err)
	if !strings.Contains(err.Error(), "system_creds_file") {
		t.Errorf("error %q does not name system_creds_file", err)
	}
}

func TestRemote_RevokeRejectsEmptyWorkerID(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{AuthEnabled: true})
	cfg := authRemoteConfig(c, newTestCreds().lookup)
	cfg.Auth.SystemCredsFile = c.SystemCreds
	logger, _ := bufLogger()
	r := startRemote(t, cfg, logger)
	// An empty user filter would match every connection in the account,
	// sqi's own data connection included.
	if err := r.Revoke(context.Background(), ""); err == nil {
		t.Fatal("Revoke(\"\") succeeded")
	}
	if err := r.Check(t.Context()); err != nil {
		t.Fatalf("Check after Revoke(\"\"): %v", err)
	}
}

// TestRemote_CheckRequiresCalloutConnection: with auth on, every worker
// connect needs the callout connection, so readiness does too.
func TestRemote_CheckRequiresCalloutConnection(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{AuthEnabled: true})
	logger, _ := bufLogger()
	r := startRemote(t, authRemoteConfig(c, newTestCreds().lookup), logger)
	if err := r.Check(t.Context()); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
	bus.CloseRemoteCallout(r)
	err := r.Check(t.Context())
	if err == nil || !strings.Contains(err.Error(), "callout") {
		t.Fatalf("Check with the callout connection closed = %v, want an error naming it", err)
	}
}

// TestRemote_RevokeWaitsOutTheAuthTimeout pins the final pass: Revoke
// returns no sooner than the cluster's auth timeout after it began, because
// an authorization already in flight can place its connection until then.
func TestRemote_RevokeWaitsOutTheAuthTimeout(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	cfg := authRemoteConfig(c, newTestCreds().lookup)
	cfg.Auth.SystemCredsFile = c.SystemCreds
	logger, _ := bufLogger()
	r := startRemote(t, cfg, logger)

	began := time.Now()
	if err := r.Revoke(context.Background(), "worker-a"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	took := time.Since(began)
	t.Logf("Revoke took %s", took.Round(time.Millisecond))
	// nats-server's default auth timeout without TLS is 2s.
	if took < 2*time.Second {
		t.Errorf("Revoke returned after %s, before the cluster's 2s auth timeout", took)
	}

	t.Run("a longer auth timeout", func(t *testing.T) {
		t.Parallel()
		const authTimeout = 4 * time.Second
		c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true, AuthTimeout: authTimeout})
		cfg := authRemoteConfig(c, newTestCreds().lookup)
		cfg.Auth.SystemCredsFile = c.SystemCreds
		logger, _ := bufLogger()
		r := startRemote(t, cfg, logger)
		began := time.Now()
		if err := r.Revoke(context.Background(), "worker-a"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		took := time.Since(began)
		t.Logf("Revoke took %s", took.Round(time.Millisecond))
		if took < authTimeout {
			t.Errorf("Revoke returned after %s, before the cluster's %s auth timeout", took, authTimeout)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := r.Revoke(ctx, "worker-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Revoke with a 200ms context = %v, want the context's error", err)
	}
}

// clusterConnections counts user's connections in sqi's account across every
// node of c, asking as c's system user.
func clusterConnections(t *testing.T, c *bustest.Cluster, user string) int {
	t.Helper()
	cred, err := nats.NkeyOptionFromSeed(c.SystemCreds)
	if err != nil {
		t.Fatal(err)
	}
	sys, err := nats.Connect(strings.Join(c.URLs, ","), cred)
	if err != nil {
		t.Fatalf("connect as the system user: %v", err)
	}
	defer sys.Close()
	sub, err := sys.SubscribeSync(sys.NewInbox())
	if err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(map[string]any{"auth": true, "user": user, "acc": bus.DefaultAccount})
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.PublishRequest("$SYS.REQ.SERVER.PING.CONNZ", sub.Subject, req); err != nil {
		t.Fatal(err)
	}
	total := 0
	for i := range c.URLs {
		m, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("CONNZ reply %d of %d: %v", i+1, len(c.URLs), err)
		}
		var resp struct {
			Data struct {
				Conns []struct {
					User string `json:"authorized_user"`
				} `json:"connections"`
			} `json:"data"`
		}
		if err := json.Unmarshal(m.Data, &resp); err != nil {
			t.Fatal(err)
		}
		for _, conn := range resp.Data.Conns {
			if conn.User == user {
				total++
			}
		}
	}
	return total
}

// TestRemote_RevokeCatchesInFlightAuthorization pins the window a single
// kick pass leaves open on a cluster: an authorization whose store read came
// before the revocation, and whose connection is placed only after Revoke's
// first pass. Revoke must still have kicked it by the time it returns.
func TestRemote_RevokeCatchesInFlightAuthorization(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	lookup := func(ctx context.Context, key string) (string, error) {
		if key == pub && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
			return "worker-a", nil // the answer from before the revocation
		}
		return creds.lookup(ctx, key)
	}
	cfg := authRemoteConfig(c, lookup)
	cfg.Auth.SystemCredsFile = c.SystemCreds
	logger, _ := bufLogger()
	r := startRemote(t, cfg, logger)

	type dialed struct {
		nc  *nats.Conn
		err error
	}
	dial := make(chan dialed, 1)
	go func() {
		nc, err := nats.Connect(c.URLs[2], nats.NoReconnect(), brokerauth.NkeyOption(pub, seed))
		dial <- dialed{nc, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker's authorization never reached the lookup")
	}

	// The revocation commits while that authorization is in flight.
	creds.remove(pub)
	began := time.Now()
	done := make(chan error, 1)
	go func() { done <- r.Revoke(context.Background(), "worker-a") }()

	// Let the first pass find nothing, then let the stale authorization
	// place its connection.
	time.Sleep(200 * time.Millisecond)
	close(release)
	d := <-dial
	if d.err != nil {
		t.Fatalf("the in-flight authorization was refused (%v); the test needs it to succeed to mean anything", d.err)
	}
	defer d.nc.Close()

	if err := <-done; err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	t.Logf("Revoke took %s", time.Since(began).Round(time.Millisecond))
	if n := clusterConnections(t, c, "worker-a"); n != 0 {
		t.Fatalf("the cluster holds %d connections for worker-a after Revoke returned", n)
	}
}

// systemRemote starts a remote broker with a system credential whose own
// connections all go to node 0 of c.
func systemRemote(t *testing.T, c *bustest.Cluster) *bus.Remote {
	t.Helper()
	cfg := authRemoteConfig(c, newTestCreds().lookup)
	cfg.Auth.SystemCredsFile = c.SystemCreds
	cfg.URLs = c.URLs[:1]
	logger, _ := bufLogger()
	return startRemote(t, cfg, logger)
}

// TestRemote_RevokeNeverReportsAnUncheckedSuccess covers the ways Revoke
// could return nil without having asked every node: each must be an error,
// since the worker may then still be connected.
func TestRemote_RevokeNeverReportsAnUncheckedSuccess(t *testing.T) {
	t.Parallel()
	t.Run("system connection closed", func(t *testing.T) {
		t.Parallel()
		c := bustest.StartCluster(t, 1, bustest.Options{AuthEnabled: true})
		r := systemRemote(t, c)
		bus.CloseRemoteSystem(r)
		err := r.Revoke(context.Background(), "worker-a")
		if err == nil || !strings.Contains(err.Error(), "system connection") {
			t.Fatalf("Revoke with the system connection closed = %v, want an error naming it", err)
		}
	})
	t.Run("no node answers", func(t *testing.T) {
		t.Parallel()
		c := bustest.StartCluster(t, 1, bustest.Options{AuthEnabled: true})
		r := systemRemote(t, c)
		bus.SetRemoteSystemSubjects(r, "$SYS.REQ.SERVER.PING.NOBODY", bus.VarzPingSubject)
		err := r.Revoke(context.Background(), "worker-a")
		if err == nil || !strings.Contains(err.Error(), "no node answered") {
			t.Fatalf("Revoke with no CONNZ answer = %v, want a no-node-answered error", err)
		}
	})
	t.Run("a node does not answer", func(t *testing.T) {
		t.Parallel()
		c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
		r := systemRemote(t, c)
		c.StopNode(2)
		err := r.Revoke(context.Background(), "worker-a")
		if err == nil || !strings.Contains(err.Error(), "only 2 of the cluster's 3 nodes") {
			t.Fatalf("Revoke with a node down = %v, want an error naming 2 of 3 nodes", err)
		}
	})
}

// TestRemote_RevokeUsesTheFreshAuthTimeout raises two nodes' auth timeout
// after Start. Revoke reads it alongside its first pass and waits it out.
func TestRemote_RevokeUsesTheFreshAuthTimeout(t *testing.T) {
	t.Parallel()
	const raised = 4 * time.Second
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	r := systemRemote(t, c)
	before := r.RevokeBudget()
	c.SetAuthTimeout(raised)
	c.RestartNode(1)
	c.RestartNode(2)

	var took time.Duration
	eventually(t, 30*time.Second, "a revoke every node answers", func() error {
		began := time.Now()
		err := r.Revoke(context.Background(), "worker-a")
		took = time.Since(began)
		return err
	})
	t.Logf("Revoke took %s", took.Round(time.Millisecond))
	if took < raised {
		t.Errorf("Revoke returned after %s, before the raised %s auth timeout", took, raised)
	}
	if after := r.RevokeBudget(); after < before+raised-2*time.Second {
		t.Errorf("RevokeBudget = %s after the raise (was %s), want it to grow by the raise", after, before)
	}
}

// TestRemote_RevokeFallsBackToTheStartAuthTimeout makes the fresh VARZ read
// fail. Revoke still waits out the longest auth timeout the nodes reported at
// Start.
func TestRemote_RevokeFallsBackToTheStartAuthTimeout(t *testing.T) {
	t.Parallel()
	const authTimeout = 4 * time.Second
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true, AuthTimeout: authTimeout})
	r := systemRemote(t, c)
	bus.SetRemoteSystemSubjects(r, bus.ConnzPingSubject, "$SYS.REQ.SERVER.PING.NOBODY")
	began := time.Now()
	if err := r.Revoke(context.Background(), "worker-a"); err != nil {
		t.Fatalf("Revoke with the fresh VARZ read failing: %v", err)
	}
	took := time.Since(began)
	t.Logf("Revoke took %s", took.Round(time.Millisecond))
	if took < authTimeout {
		t.Errorf("Revoke returned after %s, before the %s auth timeout read at Start", took, authTimeout)
	}
	if b := r.RevokeBudget(); b < authTimeout+2*time.Second {
		t.Errorf("RevokeBudget = %s, want at least the auth timeout plus the final pass", b)
	}
}
