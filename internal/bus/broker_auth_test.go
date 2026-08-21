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
