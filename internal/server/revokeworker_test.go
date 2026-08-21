// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// Unit tests for [Server.RevokeWorker] — the method that turns DELETE
// /api/v1/workers/{id}/credential into a synchronous disconnect, as opposed
// to "sqi-server worker revoke", which only ever writes the store from a
// separate process holding no broker handle.
//
// These exercise RevokeWorker directly against a *Server built by hand
// (store + a real embedded broker, no HTTP layer, no scheduler) so the
// store-write-then-reload ordering and its failure mode are pinned at the
// unit level. The full HTTP-to-disconnect-to-reclaim path is covered by
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

// startTestBroker boots a real embedded broker with authentication enabled
// and enrolled with creds, and registers cleanup.
func startTestBroker(t *testing.T, creds []bus.WorkerCredentialRef) *bus.Broker {
	t.Helper()
	b := bus.New(bus.BrokerConfig{
		Addr:       freeLoopbackAddr(t),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
		Auth:       bus.BrokerAuthConfig{Enabled: true, Credentials: creds},
	}, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startTestBroker: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// enrolledCredential generates a fresh nkey, seeds a matching
// [store.WorkerCredential] row in st, and returns the [bus.WorkerCredentialRef]
// and raw seed needed to connect as that worker.
func enrolledCredential(t *testing.T, st store.Store, workerID string) (bus.WorkerCredentialRef, []byte) {
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
	return bus.WorkerCredentialRef{WorkerID: workerID, PublicKey: pub}, seed
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
func connectAsWorker(t *testing.T, b *bus.Broker, seed []byte, pub string) (*nats.Conn, <-chan struct{}) {
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
// after auth was turned off) but must never touch the broker — there is no
// authorized-key set to reload, and s.broker is left nil here specifically
// to prove that: touching it would panic.
func TestRevokeWorker_NATSAuthDisabled_StoreWriteOnly(t *testing.T) {
	st := fake.New()
	ref, _ := enrolledCredential(t, st, "worker-a")

	s := &Server{cfg: Config{NATSAuthEnabled: false}, store: st, logger: testLogger()}

	if err := s.RevokeWorker(context.Background(), ref.WorkerID); err != nil {
		t.Fatalf("RevokeWorker: %v", err)
	}

	if _, err := st.GetActiveWorkerCredentialByWorkerID(context.Background(), ref.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveWorkerCredentialByWorkerID after revoke: %v, want store.ErrNotFound", err)
	}
}

// TestRevokeWorker_UnknownWorker_ReturnsErrNotFoundWithoutTouchingBroker
// proves the store write happens FIRST: with no credential for "ghost" in
// the store, RevokeWorker must fail and return before ever reaching s.broker
// — which is nil here, so touching it would panic — even though
// NATSAuthEnabled is true.
func TestRevokeWorker_UnknownWorker_ReturnsErrNotFoundWithoutTouchingBroker(t *testing.T) {
	st := fake.New()
	s := &Server{cfg: Config{NATSAuthEnabled: true}, store: st, logger: testLogger()}

	err := s.RevokeWorker(context.Background(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RevokeWorker(\"ghost\") = %v, want store.ErrNotFound", err)
	}
}

// TestRevokeWorker_ReloadDisconnectsRevokedWorkerOnly is the core positive
// case: revoking worker A through Server.RevokeWorker disconnects A's live
// broker connection inside the call (nats-server's reloadAuthorization
// re-authorizes every connected client synchronously) and leaves B
// completely unaffected.
func TestRevokeWorker_ReloadDisconnectsRevokedWorkerOnly(t *testing.T) {
	st := fake.New()
	refA, seedA := enrolledCredential(t, st, "worker-a")
	refB, seedB := enrolledCredential(t, st, "worker-b")

	broker := startTestBroker(t, []bus.WorkerCredentialRef{refA, refB})
	s := &Server{cfg: Config{NATSAuthEnabled: true}, store: st, broker: broker, logger: testLogger()}

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

	if _, err := st.GetActiveWorkerCredentialByWorkerID(context.Background(), refA.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A's credential still active after revoke: %v, want store.ErrNotFound", err)
	}
	if _, err := st.GetActiveWorkerCredentialByWorkerID(context.Background(), refB.WorkerID); err != nil {
		t.Errorf("B's credential was disturbed by A's revocation: %v", err)
	}
}

// TestRevokeWorker_ReloadFailure_StoreStaysRevoked pins the defensible
// outcome of a reload failure after a successful store write: the credential
// row stays revoked (the store is never rolled back) and the failure is
// still surfaced to the caller, since the synchronous disconnect this method
// exists to provide did not actually happen. A shut-down broker stands in
// for "the reload call itself failed" — ReloadCredentials returns "broker
// not started" once Shutdown has run, which is the same shape of failure as
// any other ReloadOptions error from the caller's point of view.
func TestRevokeWorker_ReloadFailure_StoreStaysRevoked(t *testing.T) {
	st := fake.New()
	ref, _ := enrolledCredential(t, st, "worker-a")

	broker := startTestBroker(t, []bus.WorkerCredentialRef{ref})
	broker.Shutdown() // torn down before RevokeWorker runs

	s := &Server{cfg: Config{NATSAuthEnabled: true}, store: st, broker: broker, logger: testLogger()}

	err := s.RevokeWorker(context.Background(), ref.WorkerID)
	if err == nil {
		t.Fatal("RevokeWorker: want an error when the broker reload fails, got nil")
	}

	// The store write must stand regardless: rolling it back on a reload
	// failure would leave a credential the operator explicitly revoked
	// silently trusted again, which is worse than the reload simply not
	// having taken effect yet.
	if _, err := st.GetActiveWorkerCredentialByWorkerID(context.Background(), ref.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("credential still active after a reload failure: %v, want store.ErrNotFound", err)
	}
}

// ── An unserialized read-then-reload span loses an update ──────────────────
//
// An unlocked "read the active credential set, then call ReloadCredentials"
// sequence is vulnerable to a lost update. Two concurrent revocations of
// DIFFERENT workers can interleave like this:
//
//  1. revoke(A) commits its store write.
//  2. revoke(A) reads the active set — B is still active, so the set
//     contains B.
//  3. revoke(B) commits its store write.
//  4. revoke(B) reads the active set — correctly excludes A and B — and
//     reloads.
//  5. revoke(A)'s reload, built from the STALE step-2 read, applies LAST
//     and reintroduces B into the broker's trusted key set.
//
// raceMarkerKey and blockingListStore below reproduce this deterministically
// instead of hoping many iterations happen to hit the right interleaving:
// A's read is captured, then paused (holding s.brokerReloadMu) until the
// test explicitly releases it — by which point B's own revoke has fully
// run. This pins the exact scenario s.brokerReloadMu closes, not just "some
// race somewhere."

// raceMarkerKey tags a context so blockingListStore knows which caller's
// ListActiveWorkerCredentials call to pause.
type raceMarkerKey struct{}

// blockingListStore wraps a store.Store and, only for the call whose
// context carries blockFor, pauses AFTER reading the real result (so the
// caller holds a real, but soon-to-be-stale, snapshot) until release is
// closed. entered is closed the moment the pause begins, so the test knows
// the blocked call has already captured its snapshot before letting the
// other revoke proceed.
type blockingListStore struct {
	store.Store

	blockFor string
	entered  chan struct{}
	release  chan struct{}
}

func (s *blockingListStore) ListActiveWorkerCredentials(ctx context.Context) ([]store.WorkerCredential, error) {
	creds, err := s.Store.ListActiveWorkerCredentials(ctx)
	if v, ok := ctx.Value(raceMarkerKey{}).(string); ok && v == s.blockFor {
		close(s.entered)
		<-s.release
	}
	return creds, err
}

// TestRevokeWorker_ConcurrentRevocationsOfDifferentWorkers_BothStayRevoked
// deterministically forces the interleaving described above and asserts the
// broker ends up trusting NEITHER worker — not just that the store shows
// both revoked (the store was never the vulnerable part; the broker's
// authorized-key set was).
func TestRevokeWorker_ConcurrentRevocationsOfDifferentWorkers_BothStayRevoked(t *testing.T) {
	st := fake.New()
	refA, seedA := enrolledCredential(t, st, "worker-a")
	refB, seedB := enrolledCredential(t, st, "worker-b")

	broker := startTestBroker(t, []bus.WorkerCredentialRef{refA, refB})

	wrapped := &blockingListStore{
		Store:    st,
		blockFor: "A",
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	s := &Server{cfg: Config{NATSAuthEnabled: true}, store: wrapped, broker: broker, logger: testLogger()}

	ctxA := context.WithValue(context.Background(), raceMarkerKey{}, "A")
	ctxB := context.WithValue(context.Background(), raceMarkerKey{}, "B") // never matches blockFor; B is not paused

	var wg sync.WaitGroup
	var errA, errB error
	wg.Go(func() {
		errA = s.RevokeWorker(ctxA, refA.WorkerID)
	})

	// Wait until A has committed its store write, read the (still-stale,
	// B-included) active set, and is now paused holding that snapshot.
	<-wrapped.entered

	// Run B's revoke to completion while A is paused. RevokeWorker's own
	// call blocks trying to acquire s.brokerReloadMu (A is still inside the
	// critical section) until A finishes — so this goroutine only returns
	// after A's whole RevokeWorker call has completed. An unserialized
	// implementation would instead let B run immediately and finish well
	// before A resumes.
	wg.Go(func() {
		errB = s.RevokeWorker(ctxB, refB.WorkerID)
	})

	// Give B's goroutine a moment to either finish (unserialized) or block
	// on the mutex (serialized) before releasing A — the property under
	// test does not depend on this sleep's exact duration, only that B has
	// had the chance to run to the point it would reach if it were going to.
	time.Sleep(50 * time.Millisecond)
	close(wrapped.release)

	wg.Wait()

	if errA != nil {
		t.Errorf("RevokeWorker(A): %v", errA)
	}
	if errB != nil {
		t.Errorf("RevokeWorker(B): %v", errB)
	}

	// Both must be gone from the store...
	if _, err := st.GetActiveWorkerCredentialByWorkerID(context.Background(), refA.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("worker A still active in the store: %v, want store.ErrNotFound", err)
	}
	if _, err := st.GetActiveWorkerCredentialByWorkerID(context.Background(), refB.WorkerID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("worker B still active in the store: %v, want store.ErrNotFound", err)
	}

	// ...and, the property this test exists to pin, neither can connect to
	// the broker: a fresh connection attempt with either's key must be
	// refused. The defect this fixes would let B's stale reintroduction
	// succeed here even though the store already shows it revoked.
	if nc, err := nats.Connect(broker.ClientURL(), nkeyOption(t, seedA, refA.PublicKey), nats.NoReconnect()); err == nil {
		nc.Close()
		t.Error("worker A connected to the broker after concurrent revocation — its credential was reintroduced")
	}
	if nc, err := nats.Connect(broker.ClientURL(), nkeyOption(t, seedB, refB.PublicKey), nats.NoReconnect()); err == nil {
		nc.Close()
		t.Error("worker B connected to the broker after concurrent revocation — its credential was reintroduced by a stale reload")
	}
}
