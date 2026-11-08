// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// Unit tests for [Server.RevokeWorker] — the method that turns DELETE
// /api/v1/workers/{id}/credential into a synchronous disconnect, as opposed
// to "sqi-server worker revoke", which only ever writes the store from a
// separate process holding no broker handle.
//
// These exercise RevokeWorker directly against a *Server built by hand
// (store + a real embedded broker whose auth callout reads that store, no
// HTTP layer, no scheduler) so the store-write-then-disconnect ordering and
// its failure mode are pinned at the unit level. The full
// HTTP-to-disconnect-to-reclaim path is covered by
// test/integration's broker-auth suite, which also proves the existing
// heartbeat-sweep/reclaim path — not anything reimplemented here — is what
// returns a revoked worker's task to ready.

// freeLoopbackAddr asks the OS for an available loopback TCP port and
// returns "127.0.0.1:<port>", releasing it immediately so the broker under
// test can bind it.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeLoopbackAddr: listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("freeLoopbackAddr: close: %v", err)
	}
	return addr
}

// startTestBroker boots a real embedded broker with authentication enabled,
// answering connects from st's active credentials, and registers cleanup.
func startTestBroker(t *testing.T, st store.WorkerCredentialStore, mutate ...func(*bus.EmbeddedConfig)) *bus.Embedded {
	t.Helper()
	cfg := bus.EmbeddedConfig{
		Addr:       freeLoopbackAddr(t),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
		Auth:       bus.AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: credentialLookup(st)},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	b := bus.NewEmbedded(cfg, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startTestBroker: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// testCredential is an enrolled worker's ID and public key.
type testCredential struct {
	WorkerID  string
	PublicKey string
}

// enrolledCredential generates a fresh nkey, seeds a matching
// [store.WorkerCredential] row in st, and returns it with the raw seed
// needed to connect as that worker.
func enrolledCredential(t *testing.T, st store.Store, workerID string) (testCredential, []byte) {
	t.Helper()
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("enrolledCredential: GenerateSeed: %v", err)
	}
	if _, err := st.CreateWorkerCredential(context.Background(), store.WorkerCredential{
		ID:         workerID + "-cred",
		WorkerID:   workerID,
		PublicKey:  pub,
		EnrolledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enrolledCredential: CreateWorkerCredential: %v", err)
	}
	return testCredential{WorkerID: workerID, PublicKey: pub}, seed
}

// nkeyOption builds a nats.Option that authenticates as the nkey pair
// identified by pub, signing server challenges with seed.
func nkeyOption(t *testing.T, seed []byte, pub string) nats.Option {
	t.Helper()
	return nats.Nkey(pub, func(nonce []byte) ([]byte, error) {
		kp, err := nkeys.FromSeed(seed)
		if err != nil {
			return nil, err
		}
		return kp.Sign(nonce)
	})
}

// connectAsWorker dials b as the given nkey credential, with NoReconnect and
// a ClosedHandler feeding the returned channel — the same pattern
// internal/bus's own revocation tests use, so the disconnect assertion is
// not coupled to nats.go's reconnect/backoff timing.
func connectAsWorker(t *testing.T, b *bus.Embedded, seed []byte, pub string) (*nats.Conn, <-chan struct{}) {
	t.Helper()
	closedCh := make(chan struct{})
	nc, err := nats.Connect(
		b.ClientURL(),
		nkeyOption(t, seed, pub),
		nats.NoReconnect(),
		nats.ClosedHandler(func(*nats.Conn) { close(closedCh) }),
	)
	if err != nil {
		t.Fatalf("connectAsWorker: Connect: %v", err)
	}
	t.Cleanup(func() {
		if !nc.IsClosed() {
			nc.Close()
		}
	})
	return nc, closedCh
}

// TestRevokeWorker_NATSAuthDisabled_StoreWriteOnly covers the farm that runs
// without broker authentication at all: RevokeWorker must still perform the
// store write (so an operator can pre-provision revocations, or clean up
// after auth was turned off) but must never touch the broker: there is
// nothing to disconnect. s.broker is left nil so that touching it would
// panic.
func TestRevokeWorker_NATSAuthDisabled_StoreWriteOnly(t *testing.T) {
	st := fake.New()
	ref, _ := enrolledCredential(t, st, "worker-a")

	s := &Server{cfg: brokerAuthConfig(false), store: st, logger: testLogger()}

	if err := s.RevokeWorker(context.Background(), ref.WorkerID); err != nil {
		t.Fatalf("RevokeWorker: %v", err)
	}

	if _, err := storetest.ActiveWorkerCredential(context.Background(), st, ref.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ActiveWorkerCredential after revoke: %v, want store.ErrNotFound", err)
	}
}

// TestRevokeWorker_UnknownWorker_ReturnsErrNotFoundWithoutTouchingBroker
// proves the store write happens FIRST: with no credential for "ghost" in
// the store, RevokeWorker must fail and return before ever reaching s.broker
// — which is nil here, so touching it would panic — even though
// NATSAuthEnabled is true.
func TestRevokeWorker_UnknownWorker_ReturnsErrNotFoundWithoutTouchingBroker(t *testing.T) {
	st := fake.New()
	s := &Server{cfg: brokerAuthConfig(true), store: st, logger: testLogger()}

	err := s.RevokeWorker(context.Background(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RevokeWorker(\"ghost\") = %v, want store.ErrNotFound", err)
	}
}

// TestRevokeWorker_DisconnectsRevokedWorkerOnly is the core positive case:
// revoking worker A through Server.RevokeWorker disconnects A's live broker
// connection inside the call and leaves B connected.
func TestRevokeWorker_DisconnectsRevokedWorkerOnly(t *testing.T) {
	st := fake.New()
	refA, seedA := enrolledCredential(t, st, "worker-a")
	refB, seedB := enrolledCredential(t, st, "worker-b")

	broker := startTestBroker(t, st)
	s := &Server{cfg: brokerAuthConfig(true), store: st, broker: broker, logger: testLogger()}

	_, closedA := connectAsWorker(t, broker, seedA, refA.PublicKey)
	ncB, closedB := connectAsWorker(t, broker, seedB, refB.PublicKey)

	if err := s.RevokeWorker(context.Background(), refA.WorkerID); err != nil {
		t.Fatalf("RevokeWorker: %v", err)
	}

	select {
	case <-closedA:
	case <-time.After(2 * time.Second):
		t.Fatal("worker A's connection was not closed after revocation")
	}

	select {
	case <-closedB:
		t.Fatal("worker B's connection was closed by an unrelated revocation")
	case <-time.After(200 * time.Millisecond):
	}
	if err := ncB.Flush(); err != nil {
		t.Fatalf("worker B's connection unusable after A's revocation: %v", err)
	}

	if _, err := storetest.ActiveWorkerCredential(context.Background(), st, refA.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A's credential still active after revoke: %v, want store.ErrNotFound", err)
	}
	if _, err := storetest.ActiveWorkerCredential(context.Background(), st, refB.WorkerID); err != nil {
		t.Errorf("B's credential was disturbed by A's revocation: %v", err)
	}
}

// revokeStubBroker is a [bus.Broker] whose Revoke records what it was called
// with. Every other method is unused by RevokeWorker.
type revokeStubBroker struct {
	bus.Broker // nil: any other method panics, which RevokeWorker must never reach

	err    error
	budget time.Duration

	calls       int
	workerID    string
	ctxErr      error
	hasDeadline bool
	deadline    time.Time
}

func (b *revokeStubBroker) RevokeBudget() time.Duration { return b.budget }

func (b *revokeStubBroker) Revoke(ctx context.Context, workerID string) error {
	b.calls++
	b.workerID = workerID
	b.ctxErr = ctx.Err()
	b.deadline, b.hasDeadline = ctx.Deadline()
	return b.err
}

// TestRevokeWorker_BrokerFailure_StoreStaysRevoked pins the outcome of a
// broker failure after a successful store write: the credential row stays
// revoked (the store is never rolled back) and the failure is still returned
// to the caller, since the synchronous disconnect did not happen.
func TestRevokeWorker_BrokerFailure_StoreStaysRevoked(t *testing.T) {
	st := fake.New()
	ref, _ := enrolledCredential(t, st, "worker-a")

	boom := errors.New("broker unreachable")
	stub := &revokeStubBroker{err: boom}
	s := &Server{cfg: brokerAuthConfig(true), store: st, broker: stub, logger: testLogger()}

	err := s.RevokeWorker(context.Background(), ref.WorkerID)
	if !errors.Is(err, boom) {
		t.Fatalf("RevokeWorker = %v, want an error wrapping the broker's", err)
	}
	if stub.calls != 1 || stub.workerID != ref.WorkerID {
		t.Errorf("broker.Revoke calls = %d for %q, want 1 for %q", stub.calls, stub.workerID, ref.WorkerID)
	}

	// The store write must stand regardless: rolling it back on a broker
	// failure would leave a credential the operator explicitly revoked
	// silently trusted again, which is worse than a disconnect that has not
	// happened yet.
	if _, err := storetest.ActiveWorkerCredential(context.Background(), st, ref.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("credential still active after a broker failure: %v, want store.ErrNotFound", err)
	}
}

// TestRevokeWorker_CancelledRequestStillGetsALiveBrokerContext pins that the
// broker's final pass is not cut off by the HTTP client going away: Revoke
// runs for seconds, and a disconnecting client cancels the request context
// partway through. RevokeWorker must hand the broker a context detached from
// that cancellation, bounded by its own timeout.
func TestRevokeWorker_CancelledRequestStillGetsALiveBrokerContext(t *testing.T) {
	st := fake.New()
	ref, _ := enrolledCredential(t, st, "worker-a")

	stub := &revokeStubBroker{}
	s := &Server{cfg: brokerAuthConfig(true), store: st, broker: stub, logger: testLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone when RevokeWorker runs

	// The store write must not be skipped either. The fake ignores the
	// context; the integration suite exercises the real store's write path.
	if err := s.RevokeWorker(ctx, ref.WorkerID); err != nil {
		t.Fatalf("RevokeWorker: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("broker.Revoke calls = %d, want 1", stub.calls)
	}
	if stub.ctxErr != nil {
		t.Errorf("broker.Revoke saw ctx.Err() = %v, want nil: the request's cancellation must not reach the broker", stub.ctxErr)
	}
	if !stub.hasDeadline {
		t.Fatal("broker.Revoke ctx has no deadline, want one bounding a stuck broker")
	}
	if left := time.Until(stub.deadline); left <= 0 || left > revokeBrokerTimeout {
		t.Errorf("broker.Revoke deadline is %s away, want within (0, %s]", left, revokeBrokerTimeout)
	}
}

// TestRevokeRemovedWorker_DropsConnectionOfARevokedCredential pins the
// scheduler's hook: the deletion of an ephemeral capacity-group member has
// already revoked its credential, and the connection must be dropped anyway,
// where Server.RevokeWorker would stop at the store's ErrNotFound. The hook
// writes nothing to the store: an active credential (one enrolled under the
// same worker ID since the deletion) is left active. With broker
// authentication off there is nothing to drop and no error.
func TestRevokeRemovedWorker_DropsConnectionOfARevokedCredential(t *testing.T) {
	tests := []struct {
		name      string
		auth      bool
		revoked   bool
		wantCalls int
	}{
		{"credential already revoked", true, true, 1},
		{"credential still active", true, false, 1},
		{"auth off, credential already revoked", false, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			ref, _ := enrolledCredential(t, st, "worker-a")
			if tt.revoked {
				if err := st.RevokeWorkerCredential(context.Background(), ref.WorkerID, time.Now().UTC()); err != nil {
					t.Fatalf("seed RevokeWorkerCredential: %v", err)
				}
			}
			stub := &revokeStubBroker{}
			s := &Server{cfg: brokerAuthConfig(tt.auth), store: st, broker: stub, logger: testLogger()}

			// Through the wiring the scheduler is built with.
			revoke := s.schedulerConfigFor(nil).RevokeWorker
			if revoke == nil {
				t.Fatal("scheduler config has no RevokeWorker hook")
			}
			if err := revoke(context.Background(), ref.WorkerID); err != nil {
				t.Fatalf("RevokeWorker hook: %v", err)
			}
			if stub.calls != tt.wantCalls {
				t.Errorf("broker.Revoke calls = %d, want %d", stub.calls, tt.wantCalls)
			}
			_, err := storetest.ActiveWorkerCredential(context.Background(), st, ref.WorkerID)
			switch {
			case tt.revoked && !errors.Is(err, store.ErrNotFound):
				t.Errorf("active credential after the hook: %v, want store.ErrNotFound", err)
			case !tt.revoked && err != nil:
				t.Errorf("active credential after the hook: %v, want it left active", err)
			}
		})
	}
}

// TestRevokeWorker_BrokerBudgetExtendsTheBound pins that a broker needing
// longer than revokeBrokerTimeout, an external cluster with a long auth
// timeout, gets its own budget: a shorter bound would cut its final pass
// off and fail every revocation.
func TestRevokeWorker_BrokerBudgetExtendsTheBound(t *testing.T) {
	for _, tt := range []struct {
		name   string
		budget time.Duration
		want   time.Duration
	}{
		{"short budget keeps the floor", 3 * time.Second, revokeBrokerTimeout},
		{"long budget extends it", 30 * time.Second, 30 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			ref, _ := enrolledCredential(t, st, "worker-a")
			stub := &revokeStubBroker{budget: tt.budget}
			s := &Server{cfg: brokerAuthConfig(true), store: st, broker: stub, logger: testLogger()}
			if err := s.RevokeWorker(context.Background(), ref.WorkerID); err != nil {
				t.Fatalf("RevokeWorker: %v", err)
			}
			if !stub.hasDeadline {
				t.Fatal("broker.Revoke ctx has no deadline")
			}
			if left := time.Until(stub.deadline); left > tt.want || left < tt.want-time.Second {
				t.Errorf("broker.Revoke deadline is %s away, want about %s", left, tt.want)
			}
		})
	}
}
