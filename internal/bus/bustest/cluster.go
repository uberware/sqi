// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bustest runs an in-process nats-server cluster booted from sqi's
// own account layout, for tests of the external broker mode. Each node is
// started from a config file, so the config-file form of the layout
// ([bus.Layout.ConfigFragmentWithSystemUser]) is what the cluster runs on.
//
// It is a test helper. Production code must not import it.
package bustest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
)

// stopAllGrace and stopAllDuration shape the lame duck shutdown of
// [Cluster.StopAll]: how long every node waits, with its listener already
// closed, before it starts closing its clients, and the time over which it
// closes them. nats-server's own defaults (10s and 2m) are for production.
const (
	stopAllGrace    = 100 * time.Millisecond
	stopAllDuration = time.Second
)

// readyTimeout bounds how long a node may take to accept client connections,
// and how long the cluster may take to elect a JetStream meta leader that
// sees every node.
const readyTimeout = 30 * time.Second

// Options shapes the cluster [StartCluster] boots.
type Options struct {
	// Account is sqi's account name. Empty means [bus.DefaultAccount].
	Account string

	// AuthEnabled renders the auth-callout layout: worker connections go to
	// a responder in the AUTH account, and the issuer and callout keys are
	// generated.
	AuthEnabled bool

	// PasswordUsers renders sqi's data and callout users, and the system
	// user, as username/password users instead of nkeys. A server with no
	// nkey user issues no nonce, so a worker presenting an nkey cannot
	// authenticate. The credentials are in [Cluster.DataUser] and the fields
	// after it; the user seed fields stay empty, and [Cluster.IssuerSeed] is
	// still written when AuthEnabled is set.
	PasswordUsers bool

	// NoJetStream leaves JetStream off for sqi's account. It stays on for the
	// servers, so the cluster still elects a meta leader.
	NoJetStream bool

	// AuthTimeout, when set, is every node's auth timeout: how long a node
	// waits for a client to authenticate, an auth-callout answer included.
	// Zero leaves nats-server's default (2s without TLS).
	AuthTimeout time.Duration

	// FragmentFile, when set, is an account layout file used verbatim instead
	// of one the harness renders. No keys are generated, so the credential
	// fields of [Cluster] stay empty.
	FragmentFile string
}

// Cluster is a running in-process nats-server cluster. Every node is routed
// to every other, runs JetStream with its store under the test's temporary
// directory, and keeps its ports across [Cluster.RestartNode].
type Cluster struct {
	// URLs are the nodes' client URLs, one per node, in node order.
	URLs []string

	// ClusterName is the cluster's name, as nats-server reports it in INFO.
	// Empty for a single node, which runs without a cluster block.
	ClusterName string

	// DataSeed, CalloutSeed and IssuerSeed are files holding the nkey seeds
	// of sqi's data user, its callout user (auth on only) and the callout
	// issuer account (auth on only).
	DataSeed    string
	CalloutSeed string
	IssuerSeed  string

	// SystemCreds is a file holding the nkey seed of the one user in the SYS
	// account. It is an nkey seed rather than a JWT .creds file because the
	// layout is non-operator, where nats-server authenticates nkey users by
	// their signature only and ignores a user JWT.
	SystemCreds string

	// DataUser, CalloutUser and SystemUser, with their passwords, are the
	// users' credentials when [Options.PasswordUsers] is set.
	DataUser        string
	DataPassword    string
	CalloutUser     string
	CalloutPassword string
	SystemUser      string
	SystemPassword  string

	t           testing.TB
	authTimeout time.Duration
	mu          sync.Mutex
	nodes       []*node
}

// node is one cluster member. conf, storeDir and the ports never change, so a
// restarted node comes back as the same member with the same data.
type node struct {
	name string
	conf string
	srv  *natsserver.Server
}

// StartCluster boots nodes routed nats-server nodes from sqi's account layout
// and waits until each accepts connections and, with more than one node, a
// JetStream meta leader sees all of them. The cluster is shut down when the
// test ends.
func StartCluster(t testing.TB, nodes int, o Options) *Cluster {
	t.Helper()
	if nodes < 1 {
		t.Fatalf("bustest: StartCluster needs at least one node, got %d", nodes)
	}
	c := &Cluster{t: t, authTimeout: o.AuthTimeout}
	frag := c.fragment(o)

	clientPorts := freePorts(t, nodes)
	clusterPorts := freePorts(t, nodes)
	if nodes > 1 {
		c.ClusterName = "sqi-test-" + randomHex(t, 4)
	}
	dir := t.TempDir()
	for i := range nodes {
		name := fmt.Sprintf("n%d", i)
		text := c.nodeConfig(name, filepath.Join(dir, name, "jetstream"), i, clientPorts, clusterPorts) + frag
		conf := filepath.Join(dir, name+".conf")
		if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
			t.Fatalf("bustest: write %s: %v", conf, err)
		}
		c.nodes = append(c.nodes, &node{name: name, conf: conf})
		c.URLs = append(c.URLs, fmt.Sprintf("nats://127.0.0.1:%d", clientPorts[i]))
	}
	t.Cleanup(c.Shutdown)

	for _, n := range c.nodes {
		c.boot(n)
	}
	for _, n := range c.nodes {
		c.waitReady(n)
	}
	c.waitMetaLeader()
	return c
}

// fragment returns the account layout text every node includes, generating
// the keys it names unless o.FragmentFile supplies the layout.
func (c *Cluster) fragment(o Options) string {
	t := c.t
	t.Helper()
	if o.FragmentFile != "" {
		b, err := os.ReadFile(o.FragmentFile)
		if err != nil {
			t.Fatalf("bustest: read fragment: %v", err)
		}
		return string(b)
	}

	account := o.Account
	if account == "" {
		account = bus.DefaultAccount
	}
	dir := t.TempDir()
	layout := bus.Layout{Account: account, AuthEnabled: o.AuthEnabled}
	var dataSeed, calloutSeed, issuerSeed []byte
	dataSeed, layout.DataUserPub = generate(t, brokerauth.GenerateSeed)
	if o.AuthEnabled {
		calloutSeed, layout.CalloutUserPub = generate(t, brokerauth.GenerateSeed)
		issuerSeed, layout.IssuerPub = generate(t, brokerauth.GenerateAccountSeed)
	}
	systemSeed, systemPub := generate(t, brokerauth.GenerateSeed)

	frag, err := layout.ConfigFragmentWithSystemUser(systemPub)
	if err != nil {
		t.Fatalf("bustest: render layout: %v", err)
	}
	if o.NoJetStream {
		frag = mustReplace(t, frag, "    jetstream: enabled\n", "")
	}
	if o.AuthEnabled {
		// The issuer is an account key, not a user, so it is written in both
		// forms: a responder needs it to sign answers either way.
		c.IssuerSeed = writeSecret(t, dir, "issuer.nk", issuerSeed)
	}
	if o.PasswordUsers {
		return c.passwordUsers(frag, layout, systemPub)
	}

	c.DataSeed = writeSecret(t, dir, "sqi-data.nk", dataSeed)
	c.SystemCreds = writeSecret(t, dir, "system.nk", systemSeed)
	if o.AuthEnabled {
		c.CalloutSeed = writeSecret(t, dir, "sqi-auth.nk", calloutSeed)
	}
	return frag
}

// passwordUsers rewrites frag's nkey users as username/password users. Each
// public key appears as `nkey: "<key>"` in its account's users and as a bare
// quoted key in auth_users or no_auth_user, and both become the username.
func (c *Cluster) passwordUsers(frag string, layout bus.Layout, systemPub string) string {
	t := c.t
	t.Helper()
	c.DataUser, c.DataPassword = "sqi-data", randomHex(t, 16)
	c.SystemUser, c.SystemPassword = "sqi-system", randomHex(t, 16)
	users := []struct{ pub, user, password string }{
		{layout.DataUserPub, c.DataUser, c.DataPassword},
		{systemPub, c.SystemUser, c.SystemPassword},
	}
	if layout.AuthEnabled {
		c.CalloutUser, c.CalloutPassword = "sqi-auth", randomHex(t, 16)
		users = append(users, struct{ pub, user, password string }{layout.CalloutUserPub, c.CalloutUser, c.CalloutPassword})
	}
	for _, u := range users {
		frag = mustReplace(t, frag, fmt.Sprintf("nkey: %q", u.pub), fmt.Sprintf("user: %q, password: %q", u.user, u.password))
		frag = strings.ReplaceAll(frag, fmt.Sprintf("%q", u.pub), fmt.Sprintf("%q", u.user))
	}
	if strings.Contains(frag, "nkey:") {
		t.Fatalf("bustest: password layout still has an nkey user:\n%s", frag)
	}
	return frag
}

// nodeConfig renders node i's own settings: its name, client listener,
// JetStream store and, with more than one node, its cluster listener and
// routes to every other node.
func (c *Cluster) nodeConfig(name, storeDir string, i int, clientPorts, clusterPorts []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "server_name: %q\n", name)
	fmt.Fprintf(&b, "listen: 127.0.0.1:%d\n", clientPorts[i])
	fmt.Fprintf(&b, "jetstream {store_dir: %q}\n", storeDir)
	if len(clusterPorts) > 1 {
		var routes []string
		for j, p := range clusterPorts {
			if j != i {
				routes = append(routes, fmt.Sprintf("%q", fmt.Sprintf("nats-route://127.0.0.1:%d", p)))
			}
		}
		b.WriteString("cluster {\n")
		fmt.Fprintf(&b, "  name: %q\n", c.ClusterName)
		fmt.Fprintf(&b, "  listen: 127.0.0.1:%d\n", clusterPorts[i])
		fmt.Fprintf(&b, "  routes: [%s]\n", strings.Join(routes, ", "))
		b.WriteString("}\n")
	}
	return b.String()
}

// boot starts n from its config file without waiting for it.
func (c *Cluster) boot(n *node) {
	t := c.t
	t.Helper()
	opts, err := natsserver.ProcessConfigFile(n.conf)
	if err != nil {
		t.Fatalf("bustest: %s: parse config: %v", n.name, err)
	}
	opts.NoSigs = true
	opts.NoLog = true
	// The config file cannot set a lame duck duration under 30s.
	opts.LameDuckGracePeriod = stopAllGrace
	opts.LameDuckDuration = stopAllDuration
	c.mu.Lock()
	authTimeout := c.authTimeout
	c.mu.Unlock()
	if authTimeout > 0 {
		opts.AuthTimeout = authTimeout.Seconds()
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("bustest: %s: create server: %v", n.name, err)
	}
	srv.Start()
	c.mu.Lock()
	n.srv = srv
	c.mu.Unlock()
}

func (c *Cluster) waitReady(n *node) {
	t := c.t
	t.Helper()
	c.mu.Lock()
	srv := n.srv
	c.mu.Unlock()
	if !srv.ReadyForConnections(readyTimeout) {
		t.Fatalf("bustest: %s did not accept connections within %s", n.name, readyTimeout)
	}
}

// waitMetaLeader waits until a JetStream meta leader sees every node. A
// single node is not clustered and has no meta leader.
func (c *Cluster) waitMetaLeader() {
	t := c.t
	t.Helper()
	if len(c.nodes) < 2 {
		return
	}
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if c.metaLeaderSeesAll() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("bustest: no JetStream meta leader saw all %d nodes within %s", len(c.nodes), readyTimeout)
}

func (c *Cluster) metaLeaderSeesAll() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.nodes {
		if n.srv != nil && n.srv.JetStreamIsLeader() {
			return len(n.srv.JetStreamClusterPeers()) == len(c.nodes)
		}
	}
	return false
}

func (c *Cluster) node(i int) *node {
	c.t.Helper()
	if i < 0 || i >= len(c.nodes) {
		c.t.Fatalf("bustest: no node %d in a %d-node cluster", i, len(c.nodes))
	}
	return c.nodes[i]
}

// NodeIndex returns the index of the node whose server_name is name, as
// nats-server reports it (for example as a stream or consumer leader), or -1.
func (c *Cluster) NodeIndex(name string) int {
	for i, n := range c.nodes {
		if n.name == name {
			return i
		}
	}
	return -1
}

// StopNode shuts node i down and waits until it has exited. Stopping a
// stopped node does nothing.
func (c *Cluster) StopNode(i int) {
	c.t.Helper()
	n := c.node(i)
	c.mu.Lock()
	srv := n.srv
	n.srv = nil
	c.mu.Unlock()
	if srv != nil {
		srv.Shutdown()
		srv.WaitForShutdown()
	}
}

// SetAuthTimeout changes the auth timeout every node boots with from now
// on. Running nodes keep theirs until [Cluster.RestartNode].
func (c *Cluster) SetAuthTimeout(d time.Duration) {
	c.mu.Lock()
	c.authTimeout = d
	c.mu.Unlock()
}

// StopAll stops every node and waits until all have exited, the way an
// operator restarting a whole cluster under live clients should: every node
// enters lame duck mode at once, so all client listeners are closed before
// any node starts shutting its internals down, and no client can reconnect
// to a node that is stopping.
//
// Stopping the nodes one at a time with [Cluster.StopNode] under clients that
// reconnect through the auth callout can crash nats-server v2.15.2: a
// CONNECT that a node processes after its system account has shut down, but
// before its listener closes, dereferences nil in
// processClientOrLeafCallout. Shutdown is the only exposure, so the cluster
// harness, which runs every node inside the test process, avoids it.
func (c *Cluster) StopAll() {
	c.t.Helper()
	var wg sync.WaitGroup
	for _, n := range c.nodes {
		c.mu.Lock()
		srv := n.srv
		n.srv = nil
		c.mu.Unlock()
		if srv == nil {
			continue
		}
		wg.Go(func() {
			srv.LameDuckShutdown()
			// Lame duck mode returns without shutting down when it has
			// nothing to do.
			srv.Shutdown()
			srv.WaitForShutdown()
		})
	}
	wg.Wait()
}

// RestartNode stops node i if it is running and starts it again on the same
// ports and store, waiting until it accepts connections. When every node is
// then running, it also waits for a meta leader that sees all of them, so a
// test can restart the whole cluster by stopping every node and restarting
// each in turn.
func (c *Cluster) RestartNode(i int) {
	c.t.Helper()
	c.StopNode(i)
	n := c.node(i)
	c.boot(n)
	c.waitReady(n)
	if c.allRunning() {
		c.waitMetaLeader()
	}
}

func (c *Cluster) allRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.nodes {
		if n.srv == nil {
			return false
		}
	}
	return true
}

// Shutdown stops every node. It is safe to call more than once.
func (c *Cluster) Shutdown() {
	for i := range c.nodes {
		c.StopNode(i)
	}
}

// freePorts reserves n distinct loopback ports and releases them. A port can
// in principle be taken before the node binds it; tests accept that race.
func freePorts(t testing.TB, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for range n {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("bustest: reserve port: %v", err)
		}
		listeners = append(listeners, l)
		addr, ok := l.Addr().(*net.TCPAddr)
		if !ok {
			t.Fatalf("bustest: unexpected listener address %v", l.Addr())
		}
		ports = append(ports, addr.Port)
	}
	return ports
}

func generate(t testing.TB, gen func() ([]byte, string, error)) (seed []byte, pub string) {
	t.Helper()
	seed, pub, err := gen()
	if err != nil {
		t.Fatalf("bustest: generate key: %v", err)
	}
	return seed, pub
}

func writeSecret(t testing.TB, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("bustest: write %s: %v", path, err)
	}
	return path
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("bustest: random: %v", err)
	}
	return hex.EncodeToString(b)
}

// mustReplace replaces every old in s with replacement, failing the test when
// old does not occur: a silent no-op would boot a layout other than the one
// the test asked for.
func mustReplace(t testing.TB, s, old, replacement string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("bustest: layout has no %q to replace:\n%s", old, s)
	}
	return strings.ReplaceAll(s, old, replacement)
}
