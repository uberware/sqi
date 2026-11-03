// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
)

// lockedBuf is a log sink safe for the nats.go callback goroutines that log
// through the broker's logger while the test reads it.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func bufLogger() (*slog.Logger, *lockedBuf) {
	b := &lockedBuf{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

var allStreams = []string{bus.StreamTask, bus.StreamLogs, bus.StreamWorker, bus.StreamCancel}

// remoteConfig points a remote broker at c with the data user's seed.
func remoteConfig(c *bustest.Cluster, account string, replicas int) bus.RemoteConfig {
	return bus.RemoteConfig{
		URLs:           c.URLs,
		Account:        account,
		NkeySeedFile:   c.DataSeed,
		StreamReplicas: replicas,
		ConnectTimeout: 10 * time.Second,
	}
}

// startRemote starts a remote broker and shuts it down when the test ends.
func startRemote(t *testing.T, cfg bus.RemoteConfig, logger *slog.Logger) *bus.Remote {
	t.Helper()
	r := bus.NewRemote(cfg, logger)
	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(r.Shutdown)
	return r
}

// dataJS opens a JetStream context as sqi's data user, independent of any
// broker under test.
func dataJS(t *testing.T, c *bustest.Cluster) jetstream.JetStream {
	t.Helper()
	cred, err := nats.NkeyOptionFromSeed(c.DataSeed)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(strings.Join(c.URLs, ","), cred, nats.MaxReconnects(-1), nats.ReconnectWait(250*time.Millisecond))
	if err != nil {
		t.Fatalf("connect as data user: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

func streamInfo(t *testing.T, js jetstream.JetStream, name string) *jetstream.StreamInfo {
	t.Helper()
	s, err := js.Stream(t.Context(), name)
	if err != nil {
		t.Fatalf("stream %s: %v", name, err)
	}
	info, err := s.Info(t.Context())
	if err != nil {
		t.Fatalf("stream %s info: %v", name, err)
	}
	return info
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = cond(); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: not within %s: %v", what, timeout, err)
}

func TestRemote_ConnectsAndProvisionsStreams(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)

	if err := r.Check(t.Context()); err != nil {
		t.Fatalf("Check after Start = %v, want nil", err)
	}
	js := dataJS(t, c)
	for _, name := range allStreams {
		info := streamInfo(t, js, name)
		if info.Config.Replicas != 3 {
			t.Errorf("%s: configured replicas = %d, want 3", name, info.Config.Replicas)
		}
		if info.Cluster == nil || len(info.Cluster.Replicas)+1 != 3 {
			t.Errorf("%s: placed on %+v, want a leader and two replicas", name, info.Cluster)
		}
	}
}

func TestRemote_RaisesReplicasOnRestart(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	first := bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 1), logger)
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("Start with 1 replica: %v", err)
	}
	first.Shutdown()

	logger, logs := bufLogger()
	startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)

	js := dataJS(t, c)
	for _, name := range allStreams {
		if got := streamInfo(t, js, name).Config.Replicas; got != 3 {
			t.Errorf("%s: replicas = %d after restart with 3, want 3", name, got)
		}
	}
	var changed []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "1 → 3") {
			changed = append(changed, line)
		}
	}
	if len(changed) != 1 || !strings.Contains(changed[0], "level=INFO") {
		t.Fatalf("log lines naming the change 1 → 3 = %q, want exactly one INFO; log:\n%s", changed, logs)
	}
	for _, name := range allStreams {
		if !strings.Contains(changed[0], name) {
			t.Errorf("replica change line %q does not name %s", changed[0], name)
		}
	}
}

func TestRemote_AccountWithoutJetStreamFailsStartup(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{Account: "FARM", NoJetStream: true})
	logger, _ := bufLogger()
	r := bus.NewRemote(remoteConfig(c, "FARM", 1), logger)
	defer r.Shutdown()

	err := r.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded on an account without JetStream")
	}
	t.Logf("Start: %v", err)
	for _, want := range []string{`"FARM"`, "jetstream: enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %s", err, want)
		}
	}
}

// oneMinorBelowFloor is MinBrokerVersion with its minor decremented.
func oneMinorBelowFloor(t *testing.T) string {
	t.Helper()
	parts := strings.Split(bus.MinBrokerVersion, ".")
	minor, err := strconv.Atoi(parts[1])
	if err != nil || len(parts) != 3 {
		t.Fatalf("MinBrokerVersion %q is not major.minor.patch", bus.MinBrokerVersion)
	}
	return fmt.Sprintf("%s.%d.%s", parts[0], minor-1, parts[2])
}

// TestRemote_VersionBelowFloorRefused replaces the package's version reader,
// so it does not run in parallel. Go starts the parallel tests only once every
// sequential one has finished, by which time the reader is restored.
func TestRemote_VersionBelowFloorRefused(t *testing.T) {
	old := oneMinorBelowFloor(t)
	bus.SetBrokerVersion(t, func(*nats.Conn) string { return old })
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	r := bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 1), logger)
	defer r.Shutdown()

	err := r.Start(t.Context())
	if err == nil {
		t.Fatalf("Start accepted broker version %s", old)
	}
	t.Logf("Start: %v", err)
	for _, want := range []string{old, bus.MinBrokerVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name version %s", err, want)
		}
	}
	if err := r.Check(t.Context()); err == nil {
		t.Error("Check = nil after a refused Start")
	}
}

// TestRemote_ReconnectToOldNodeWarns reconnects the data connection to a
// node that reports a version below the floor. Start checks only the node it
// first reaches, so a node below the floor shows up on a reconnect. Not
// parallel: it replaces the version reader.
func TestRemote_ReconnectToOldNodeWarns(t *testing.T) {
	var old atomic.Bool
	below := oneMinorBelowFloor(t)
	bus.SetBrokerVersion(t, func(nc *nats.Conn) string {
		if old.Load() {
			return below
		}
		return nc.ConnectedServerVersion()
	})
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, logs := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 1), logger)

	old.Store(true)
	c.RestartNode(0)
	eventually(t, 10*time.Second, "a WARN naming the old version", func() error {
		for line := range strings.SplitSeq(logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, below) {
				return nil
			}
		}
		return errors.New("no WARN yet")
	})
	if err := r.Check(t.Context()); err != nil {
		t.Errorf("Check after the reconnect = %v; the WARN must not take the connection down", err)
	}
}

// closedPorts returns URLs on loopback ports nothing listens on.
func closedPorts(t *testing.T, n int) []string {
	t.Helper()
	var urls []string
	for range n {
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		urls = append(urls, "nats://"+l.Addr().String())
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return urls
}

func TestRemote_UnreachableFailsAfterConnectTimeout(t *testing.T) {
	t.Parallel()
	const timeout = time.Second
	urls := closedPorts(t, 3)
	logger, _ := bufLogger()
	r := bus.NewRemote(bus.RemoteConfig{
		URLs:           urls,
		Account:        bus.DefaultAccount,
		StreamReplicas: 1,
		ConnectTimeout: timeout,
	}, logger)
	defer r.Shutdown()

	began := time.Now()
	err := r.Start(t.Context())
	elapsed := time.Since(began)
	if err == nil {
		t.Fatal("Start succeeded with no broker listening")
	}
	t.Logf("Start after %s: %v", elapsed.Round(time.Millisecond), err)
	if elapsed < timeout || elapsed > timeout+5*time.Second {
		t.Errorf("Start failed after %s, want about the connect timeout %s", elapsed, timeout)
	}
	for _, u := range urls {
		if !strings.Contains(err.Error(), u) {
			t.Errorf("error %q does not list %s", err, u)
		}
	}
}

// deliveryBudget is how long a consumer may take to deliver after a broker
// restart or node loss. A pull lost while the consumer had no leader is
// replaced after two missed idle heartbeats (2×2s). The budget leaves room
// for leader election and stays well below the ~30s the library defaults
// take, so losing the pull tuning fails the test.
const deliveryBudget = 10 * time.Second

func TestRemote_SurvivesBrokerRestart(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)
	client, err := r.NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(client.Close)
	ctx := t.Context()

	// The status durable exists but is not consuming while the message is
	// published, so only the restarted cluster can deliver it.
	statusCC, err := client.ConsumeTaskStatus(ctx, func(jetstream.Msg) {})
	if err != nil {
		t.Fatal(err)
	}
	statusCC.Stop()
	if err := client.PublishTaskStatus(ctx, "w1", "before-restart", []byte("before-restart")); err != nil {
		t.Fatal(err)
	}

	// A consume context that stays open across the restart must keep
	// receiving afterwards.
	heartbeats := make(chan string, 16)
	if _, err := client.ConsumeWorker(ctx, func(m jetstream.Msg) {
		heartbeats <- string(m.Data())
		m.Ack() //nolint:errcheck // a lost ack is redelivered, which the test tolerates
	}); err != nil {
		t.Fatal(err)
	}

	for i := range c.URLs {
		c.StopNode(i)
	}
	eventually(t, 5*time.Second, "Check degraded with every node down", func() error {
		if r.Check(ctx) == nil {
			return errors.New("Check = nil")
		}
		return nil
	})
	for i := range c.URLs {
		c.RestartNode(i)
	}
	eventually(t, 20*time.Second, "Check healthy after the restart", func() error { return r.Check(ctx) })

	// The streams and consumers elect their leaders after the nodes are
	// back, so the first requests may time out.
	status := make(chan string, 4)
	eventually(t, 20*time.Second, "resume the status durable", func() error {
		_, err := client.ConsumeTaskStatus(ctx, func(m jetstream.Msg) {
			status <- string(m.Data())
			m.Ack() //nolint:errcheck // see above
		})
		return err
	})
	select {
	case got := <-status:
		if got != "before-restart" {
			t.Fatalf("status durable delivered %q, want before-restart", got)
		}
	case <-time.After(deliveryBudget):
		t.Fatal("the status message published before the restart was not delivered")
	}

	eventually(t, 20*time.Second, "publish a heartbeat after the restart", func() error {
		return client.PublishWorkerHeartbeat(ctx, "w1", []byte("after-restart"))
	})
	waited := time.Now()
	select {
	case got := <-heartbeats:
		if got != "after-restart" {
			t.Fatalf("worker consumer delivered %q, want after-restart", got)
		}
		t.Logf("the open consume context resumed %s after the publish", time.Since(waited).Round(time.Millisecond))
	case <-time.After(deliveryBudget):
		t.Fatalf("the consume context open across the restart received nothing within %s", deliveryBudget)
	}
}

// consumerLeader returns the index of the node leading the durable consumer.
func consumerLeader(t *testing.T, c *bustest.Cluster, js jetstream.JetStream, stream, durable string) int {
	t.Helper()
	var leader string
	eventually(t, 10*time.Second, "find the leader of "+durable, func() error {
		cons, err := js.Consumer(t.Context(), stream, durable)
		if err != nil {
			return err
		}
		info, err := cons.Info(t.Context())
		if err != nil {
			return err
		}
		if info.Cluster == nil || info.Cluster.Leader == "" {
			return errors.New("no leader yet")
		}
		leader = info.Cluster.Leader
		return nil
	})
	i := c.NodeIndex(leader)
	if i < 0 {
		t.Fatalf("consumer leader %q is not a harness node", leader)
	}
	return i
}

func TestRemote_SurvivesNodeLossAtR3(t *testing.T) {
	t.Parallel()
	const n = 100
	c := bustest.StartCluster(t, 3, bustest.Options{})
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 3), logger)
	client, err := r.NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(client.Close)
	ctx := t.Context()

	// The consumer is running before the node goes, and the node that goes
	// is the one leading it: the outstanding pull dies with it.
	var mu sync.Mutex
	seen := map[string]bool{}
	if _, err := client.ConsumeTaskStatus(ctx, func(m jetstream.Msg) {
		mu.Lock()
		seen[string(m.Data())] = true
		mu.Unlock()
		m.Ack() //nolint:errcheck // see TestRemote_SurvivesBrokerRestart
	}); err != nil {
		t.Fatal(err)
	}
	leader := consumerLeader(t, c, dataJS(t, c), bus.StreamTask, "sqi-task-status-srv")
	c.StopNode(leader)

	for i := range n {
		body := []byte(strconv.Itoa(i))
		eventually(t, 20*time.Second, "publish status "+string(body), func() error {
			return client.PublishTaskStatus(ctx, "w1", "job", body)
		})
	}
	eventually(t, deliveryBudget, "consume every status message", func() error {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != n {
			return fmt.Errorf("consumed %d distinct messages, want %d", len(seen), n)
		}
		return nil
	})
	for i := range n {
		if !seen[strconv.Itoa(i)] {
			t.Errorf("message %d was lost", i)
		}
	}
	t.Logf("stopped node n%d, the consumer's leader", leader)
}

// TestConsume_InterruptionIsLogged deletes a running consumer under its
// consume context. The context reports it, and sqi logs it at WARN under the
// consumer's name.
func TestConsume_InterruptionIsLogged(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, logs := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 1), logger)
	client, err := r.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if _, err := client.ConsumeTaskStatus(t.Context(), func(jetstream.Msg) {}); err != nil {
		t.Fatal(err)
	}
	if err := dataJS(t, c).DeleteConsumer(t.Context(), bus.StreamTask, "sqi-task-status-srv"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "a WARN naming the consumer", func() error {
		for line := range strings.SplitSeq(logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "consumer=sqi-task-status-srv") {
				t.Logf("logged: %s", line)
				return nil
			}
		}
		return errors.New("no WARN yet")
	})
}

func TestRemote_IdentityNamesAccountAndCluster(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{Account: "FARM"})
	if c.ClusterName == "" {
		t.Fatal("harness cluster has no name")
	}
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, "FARM", 1), logger)
	if got, want := r.Identity(), "external:FARM@"+c.ClusterName; got != want {
		t.Fatalf("Identity = %q, want %q", got, want)
	}
}

func TestRemote_RevokeIsNoOpWithAuthOff(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 1), logger)
	if err := r.Revoke(context.Background(), "w1"); err != nil {
		t.Fatalf("Revoke with auth off = %v, want nil", err)
	}
}

func TestRemote_StartTwiceRefused(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 1), logger)
	if err := r.Start(t.Context()); err == nil {
		t.Fatal("second Start succeeded")
	}
}

func TestRemote_StandaloneIdentityUsesServerName(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 1), logger)
	// The harness names its nodes n0, n1, ...; a single node has no cluster.
	if got, want := r.Identity(), "external:SQI@n0"; got != want {
		t.Fatalf("Identity = %q, want %q", got, want)
	}
}

// TestRemote_UnreachableRedactsCredentials checks that a startup failure
// names the broker URLs without their credentials: url.Redacted masks a
// password but leaves a bare token, which nats.go accepts as one.
func TestRemote_UnreachableRedactsCredentials(t *testing.T) {
	t.Parallel()
	closed := closedPorts(t, 2)
	urls := []string{
		strings.Replace(closed[0], "nats://", "nats://sqi:hunter2@", 1),
		strings.Replace(closed[1], "nats://", "nats://s3cr3tt0ken@", 1),
	}
	logger, logs := bufLogger()
	r := bus.NewRemote(bus.RemoteConfig{
		URLs:           urls,
		Account:        bus.DefaultAccount,
		StreamReplicas: 1,
		ConnectTimeout: 500 * time.Millisecond,
	}, logger)
	defer r.Shutdown()

	err := r.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded with no broker listening")
	}
	for _, secret := range []string{"hunter2", "s3cr3tt0ken"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("credential %q leaked; error %q, log:\n%s", secret, err, logs.String())
		}
	}
	for _, c := range closed {
		if want := strings.Replace(c, "nats://", "nats://REDACTED@", 1); !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// TestRemote_WarnsOnLooseSeedFiles checks the WARN for a server seed file
// that other accounts can read. The files are the operator's to manage, so it
// warns rather than refuses: Start goes on (and here fails only because no
// broker listens).
func TestRemote_WarnsOnLooseSeedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, mode os.FileMode, gen func() (nkeys.KeyPair, error)) string {
		t.Helper()
		kp, err := gen()
		if err != nil {
			t.Fatal(err)
		}
		seed, err := kp.Seed()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, seed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	data := write("data.nk", 0o644, nkeys.CreateUser)
	callout := write("auth.nk", 0o640, nkeys.CreateUser)
	issuer := write("issuer.nk", 0o604, nkeys.CreateAccount)
	system := write("sys.nk", 0o600, nkeys.CreateUser)

	logger, logs := bufLogger()
	r := bus.NewRemote(bus.RemoteConfig{
		URLs:           closedPorts(t, 1),
		Account:        bus.DefaultAccount,
		NkeySeedFile:   data,
		StreamReplicas: 1,
		ConnectTimeout: 200 * time.Millisecond,
		Auth: bus.RemoteAuthConfig{
			AuthConfig:          bus.AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: func(context.Context, string) (string, error) { return "", nil }},
			CalloutNkeySeedFile: callout,
			IssuerSeedFile:      issuer,
			SystemCredsFile:     system,
		},
	}, logger)
	defer r.Shutdown()
	if err := r.Start(t.Context()); err == nil {
		t.Fatal("Start succeeded with no broker listening")
	}

	var warns []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "readable beyond its owner") {
			warns = append(warns, line)
		}
	}
	for _, path := range []string{data, callout, issuer} {
		found := false
		for _, w := range warns {
			if strings.Contains(w, path) && strings.Contains(w, "chmod 600 "+path) {
				found = true
			}
		}
		if !found {
			t.Errorf("no WARN naming %s with its chmod 600 fix; log:\n%s", path, logs.String())
		}
	}
	for _, w := range warns {
		if strings.Contains(w, system) {
			t.Errorf("WARN for the owner-only %s: %s", system, w)
		}
	}
	if len(warns) != 3 {
		t.Errorf("seed-file WARN lines = %d, want 3; log:\n%s", len(warns), logs.String())
	}
}
