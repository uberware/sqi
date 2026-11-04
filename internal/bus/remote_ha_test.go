// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/bus/bustest"
)

// haRemote is a remote broker for an HA server with member instance id, shut
// down when the test ends.
func haRemote(t *testing.T, cfg bus.RemoteConfig, instanceID string) *bus.Remote {
	t.Helper()
	cfg.InstanceID = instanceID
	logger, _ := bufLogger()
	r := bus.NewRemote(cfg, logger)
	t.Cleanup(r.Shutdown)
	return r
}

// startStandby starts r as a standby and fails the test on an error.
func startStandby(t *testing.T, r *bus.Remote, opts bus.GuardOptions) {
	t.Helper()
	if err := r.StartStandby(t.Context(), opts); err != nil {
		t.Fatalf("StartStandby: %v", err)
	}
}

// streamCount is how many streams the data account holds.
func streamCount(t *testing.T, js jetstream.JetStream) int {
	t.Helper()
	n := 0
	names := js.StreamNames(t.Context())
	for range names.Name() {
		n++
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list streams: %v", err)
	}
	return n
}

// requireNoGuardKey fails the test when the guard key exists.
func requireNoGuardKey(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bus.GuardBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return
	}
	if err != nil {
		t.Fatalf("open guard bucket: %v", err)
	}
	if _, err := kv.Get(t.Context(), "active"); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("guard key read = %v, want it absent", err)
	}
}

// requireConnected waits for r to report its data connection up.
func requireConnected(t *testing.T, r *bus.Remote, within time.Duration) time.Time {
	t.Helper()
	var since time.Time
	eventually(t, within, "Connected() to report the broker", func() error {
		up, s := r.Connected()
		if !up {
			return errors.New("not connected")
		}
		if s.IsZero() {
			return errors.New("connected with a zero since")
		}
		since = s
		return nil
	})
	return since
}

func TestRemote_StartStandbyTakesNoGuardAndCreatesNoStream(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	js := dataJS(t, c)
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	r := haRemote(t, authRemoteConfig(c, creds.lookup), bus.NewInstanceID())

	began := time.Now()
	startStandby(t, r, bus.GuardOptions{IsMember: members()})
	since := requireConnected(t, r, 5*time.Second)
	if since.Before(began.Add(-time.Second)) || since.After(time.Now()) {
		t.Errorf("Connected since %s, want a time during StartStandby (began %s)", since, began)
	}
	if err := r.Check(t.Context()); err != nil {
		t.Errorf("Check on a connected standby = %v, want nil", err)
	}
	requireStandbyChecked(t, r, time.Second)
	if err := r.StandbyReady(); err != nil {
		t.Errorf("StandbyReady() on a checked standby = %v, want nil", err)
	}
	if g := bus.RemoteGuard(r); g != nil {
		t.Error("a standby holds the guard")
	}
	requireNoGuardKey(t, js)
	if n := streamCount(t, js); n != 0 {
		t.Errorf("a standby left %d streams on the account, want none", n)
	}

	// The callout runs on a standby, so workers connect with no leader.
	nc := connectWorker(t, c.URLs, seed, pub)
	bus.AssertWorkerGrants(t, nc, "worker-a")
}

func TestRemote_StartStandbyWithBrokerDown(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	c.StopAll()

	cfg := authRemoteConfig(c, creds.lookup)
	r := haRemote(t, cfg, bus.NewInstanceID())
	began := time.Now()
	startStandby(t, r, bus.GuardOptions{IsMember: members()})
	if took := time.Since(began); took >= cfg.ConnectTimeout/2 {
		t.Errorf("StartStandby with no broker took %s, want it to return at once (connect timeout %s)", took, cfg.ConnectTimeout)
	}
	if up, since := r.Connected(); up || !since.IsZero() {
		t.Errorf("Connected() = %t, %s with every node down, want false and the zero time", up, since)
	}
	if err := r.Check(t.Context()); err == nil {
		t.Error("Check = nil with every node down, want an error")
	}
	if err := r.StandbyReady(); !errors.Is(err, bus.ErrStandbyUnchecked) {
		t.Errorf("StandbyReady() with every node down = %v, want ErrStandbyUnchecked", err)
	}
	select {
	case <-r.StandbyChecked():
		t.Error("StandbyChecked is closed before any broker answered")
	default:
	}
	if err := r.TakeLeadership(t.Context(), bus.GuardOptions{IsMember: members()}); err == nil {
		t.Error("TakeLeadership succeeded with no broker")
	}

	restarted := time.Now()
	for i := range c.URLs {
		c.RestartNode(i)
	}
	since := requireConnected(t, r, 20*time.Second)
	if since.Before(restarted) {
		t.Errorf("Connected since %s, before the cluster was restarted at %s", since, restarted)
	}
	// The checks StartStandby could not run, it runs on the first connect.
	requireStandbyChecked(t, r, 30*time.Second)
	if err := r.StandbyReady(); err != nil {
		t.Fatalf("StandbyReady() once the cluster is back = %v, want nil", err)
	}
	eventually(t, 20*time.Second, "Check healthy once the cluster is back", func() error { return r.Check(t.Context()) })

	// The callout starts once the checks pass.
	nc := connectWorker(t, c.URLs, seed, pub)
	bus.AssertWorkerGrants(t, nc, "worker-a")

	if err := r.TakeLeadership(t.Context(), bus.GuardOptions{IsMember: members()}); err != nil {
		t.Fatalf("TakeLeadership once the broker is back: %v", err)
	}
	if bus.RemoteGuard(r) == nil {
		t.Error("TakeLeadership took no guard")
	}
}

func TestRemote_TakeLeadershipProvisions(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	self := bus.NewInstanceID()
	r := haRemote(t, remoteConfig(c, bus.DefaultAccount, 3), self)
	opts := bus.GuardOptions{IsMember: members(self)}
	startStandby(t, r, opts)

	if err := r.TakeLeadership(t.Context(), opts); err != nil {
		t.Fatalf("TakeLeadership: %v", err)
	}
	g := bus.RemoteGuard(r)
	if g == nil {
		t.Fatal("TakeLeadership took no guard")
	}
	if err := g.Healthy(); err != nil {
		t.Errorf("guard Healthy = %v, want nil", err)
	}
	requireKeyHeldBy(t, js, bus.GuardHolder{InstanceID: self, Host: "this server"})
	for _, name := range allStreams {
		if got := streamInfo(t, js, name).Config.Replicas; got != 3 {
			t.Errorf("stream %s replicas = %d, want 3", name, got)
		}
	}
	if want := "external:" + bus.DefaultAccount + "@" + c.ClusterName; r.Identity() != want {
		t.Errorf("Identity() = %q, want %q", r.Identity(), want)
	}
	if err := r.Check(t.Context()); err != nil {
		t.Errorf("Check on the leader = %v, want nil", err)
	}
	if err := r.TakeLeadership(t.Context(), opts); err == nil {
		t.Error("a second TakeLeadership succeeded")
	}

	// Shutdown gives the key up, so the next leader need not wait.
	r.Shutdown()
	if rev := guardKeyRevision(t, js); rev != 0 {
		t.Errorf("guard key revision after Shutdown = %d, want the key released", rev)
	}
	if up, _ := r.Connected(); up {
		t.Error("Connected() = true after Shutdown")
	}
}

// TestRemote_TakeLeadershipOverwritesCrashedMember is a failover: the old
// leader died holding the key under its member instance id, and the new one
// overwrites it at once instead of waiting out the TTL.
func TestRemote_TakeLeadershipOverwritesCrashedMember(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	js := dataJS(t, c)
	oldID, newID := bus.NewInstanceID(), bus.NewInstanceID()
	opts := bus.GuardOptions{IsMember: members(oldID, newID)}

	old := haRemote(t, remoteConfig(c, bus.DefaultAccount, 3), oldID)
	startStandby(t, old, opts)
	if err := old.TakeLeadership(t.Context(), opts); err != nil {
		t.Fatalf("old leader TakeLeadership: %v", err)
	}
	requireKeyHeldBy(t, js, bus.GuardHolder{InstanceID: oldID, Host: "the old leader"})
	bus.StopGuardRefresh(bus.RemoteGuard(old))

	next := haRemote(t, remoteConfig(c, bus.DefaultAccount, 3), newID)
	startStandby(t, next, opts)
	began := time.Now()
	if err := next.TakeLeadership(t.Context(), opts); err != nil {
		t.Fatalf("new leader TakeLeadership: %v", err)
	}
	if took := time.Since(began); took >= bus.GuardTTL() {
		t.Errorf("taking over a member's key took %s, want it at once (TTL %s)", took, bus.GuardTTL())
	}
	requireKeyHeldBy(t, js, bus.GuardHolder{InstanceID: newID, Host: "the new leader"})
}

// requireStandbyChecked waits until r's standby checks have an outcome.
func requireStandbyChecked(t *testing.T, r *bus.Remote, within time.Duration) {
	t.Helper()
	select {
	case <-r.StandbyChecked():
	case <-time.After(within):
		t.Fatalf("the standby checks had no outcome within %s: %v", within, r.StandbyReady())
	}
}

// connectThroughout connects as a worker through url, one connect after
// another, until stop is closed, and reports how many were refused.
func connectThroughout(url string, seed []byte, pub string, stop <-chan struct{}) <-chan int {
	refused := make(chan int, 1)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				refused <- n
				return
			default:
			}
			nc, err := nats.Connect(url, nats.NoReconnect(), brokerauth.NkeyOption(pub, seed))
			if err != nil {
				n++
				continue
			}
			nc.Close()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return refused
}

// TestRemote_StandbyCheckRefusesForeignHolder runs a standby beside a live
// server of another database. The standby is refused, and while it watches
// that server's key it answers none of the cluster's callouts: they belong to
// the other farm.
func TestRemote_StandbyCheckRefusesForeignHolder(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	logger, _ := bufLogger()
	// A server on another database, holding and refreshing the guard.
	startRemote(t, authRemoteConfig(c, creds.lookup), logger)

	var answered atomic.Int64
	r := haRemote(t, authRemoteConfig(c, countingLookup(&answered, creds)), bus.NewInstanceID())
	stop := make(chan struct{})
	refused := connectThroughout(c.URLs[0], seed, pub, stop)
	began := time.Now()
	err := r.StartStandby(t.Context(), bus.GuardOptions{IsMember: members()})
	took := time.Since(began)
	close(stop)
	if n := <-refused; n != 0 {
		t.Errorf("%d worker connects were refused; the foreign server should have answered them", n)
	}
	if got := answered.Load(); got != 0 {
		t.Errorf("the refused standby answered %d callouts, want none", got)
	}
	if !errors.Is(err, bus.ErrGuardHeld) {
		t.Fatalf("StartStandby beside a live non-member = %v, want ErrGuardHeld", err)
	}
	if !strings.Contains(err.Error(), notMemberText) {
		t.Errorf("error %q does not say the holder is not a member", err)
	}
	if rerr := r.StandbyReady(); !errors.Is(rerr, bus.ErrGuardHeld) {
		t.Errorf("StandbyReady() after the refusal = %v, want ErrGuardHeld", rerr)
	}
	if took > bus.GuardTTL()+5*time.Second {
		t.Errorf("refused after %s, want within about one TTL (%s)", took, bus.GuardTTL())
	}
	if up, _ := r.Connected(); up {
		t.Error("a refused standby still reports a connection")
	}
}

// TestRemote_StandbyLateCheckRefusesForeignHolder starts a standby whose only
// node is down beside a live server of another database. When the node comes
// back the standby checks the guard, is refused, stays not ready, and answers
// no callout throughout.
func TestRemote_StandbyLateCheckRefusesForeignHolder(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	logger, _ := bufLogger()
	// The foreign server reaches the cluster through the nodes that stay up.
	foreign := authRemoteConfig(c, creds.lookup)
	foreign.URLs = c.URLs[:2]
	foreign.StreamReplicas = 3
	startRemote(t, foreign, logger)
	c.StopNode(2)

	var answered atomic.Int64
	cfg := authRemoteConfig(c, countingLookup(&answered, creds))
	cfg.URLs = c.URLs[2:]
	r := haRemote(t, cfg, bus.NewInstanceID())
	startStandby(t, r, bus.GuardOptions{IsMember: members()})
	if err := r.StandbyReady(); !errors.Is(err, bus.ErrStandbyUnchecked) {
		t.Errorf("StandbyReady() with its node down = %v, want ErrStandbyUnchecked", err)
	}

	stop := make(chan struct{})
	refused := connectThroughout(c.URLs[0], seed, pub, stop)
	c.RestartNode(2)
	requireConnected(t, r, 20*time.Second)
	requireStandbyChecked(t, r, 3*bus.GuardTTL()+20*time.Second)
	close(stop)
	if n := <-refused; n != 0 {
		t.Errorf("%d worker connects were refused; the foreign server should have answered them", n)
	}

	err := r.StandbyReady()
	if !errors.Is(err, bus.ErrGuardHeld) || !strings.Contains(err.Error(), notMemberText) {
		t.Fatalf("StandbyReady() beside a live non-member = %v, want ErrGuardHeld naming a non-member", err)
	}
	if cerr := r.Check(t.Context()); cerr == nil {
		t.Error("Check = nil on a refused standby, want it not ready")
	}
	if terr := r.TakeLeadership(t.Context(), bus.GuardOptions{IsMember: members()}); !errors.Is(terr, bus.ErrGuardHeld) {
		t.Errorf("TakeLeadership on a refused standby = %v, want its refusal", terr)
	}
	if got := answered.Load(); got != 0 {
		t.Errorf("the refused standby answered %d callouts, want none", got)
	}
	// A refusal is final: the callout stays off whatever is asked of it.
	r.SetCalloutActive(true)
	for range 5 {
		connectOnce(t, c.URLs[0], seed, pub)
	}
	if got := answered.Load(); got != 0 {
		t.Errorf("the refused standby answered %d callouts after SetCalloutActive(true), want none", got)
	}
}

func TestRemote_StandbyAcceptsMemberHolder(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 3, bustest.Options{})
	leaderID := bus.NewInstanceID()
	opts := bus.GuardOptions{IsMember: members(leaderID)}
	leader := haRemote(t, remoteConfig(c, bus.DefaultAccount, 3), leaderID)
	startStandby(t, leader, opts)
	if err := leader.TakeLeadership(t.Context(), opts); err != nil {
		t.Fatal(err)
	}

	r := haRemote(t, remoteConfig(c, bus.DefaultAccount, 3), bus.NewInstanceID())
	began := time.Now()
	startStandby(t, r, opts)
	if took := time.Since(began); took >= bus.GuardTTL() {
		t.Errorf("a standby beside a member leader took %s to start, want no watch", took)
	}
}

func TestRemote_StandbyStartRefusals(t *testing.T) {
	t.Parallel()
	urls := closedPorts(t, 1)
	cfg := bus.RemoteConfig{URLs: urls, Account: bus.DefaultAccount, StreamReplicas: 1, ConnectTimeout: time.Second}

	tests := []struct {
		name string
		run  func(r *bus.Remote) error
		want string
	}{
		{"no instance id", func(r *bus.Remote) error {
			return r.StartStandby(t.Context(), bus.GuardOptions{})
		}, "instance id"},
		{"invalid config", func(*bus.Remote) error {
			bad := bus.NewRemote(bus.RemoteConfig{Account: bus.DefaultAccount, InstanceID: "i"}, slog.New(slog.DiscardHandler))
			return bad.StartStandby(t.Context(), bus.GuardOptions{})
		}, "no URLs"},
		{"TakeLeadership before StartStandby", func(r *bus.Remote) error {
			return r.TakeLeadership(t.Context(), bus.GuardOptions{})
		}, "StartStandby"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bus.NewRemote(cfg, slog.New(slog.DiscardHandler))
			defer r.Shutdown()
			err := tt.run(r)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}

	t.Run("start twice", func(t *testing.T) {
		withID := cfg
		withID.InstanceID = bus.NewInstanceID()
		r := bus.NewRemote(withID, slog.New(slog.DiscardHandler))
		defer r.Shutdown()
		if err := r.StartStandby(t.Context(), bus.GuardOptions{}); err != nil {
			t.Fatalf("StartStandby: %v", err)
		}
		if err := r.StartStandby(t.Context(), bus.GuardOptions{}); err == nil {
			t.Error("a second StartStandby succeeded")
		}
		if err := r.Start(t.Context()); err == nil {
			t.Error("Start after StartStandby succeeded")
		}
	})
}

// countingLookup counts the authorizations it answers.
func countingLookup(n *atomic.Int64, creds *testCreds) bus.CredentialLookup {
	return func(ctx context.Context, key string) (string, error) {
		n.Add(1)
		return creds.lookup(ctx, key)
	}
}

// connectOnce makes one worker connect through url and closes it.
func connectOnce(t *testing.T, url string, seed []byte, pub string) {
	t.Helper()
	nc, err := nats.Connect(url, nats.NoReconnect(), brokerauth.NkeyOption(pub, seed))
	if err != nil {
		t.Fatalf("connect through %s: %v", url, err)
	}
	nc.Close()
}

// routeSettle is how long a queue-group change takes to reach every node of
// the cluster over its routes, after the node the responder is on has it.
const routeSettle = 250 * time.Millisecond

func TestRemote_CalloutPausedLeavesQueueGroup(t *testing.T) {
	t.Parallel()
	const connects = 20
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	var answeredA, answeredB atomic.Int64
	a := haRemote(t, authRemoteConfig(c, countingLookup(&answeredA, creds)), bus.NewInstanceID())
	b := haRemote(t, authRemoteConfig(c, countingLookup(&answeredB, creds)), bus.NewInstanceID())
	startStandby(t, a, bus.GuardOptions{})
	startStandby(t, b, bus.GuardOptions{})

	b.SetCalloutActive(false)
	b.SetCalloutActive(false) // idempotent
	time.Sleep(routeSettle)
	for i := range connects {
		connectOnce(t, c.URLs[i%len(c.URLs)], seed, pub)
	}
	if got := answeredA.Load(); got != connects {
		t.Errorf("the active responder answered %d of %d connects", got, connects)
	}
	if got := answeredB.Load(); got != 0 {
		t.Errorf("the paused responder answered %d connects, want none", got)
	}

	b.SetCalloutActive(true)
	b.SetCalloutActive(true) // idempotent: one subscription, not two
	time.Sleep(routeSettle)
	answeredA.Store(0)
	answeredB.Store(0)
	for i := range connects {
		connectOnce(t, c.URLs[i%len(c.URLs)], seed, pub)
	}
	ga, gb := answeredA.Load(), answeredB.Load()
	t.Logf("after rejoining: A answered %d, B answered %d", ga, gb)
	if ga+gb != connects {
		t.Errorf("the responders answered %d connects between them, want %d (each exactly once)", ga+gb, connects)
	}
	if gb == 0 {
		t.Error("the rejoined responder answered nothing; it did not rejoin the queue group")
	}
}

// TestRemote_CalloutToggleUnderLoad flips one responder in and out of the
// queue group while workers connect, under the race detector. Connects caught
// by a flip retry; every worker must end up connected, and Shutdown must not
// hang on a paused responder.
func TestRemote_CalloutToggleUnderLoad(t *testing.T) {
	t.Parallel()
	const workers = 12
	c := bustest.StartCluster(t, 3, bustest.Options{AuthEnabled: true})
	creds := newTestCreds()
	seed, pub := creds.enroll(t, "worker-a")
	a := haRemote(t, authRemoteConfig(c, creds.lookup), bus.NewInstanceID())
	b := haRemote(t, authRemoteConfig(c, creds.lookup), bus.NewInstanceID())
	startStandby(t, a, bus.GuardOptions{})
	startStandby(t, b, bus.GuardOptions{})

	stop := make(chan struct{})
	var flips sync.WaitGroup
	flips.Go(func() {
		active := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			b.SetCalloutActive(active)
			active = !active
			time.Sleep(5 * time.Millisecond)
		}
	})

	var conns sync.WaitGroup
	for i := range workers {
		conns.Go(func() {
			nc, err := nats.Connect(c.URLs[i%len(c.URLs)], brokerauth.NkeyOption(pub, seed),
				nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			defer nc.Close()
			deadline := time.Now().Add(20 * time.Second)
			for !nc.IsConnected() && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if !nc.IsConnected() {
				t.Errorf("worker %d not connected after 20s: %v", i, nc.LastError())
			}
		})
	}
	conns.Wait()
	close(stop)
	flips.Wait()

	b.SetCalloutActive(false)
	done := make(chan struct{})
	go func() {
		b.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown of a paused responder did not return")
	}
}

func TestRemote_SetCalloutActiveWithAuthOffIsNoOp(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	r := haRemote(t, remoteConfig(c, bus.DefaultAccount, 1), bus.NewInstanceID())
	startStandby(t, r, bus.GuardOptions{})
	r.SetCalloutActive(false)
	r.SetCalloutActive(true)
	if err := r.Check(t.Context()); err != nil {
		t.Errorf("Check = %v, want nil", err)
	}
}

func TestRemote_ConnectedTracksTheDataConnection(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	logger, _ := bufLogger()
	// Start, without HA, reports its connection too.
	r := startRemote(t, remoteConfig(c, bus.DefaultAccount, 1), logger)
	first := requireConnected(t, r, 5*time.Second)
	if _, again := r.Connected(); !again.Equal(first) {
		t.Errorf("since moved from %s to %s with no reconnect", first, again)
	}

	c.StopNode(0)
	eventually(t, 5*time.Second, "Connected() to report the loss", func() error {
		if up, since := r.Connected(); up || !since.IsZero() {
			return errors.New("still reported up")
		}
		return nil
	})
	c.RestartNode(0)
	if again := requireConnected(t, r, 20*time.Second); !again.After(first) {
		t.Errorf("since after the reconnect = %s, want later than %s", again, first)
	}
}

// TestRemote_StandbyLateCheckRefusesAccountWithoutJetStream starts a standby
// with its broker down on an account with no JetStream. Once the broker is
// back the check refuses, finally, rather than retrying.
func TestRemote_StandbyLateCheckRefusesAccountWithoutJetStream(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{NoJetStream: true})
	c.StopAll()
	r := haRemote(t, remoteConfig(c, bus.DefaultAccount, 1), bus.NewInstanceID())
	startStandby(t, r, bus.GuardOptions{IsMember: members()})

	c.RestartNode(0)
	requireStandbyChecked(t, r, 30*time.Second)
	err := r.StandbyReady()
	if err == nil || !strings.Contains(err.Error(), "has no JetStream") {
		t.Fatalf("StandbyReady() on an account without JetStream = %v, want the no-JetStream refusal", err)
	}
	if cerr := r.Check(t.Context()); cerr == nil {
		t.Error("Check = nil on a refused standby, want it not ready")
	}
}

// TestRemote_StartLogsNoLateConnect pins non-HA Start's logs: a broker that
// answers only after Start's first attempt is waited for, without the
// standby's "connection established" line.
func TestRemote_StartLogsNoLateConnect(t *testing.T) {
	t.Parallel()
	c := bustest.StartCluster(t, 1, bustest.Options{})
	c.StopAll()
	logger, logs := bufLogger()
	r := bus.NewRemote(remoteConfig(c, bus.DefaultAccount, 1), logger)
	t.Cleanup(r.Shutdown)
	started := make(chan error, 1)
	go func() { started <- r.Start(t.Context()) }()
	time.Sleep(500 * time.Millisecond)
	c.RestartNode(0)
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if strings.Contains(logs.String(), "connection established") {
		t.Errorf("Start logged a late connect: %s", logs.String())
	}
}
