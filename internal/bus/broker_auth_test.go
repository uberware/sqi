// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

// startBrokerAuth boots an embedded broker configured with auth, on a temp
// JetStream dir and an OS-assigned loopback port, waits for it to be ready,
// and registers cleanup.
func startBrokerAuth(t *testing.T, auth BrokerAuthConfig) *Broker {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	cfg := BrokerConfig{
		Addr:       net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
		Auth:       auth,
	}
	b := New(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startBrokerAuth: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// enrolledWorker generates a fresh nkey and returns a WorkerCredentialRef for
// it, along with the raw seed needed to sign connection challenges.
func enrolledWorker(t *testing.T, workerID string) (WorkerCredentialRef, []byte) {
	t.Helper()
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("enrolledWorker: GenerateSeed: %v", err)
	}
	return WorkerCredentialRef{WorkerID: workerID, PublicKey: pub}, seed
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

func TestBrokerAuth(t *testing.T) {
	t.Run("auth disabled accepts an anonymous connection", func(t *testing.T) {
		b := startBrokerAuth(t, BrokerAuthConfig{Enabled: false})
		nc, err := nats.Connect(b.ClientURL())
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer nc.Close()
	})

	t.Run("auth enabled refuses an anonymous connection", func(t *testing.T) {
		b := startBrokerAuth(t, BrokerAuthConfig{Enabled: true})
		if _, err := nats.Connect(b.ClientURL()); err == nil {
			t.Fatal("Connect: want error for anonymous connection, got nil")
		}
	})

	t.Run("auth enabled accepts an enrolled nkey", func(t *testing.T) {
		ref, seed := enrolledWorker(t, "worker-a")
		b := startBrokerAuth(t, BrokerAuthConfig{
			Enabled:     true,
			Credentials: []WorkerCredentialRef{ref},
		})
		nc, err := nats.Connect(b.ClientURL(), nkeyOption(t, seed, ref.PublicKey))
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer nc.Close()
	})

	t.Run("auth enabled refuses an unenrolled nkey", func(t *testing.T) {
		ref, _ := enrolledWorker(t, "worker-a")
		b := startBrokerAuth(t, BrokerAuthConfig{
			Enabled:     true,
			Credentials: []WorkerCredentialRef{ref},
		})

		// A freshly generated keypair that was never passed in Credentials.
		strangerSeed, strangerPub, err := brokerauth.GenerateSeed()
		if err != nil {
			t.Fatalf("GenerateSeed: %v", err)
		}
		if _, err := nats.Connect(b.ClientURL(), nkeyOption(t, strangerSeed, strangerPub)); err == nil {
			t.Fatal("Connect: want error for unenrolled nkey, got nil")
		}
	})

	t.Run("an enrolled worker cannot subscribe to another worker's traffic", func(t *testing.T) {
		ref, seed := enrolledWorker(t, "worker-a")
		b := startBrokerAuth(t, BrokerAuthConfig{
			Enabled:     true,
			Credentials: []WorkerCredentialRef{ref},
		})
		nc, err := nats.Connect(
			b.ClientURL(),
			nkeyOption(t, seed, ref.PublicKey),
			nats.PermissionErrOnSubscribe(true),
		)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer nc.Close()

		sub, err := nc.SubscribeSync("task.status.>")
		if err != nil {
			t.Fatalf("SubscribeSync: %v", err)
		}
		if err := nc.Flush(); err != nil {
			// A flush error already demonstrates the permission violation.
			if !errors.Is(err, nats.ErrPermissionViolation) {
				t.Fatalf("Flush: unexpected error: %v", err)
			}
			return
		}

		if _, err := sub.NextMsg(2 * time.Second); err == nil {
			t.Fatal("NextMsg: want a permissions violation, got nil error")
		} else if !errors.Is(err, nats.ErrPermissionViolation) {
			t.Fatalf("NextMsg: want permissions violation, got: %v", err)
		}
	})
}

func TestBrokerReloadCredentials(t *testing.T) {
	t.Run("enrolling a second worker does not disturb the first", func(t *testing.T) {
		refA, seedA := enrolledWorker(t, "worker-a")
		b := startBrokerAuth(t, BrokerAuthConfig{
			Enabled:     true,
			Credentials: []WorkerCredentialRef{refA},
		})

		ncA, err := nats.Connect(b.ClientURL(), nkeyOption(t, seedA, refA.PublicKey))
		if err != nil {
			t.Fatalf("connect as A: %v", err)
		}
		defer ncA.Close()

		refB, seedB := enrolledWorker(t, "worker-b")
		if err := b.ReloadCredentials([]WorkerCredentialRef{refA, refB}); err != nil {
			t.Fatalf("ReloadCredentials: %v", err)
		}

		if err := ncA.Flush(); err != nil {
			t.Fatalf("A's connection unusable after reload: %v", err)
		}
		if !ncA.IsConnected() {
			t.Fatal("A's connection was disconnected by an unrelated enrollment")
		}

		ncB, err := nats.Connect(b.ClientURL(), nkeyOption(t, seedB, refB.PublicKey))
		if err != nil {
			t.Fatalf("connect as newly enrolled B: %v", err)
		}
		defer ncB.Close()
	})

	t.Run("revoking a worker disconnects it in the reload call", func(t *testing.T) {
		refA, seedA := enrolledWorker(t, "worker-a")
		refB, seedB := enrolledWorker(t, "worker-b")
		b := startBrokerAuth(t, BrokerAuthConfig{
			Enabled:     true,
			Credentials: []WorkerCredentialRef{refA, refB},
		})

		// A does not reconnect: the point of this test is to observe the
		// broker's own revocation promptly, not nats.go's reconnect/backoff
		// behavior (which would otherwise mask a slow revocation behind a
		// retry that happens to succeed once A is no longer welcome).
		closedCh := make(chan struct{})
		ncA, err := nats.Connect(
			b.ClientURL(),
			nkeyOption(t, seedA, refA.PublicKey),
			nats.NoReconnect(),
			nats.ClosedHandler(func(*nats.Conn) { close(closedCh) }),
		)
		if err != nil {
			t.Fatalf("connect as A: %v", err)
		}
		defer ncA.Close()

		ncB, err := nats.Connect(b.ClientURL(), nkeyOption(t, seedB, refB.PublicKey))
		if err != nil {
			t.Fatalf("connect as B: %v", err)
		}
		defer ncB.Close()

		// Revoke A by reloading with only B enrolled. Revocation is
		// synchronous: nats-server re-authorizes every connected client
		// inside ReloadOptions, so A is disconnected before this call
		// returns — the short deadline below is to tolerate scheduling
		// jitter in observing that, not because the disconnect itself is
		// expected to be delayed.
		if err := b.ReloadCredentials([]WorkerCredentialRef{refB}); err != nil {
			t.Fatalf("ReloadCredentials: %v", err)
		}

		select {
		case <-closedCh:
		case <-time.After(2 * time.Second):
			t.Fatal("A's connection was not closed after revocation")
		}

		if err := ncB.Flush(); err != nil {
			t.Fatalf("B's connection unusable after A's revocation: %v", err)
		}
		if !ncB.IsConnected() {
			t.Fatal("B's connection was disconnected by an unrelated revocation")
		}
	})
}

// TestBrokerReloadCredentials_ConcurrentWithShutdown exercises the hazard
// that ReloadCredentials being reachable from an HTTP handler goroutine
// (worker credential revocation) activates: nothing previously called
// ReloadCredentials outside a test, so it never ran concurrently with
// Shutdown, which nils ns/nc/bootOpts/serverSeed/serverPub. Run under
// -race, this fails without Broker.mu guarding those fields — either as a
// reported data race or as a nil-pointer panic when Shutdown wins the race
// and ReloadCredentials dereferences a nil *natsserver.Server.
//
// This does not assert anything about which of the two operations "wins" —
// either outcome (the reload completing before shutdown tears the server
// down, or ReloadCredentials observing "broker not started" because
// Shutdown got there first) is a correct, safe result. The property under
// test is only that neither goroutine corrupts Broker's own state or
// crashes the process while racing the other.
func TestBrokerReloadCredentials_ConcurrentWithShutdown(t *testing.T) {
	ref, _ := enrolledWorker(t, "worker-a")
	b := startBrokerAuth(t, BrokerAuthConfig{
		Enabled:     true,
		Credentials: []WorkerCredentialRef{ref},
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for range 20 {
			// Any outcome is acceptable here; "bus: broker not started" is
			// expected once Shutdown has run. Only a panic or a race-detector
			// report would fail this test.
			_ = b.ReloadCredentials([]WorkerCredentialRef{ref}) //nolint:errcheck // outcome is intentionally unchecked; see comment above
		}
	}()

	go func() {
		defer wg.Done()
		b.Shutdown()
	}()

	wg.Wait()

	// A second Shutdown (this one via t.Cleanup) must still be a safe no-op
	// after the concurrent one above already ran.
}

// TestBrokerAuth_WorkerCannotSeeAnotherWorkersLeaseReply pins the property
// the per-worker reply-inbox prefix exists for: an enrolled worker must not
// be able to read another worker's lease reply, which carries that worker's
// whole assignment batch.
//
// The reply travels over core-NATS request/reply (msg.Respond, lease.go), so
// its subject is the requester's own reply inbox and nothing else guards it
// but the subscribe permission. With a process-global "_INBOX" prefix and an
// "_INBOX.>" grant, any enrolled worker could subscribe to every other
// client's inbox on the broker and collect the OnRun command lines, embedded
// files, parameters, environment and isolation account of work it never
// leased. Each worker therefore gets its own prefix
// ([brokerauth.InboxPrefix]) and is granted only that subtree.
func TestBrokerAuth_WorkerCannotSeeAnotherWorkersLeaseReply(t *testing.T) {
	refA, seedA := enrolledWorker(t, "worker-a")
	refB, seedB := enrolledWorker(t, "worker-b")
	b := startBrokerAuth(t, BrokerAuthConfig{
		Enabled:     true,
		Credentials: []WorkerCredentialRef{refA, refB},
	})

	// Stands in for an AssignMsg batch: whatever A can read here, it could
	// read of a real assignment.
	const assignment = "SECRET-ASSIGNMENT-PAYLOAD"

	// The server side of the lease, on the server's own credential and the
	// real SubscribeLease path.
	srv, err := b.NewClient()
	if err != nil {
		t.Fatalf("broker NewClient: %v", err)
	}
	defer srv.Close()
	leaseSub, err := srv.SubscribeLease(func(string, string, []byte) []byte { return []byte(assignment) })
	if err != nil {
		t.Fatalf("SubscribeLease: %v", err)
	}
	defer leaseSub.Unsubscribe() //nolint:errcheck // best-effort test cleanup

	// Worker A, the eavesdropper, camps on both the process-global inbox
	// subtree nats.go uses by default and B's own per-worker one. Neither
	// subscription may be granted, so the Flush is expected to fail; the
	// subscriptions are kept regardless so the assertion at the end holds
	// even if a future nats.go stops reporting the violation here.
	ncA, err := nats.Connect(b.ClientURL(), nkeyOption(t, seedA, refA.PublicKey))
	if err != nil {
		t.Fatalf("connect as A: %v", err)
	}
	defer ncA.Close()
	var spies []*nats.Subscription
	for _, subject := range []string{"_INBOX.>", brokerauth.InboxPrefix(refB.WorkerID) + ".>"} {
		spy, err := ncA.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("A SubscribeSync %q: %v", subject, err)
		}
		spies = append(spies, spy)
	}
	if err := ncA.Flush(); err != nil && !errors.Is(err, nats.ErrPermissionViolation) {
		t.Fatalf("A Flush: unexpected error: %v", err)
	}

	// Worker B leases work over its own connection, with the per-worker
	// inbox prefix internal/worker/natsclient gives every real worker.
	clientB, err := NewClient(b.ClientURL(), slog.New(slog.DiscardHandler),
		nkeyOption(t, seedB, refB.PublicKey),
		nats.CustomInboxPrefix(brokerauth.InboxPrefix(refB.WorkerID)))
	if err != nil {
		t.Fatalf("connect as B: %v", err)
	}
	defer clientB.Close()

	reply, err := clientB.RequestLease(context.Background(), refB.WorkerID, "queue-1", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("B RequestLease: %v", err)
	}
	if string(reply) != assignment {
		t.Fatalf("B's lease reply = %q, want %q — the test cannot prove anything if B never got the payload", reply, assignment)
	}

	for i, spy := range spies {
		if msg, err := spy.NextMsg(time.Second); err == nil {
			t.Fatalf("worker A read worker B's lease reply on spy %d (%s): %q", i, spy.Subject, msg.Data)
		} else if !errors.Is(err, nats.ErrTimeout) && !errors.Is(err, nats.ErrPermissionViolation) {
			t.Fatalf("spy %d (%s): unexpected error: %v", i, spy.Subject, err)
		}
	}
}

// TestBrokerAuth_SkipsCredentialWithAnInvalidWorkerID is the last line of
// defense behind the two enrollment boundaries, which reject an invalid
// worker ID before it is ever stored (internal/api's enroll handler and
// sqi-server's worker enroll command).
//
// The worker ID becomes a NATS subject PATTERN in this credential's grants,
// so a stored "*" would mint one credential allowed to publish concrete
// subjects belonging to any worker, and a stored ">" would put the
// malformed "task.status.>.*" into Options.Nkeys — which nats-server may
// reject, failing every later ReloadOptions (revocation permanently 500ing)
// or refusing to boot at all. A row from a hand-edited database or an older
// binary must not be able to do either, so buildNkeys drops it and logs.
func TestBrokerAuth_SkipsCredentialWithAnInvalidWorkerID(t *testing.T) {
	good, goodSeed := enrolledWorker(t, "worker-a")

	for _, workerID := range []string{"*", ">", "a.b", "a b", ""} {
		t.Run("worker id "+workerID, func(t *testing.T) {
			bad, badSeed := enrolledWorker(t, workerID)
			b := startBrokerAuth(t, BrokerAuthConfig{
				Enabled:     true,
				Credentials: []WorkerCredentialRef{good, bad},
			})

			// The valid credential is unaffected — the broker booted and
			// still authorizes it.
			nc, err := nats.Connect(b.ClientURL(), nkeyOption(t, goodSeed, good.PublicKey))
			if err != nil {
				t.Fatalf("the valid credential was refused after an invalid one was skipped: %v", err)
			}
			defer nc.Close()

			// The invalid one was never installed, so its key is unknown.
			if bnc, err := nats.Connect(b.ClientURL(), nkeyOption(t, badSeed, bad.PublicKey)); err == nil {
				bnc.Close()
				t.Errorf("a credential with worker id %q was installed; it must be skipped", workerID)
			}

			// And a reload over the same set still succeeds rather than
			// failing on a malformed subject pattern.
			if err := b.ReloadCredentials([]WorkerCredentialRef{good, bad}); err != nil {
				t.Errorf("ReloadCredentials with a skipped credential failed: %v", err)
			}
		})
	}
}

// TestBrokerAuth_SkipsCredentialWithAnInvalidPublicKey is the public-key
// counterpart of TestBrokerAuth_SkipsCredentialWithAnInvalidWorkerID.
//
// Both write paths validate the public key with brokerauth.ValidatePublicKey
// before a credential is ever stored, so reaching buildNkeys with a
// malformed one needs a hand-edited database or an older binary — but
// without this guard the consequence is worse than a skipped worker: an
// invalid key handed straight to natsserver.NkeyUser makes
// natsserver.NewServer reject the options outright, which fails Start (and
// every later ReloadCredentials) for the WHOLE broker, not just the one bad
// row. One bad row must cost one worker, never the farm.
func TestBrokerAuth_SkipsCredentialWithAnInvalidPublicKey(t *testing.T) {
	good, goodSeed := enrolledWorker(t, "worker-a")
	bad := WorkerCredentialRef{WorkerID: "worker-b", PublicKey: "not-an-nkey"}

	b := startBrokerAuth(t, BrokerAuthConfig{
		Enabled:     true,
		Credentials: []WorkerCredentialRef{good, bad},
	})

	// The valid credential is unaffected — the broker booted (it would not
	// have, with the malformed key installed) and still authorizes it.
	nc, err := nats.Connect(b.ClientURL(), nkeyOption(t, goodSeed, good.PublicKey))
	if err != nil {
		t.Fatalf("the valid credential was refused after an invalid one was skipped: %v", err)
	}
	defer nc.Close()

	// And a reload over the same set still succeeds rather than failing on
	// the malformed key.
	if err := b.ReloadCredentials([]WorkerCredentialRef{good, bad}); err != nil {
		t.Errorf("ReloadCredentials with a skipped credential failed: %v", err)
	}
}
