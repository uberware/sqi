// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
)

// credentialMap is a [CredentialLookup] over an in-memory map, standing in
// for the store's active credentials. fail makes every lookup return an
// error, as a store outage would.
type credentialMap struct {
	mu    sync.Mutex
	byKey map[string]string
	err   error
}

func newCredentialMap() *credentialMap {
	return &credentialMap{byKey: map[string]string{}}
}

// enroll generates a key for workerID, records it as active and returns the
// seed and public key the worker connects with.
func (m *credentialMap) enroll(t *testing.T, workerID string) (seed []byte, pub string) {
	t.Helper()
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	m.set(pub, workerID)
	return seed, pub
}

func (m *credentialMap) set(pub, workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byKey[pub] = workerID
}

func (m *credentialMap) remove(pub string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byKey, pub)
}

func (m *credentialMap) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *credentialMap) lookup(_ context.Context, pub string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return "", m.err
	}
	id, ok := m.byKey[pub]
	if !ok {
		return "", ErrUnknownCredential
	}
	return id, nil
}

// syncBuffer is a bytes.Buffer safe to read while the broker logs into it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startAuthEmbedded boots an embedded broker with auth on, answering connects
// from creds.
func startAuthEmbedded(t *testing.T, creds *credentialMap, mutate ...func(*EmbeddedConfig)) *Embedded {
	t.Helper()
	cfg := EmbeddedConfig{Auth: AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: creds.lookup}}
	for _, m := range mutate {
		m(&cfg)
	}
	return startEmbedded(t, cfg)
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

// connzForUser returns the open connections the broker has for user in SQI.
func connzForUser(t *testing.T, b *Embedded, user string) []*natsserver.ConnInfo {
	t.Helper()
	b.mu.Lock()
	ns := b.ns
	b.mu.Unlock()
	cz, err := ns.Connz(&natsserver.ConnzOptions{Username: true, User: user, Account: DefaultAccount})
	if err != nil {
		t.Fatalf("Connz: %v", err)
	}
	return cz.Conns
}

// concreteSubject replaces each wildcard token of an allow pattern with a
// literal, giving a subject the pattern covers.
func concreteSubject(pattern string) string {
	tokens := strings.Split(pattern, ".")
	for i, tok := range tokens {
		if tok == "*" || tok == ">" {
			tokens[i] = "x"
		}
	}
	return strings.Join(tokens, ".")
}

// TestEmbedded_EnrolledWorkerGetsExactlyItsGrants checks that the user JWT
// the callout issues carries [brokerauth.WorkerPermissions], through the
// forgery checks shared with the external broker ([assertWorkerGrants]).
func TestEmbedded_EnrolledWorkerGetsExactlyItsGrants(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds)

	nc, err := nats.Connect(b.ClientURL(), nkeyOption(t, seed, pub))
	if err != nil {
		t.Fatalf("connect as worker-a: %v", err)
	}
	defer nc.Close()
	assertWorkerGrants(t, nc, "worker-a")
}

// TestEmbedded_WorkerCannotSeeAnotherWorkersLeaseReply pins the property the
// per-worker reply-inbox prefix exists for: an enrolled worker must not be
// able to read another worker's lease reply, which carries that worker's
// whole assignment batch.
//
// The reply travels over core-NATS request/reply (msg.Respond, lease.go), so
// its subject is the requester's own reply inbox and nothing else guards it
// but the subscribe permission. Each worker gets its own prefix
// ([brokerauth.InboxPrefix]) and is granted only that subtree.
func TestEmbedded_WorkerCannotSeeAnotherWorkersLeaseReply(t *testing.T) {
	creds := newCredentialMap()
	seedA, pubA := creds.enroll(t, "worker-a")
	seedB, pubB := creds.enroll(t, "worker-b")
	b := startAuthEmbedded(t, creds)

	// Stands in for an AssignMsg batch: whatever A can read here, it could
	// read of a real assignment.
	const assignment = "SECRET-ASSIGNMENT-PAYLOAD"

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

	// Worker A camps on both the process-global inbox subtree and B's own
	// per-worker one. Neither may be granted.
	ncA, err := nats.Connect(b.ClientURL(), nkeyOption(t, seedA, pubA))
	if err != nil {
		t.Fatalf("connect as A: %v", err)
	}
	defer ncA.Close()
	var spies []*nats.Subscription
	for _, subject := range []string{"_INBOX.>", brokerauth.InboxPrefix("worker-b") + ".>"} {
		spy, err := ncA.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("A SubscribeSync %q: %v", subject, err)
		}
		spies = append(spies, spy)
	}
	if err := ncA.Flush(); err != nil && !errors.Is(err, nats.ErrPermissionViolation) {
		t.Fatalf("A Flush: unexpected error: %v", err)
	}

	clientB, err := NewClient(b.ClientURL(), slog.New(slog.DiscardHandler),
		nkeyOption(t, seedB, pubB),
		nats.CustomInboxPrefix(brokerauth.InboxPrefix("worker-b")))
	if err != nil {
		t.Fatalf("connect as B: %v", err)
	}
	defer clientB.Close()

	reply, err := clientB.RequestLease(context.Background(), "worker-b", "queue-1", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("B RequestLease: %v", err)
	}
	if string(reply) != assignment {
		t.Fatalf("B's lease reply = %q, want %q", reply, assignment)
	}

	for i, spy := range spies {
		if msg, err := spy.NextMsg(time.Second); err == nil {
			t.Fatalf("worker A read worker B's lease reply on spy %d (%s): %q", i, spy.Subject, msg.Data)
		} else if !errors.Is(err, nats.ErrTimeout) && !errors.Is(err, nats.ErrPermissionViolation) {
			t.Fatalf("spy %d (%s): unexpected error: %v", i, spy.Subject, err)
		}
	}
}

func TestEmbedded_UnknownAndRevokedKeysRefused(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds)

	t.Run("anonymous", func(t *testing.T) {
		if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect()); err == nil {
			nc.Close()
			t.Fatal("an anonymous connection was accepted with auth on")
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		strangerSeed, strangerPub, err := brokerauth.GenerateSeed()
		if err != nil {
			t.Fatal(err)
		}
		nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, strangerSeed, strangerPub))
		if err == nil {
			nc.Close()
			t.Fatal("a key that was never enrolled was accepted")
		}
		if !errors.Is(err, nats.ErrAuthorization) {
			t.Errorf("error = %v, want nats.ErrAuthorization", err)
		}
	})

	t.Run("a key signed by someone else", func(t *testing.T) {
		otherSeed, _, err := brokerauth.GenerateSeed()
		if err != nil {
			t.Fatal(err)
		}
		// Presents the enrolled public key but signs with a different seed.
		if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, otherSeed, pub)); err == nil {
			nc.Close()
			t.Fatal("a connection that does not hold the enrolled key's seed was accepted")
		}
	})

	t.Run("revoked key", func(t *testing.T) {
		nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub))
		if err != nil {
			t.Fatalf("the enrolled key was refused before revocation: %v", err)
		}
		nc.Close()

		creds.remove(pub)
		if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub)); err == nil {
			nc.Close()
			t.Fatal("a revoked key was accepted")
		}
	})
}

// TestEmbedded_EnrolmentNeedsNoBrokerCall pins that the broker reads the
// credential at connect time: a key enrolled after Start connects with no
// call into the broker.
func TestEmbedded_EnrolmentNeedsNoBrokerCall(t *testing.T) {
	creds := newCredentialMap()
	b := startAuthEmbedded(t, creds)

	seed, pub := creds.enroll(t, "worker-late")
	nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub))
	if err != nil {
		t.Fatalf("a worker enrolled after Start could not connect: %v", err)
	}
	nc.Close()
}

// TestEmbedded_RevokeDisconnectsBeforeReturning pins H1's synchronous
// revocation on the callout mechanism. When Revoke returns, the broker holds
// no connection for the worker and has stopped routing to it; the worker's
// own client notices the closed socket a moment later. Its reconnect goes
// through the callout and is refused because its key is gone.
func TestEmbedded_RevokeDisconnectsBeforeReturning(t *testing.T) {
	creds := newCredentialMap()
	seedA, pubA := creds.enroll(t, "worker-a")
	seedB, pubB := creds.enroll(t, "worker-b")
	b := startAuthEmbedded(t, creds)

	connect := func(seed []byte, pub string) (*nats.Conn, <-chan struct{}) {
		t.Helper()
		closed := make(chan struct{})
		nc, err := nats.Connect(b.ClientURL(), nkeyOption(t, seed, pub),
			nats.MaxReconnects(-1), nats.ReconnectWait(10*time.Millisecond),
			nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		return nc, closed
	}
	ncA, closedA := connect(seedA, pubA)
	ncB, closedB := connect(seedB, pubB)

	if got := len(connzForUser(t, b, "worker-a")); got != 1 {
		t.Fatalf("broker sees %d connections for worker-a before revoke, want 1", got)
	}

	// The store write comes first in production; then the broker call.
	creds.remove(pubA)
	if err := b.Revoke(context.Background(), "worker-a"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// At the instant Revoke returns: A is gone from the broker, B is not.
	if got := len(connzForUser(t, b, "worker-a")); got != 0 {
		t.Fatalf("broker still holds %d connections for worker-a after Revoke returned", got)
	}
	if got := len(connzForUser(t, b, "worker-b")); got != 1 {
		t.Fatalf("broker holds %d connections for worker-b after revoking worker-a, want 1", got)
	}

	// A's client sees the close, and its reconnect is refused until nats.go
	// gives up on the repeated authorization error.
	select {
	case <-closedA:
	case <-time.After(5 * time.Second):
		t.Fatalf("worker-a's connection is %v 5s after revocation; want it closed", ncA.Status())
	}
	if err := ncA.LastError(); !errors.Is(err, nats.ErrAuthorization) {
		t.Errorf("worker-a closed with %v, want nats.ErrAuthorization from the refused reconnect", err)
	}

	select {
	case <-closedB:
		t.Fatal("worker-b's connection was closed by an unrelated revocation")
	default:
	}
	if err := ncB.Flush(); err != nil {
		t.Fatalf("worker-b's connection unusable after worker-a's revocation: %v", err)
	}
	if ncB.Status() != nats.CONNECTED {
		t.Fatalf("worker-b status = %v, want CONNECTED", ncB.Status())
	}
}

func TestEmbedded_RevokeRejectsEmptyWorkerID(t *testing.T) {
	b := startAuthEmbedded(t, newCredentialMap())
	c := newClient(t, b)

	// An empty user filter matches every connection in the account, sqi's
	// own included.
	if err := b.Revoke(context.Background(), ""); err == nil {
		t.Fatal("Revoke(\"\") succeeded; it must refuse rather than match every connection")
	}
	if err := c.nc.Flush(); err != nil {
		t.Fatalf("sqi's own connection is unusable after Revoke(\"\"): %v", err)
	}
	if err := b.Check(context.Background()); err != nil {
		t.Fatalf("Check after Revoke(\"\"): %v", err)
	}
}

func TestEmbedded_RevokeBeforeStartFails(t *testing.T) {
	b := NewEmbedded(EmbeddedConfig{Auth: AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: newCredentialMap().lookup}},
		slog.New(slog.DiscardHandler))
	if err := b.Revoke(context.Background(), "worker-a"); err == nil {
		t.Fatal("Revoke on a broker that was never started succeeded")
	}
}

// TestEmbedded_RevokeConcurrentWithShutdown runs Revoke from another
// goroutine while the broker shuts down, as an HTTP revocation racing the
// process exit would. Either outcome is correct; only a race report or a
// panic fails it.
func TestEmbedded_RevokeConcurrentWithShutdown(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds)

	// A live connection, so DisconnectClientByID is what races Shutdown.
	nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub))
	if err != nil {
		t.Fatalf("connect as worker-a: %v", err)
	}
	defer nc.Close()
	creds.remove(pub)

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 20 {
			_ = b.Revoke(context.Background(), "worker-a") //nolint:errcheck // either outcome is correct here
		}
	})
	wg.Go(b.Shutdown)
	wg.Wait()
}

// TestEmbedded_InvalidWorkerIDRowDenied is the last line of defense behind
// the enrolment boundaries: a stored worker ID that is not a single subject
// token would become a wildcard in the worker's grants.
func TestEmbedded_InvalidWorkerIDRowDenied(t *testing.T) {
	for _, workerID := range []string{"*", ">", "a.b", "a b", ""} {
		t.Run(fmt.Sprintf("worker id %q", workerID), func(t *testing.T) {
			creds := newCredentialMap()
			seed, pub := creds.enroll(t, workerID)
			logs := &syncBuffer{}
			logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			b := startEmbeddedWithLogger(t, EmbeddedConfig{
				Auth: AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: creds.lookup},
			}, logger)

			if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub)); err == nil {
				nc.Close()
				t.Fatalf("a credential with worker id %q was accepted", workerID)
			}

			found := false
			for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					continue
				}
				msg, ok := rec["msg"].(string)
				if ok && rec["level"] == "ERROR" && strings.Contains(msg, "not a valid NATS subject token") {
					found = true
				}
			}
			if !found {
				t.Errorf("no ERROR naming the invalid worker id was logged:\n%s", logs.String())
			}
		})
	}
}

// TestEmbedded_ExpiryReconnectsThreeTimes checks that a user JWT's expiry
// costs a reconnect and nothing more: the worker goes back through the
// callout and keeps working, expiry after expiry.
func TestEmbedded_ExpiryReconnectsThreeTimes(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds, func(c *EmbeddedConfig) { c.Auth.UserTTL = 2 * time.Second })

	var reconnects, expiries atomic.Int32
	nc, err := nats.Connect(b.ClientURL(), nkeyOption(t, seed, pub),
		nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { reconnects.Add(1) }),
		// nats.go reports the server's expiry error asynchronously; the
		// disconnect that follows carries only the closed socket.
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if errors.Is(err, nats.ErrAuthExpired) {
				expiries.Add(1)
			}
		}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	deadline := time.Now().Add(20 * time.Second)
	for reconnects.Load() < 3 {
		if st := nc.Status(); st == nats.CLOSED {
			t.Fatalf("connection CLOSED after %d reconnects: %v", reconnects.Load(), nc.LastError())
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d reconnects in 20s with a 2s user TTL", reconnects.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if expiries.Load() == 0 {
		t.Error("no disconnect was an authentication expiry; the reconnects had some other cause")
	}
	// The final reconnect may still be completing; it must end connected.
	waitForStatus(t, nc, nats.CONNECTED, 5*time.Second)
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush after three expiries: %v", err)
	}
}

// waitForStatus waits until nc reports want, failing the test if it reports
// CLOSED first or the deadline passes.
func waitForStatus(t *testing.T, nc *nats.Conn, want nats.Status, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		st := nc.Status()
		if st == want {
			return
		}
		if st == nats.CLOSED {
			t.Fatalf("connection CLOSED while waiting for %v: %v", want, nc.LastError())
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection status %v after %s, want %v (last error: %v)", st, within, want, nc.LastError())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEmbedded_AuthOffAnonymousConnectsInSQI pins the default path: with auth
// off, an anonymous client connects, lands in the SQI account and reaches
// sqi's streams there.
func TestEmbedded_AuthOffAnonymousConnectsInSQI(t *testing.T) {
	b := startEmbedded(t, EmbeddedConfig{})

	nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect())
	if err != nil {
		t.Fatalf("anonymous Connect with auth off: %v", err)
	}
	defer nc.Close()

	cid, err := nc.GetClientID()
	if err != nil {
		t.Fatalf("GetClientID: %v", err)
	}
	b.mu.Lock()
	ns := b.ns
	b.mu.Unlock()
	cz, err := ns.Connz(&natsserver.ConnzOptions{CID: cid, Account: DefaultAccount})
	if err != nil {
		t.Fatalf("Connz: %v", err)
	}
	if len(cz.Conns) != 1 {
		t.Fatalf("the anonymous connection is not in account %s", DefaultAccount)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.Publish(ctx, TaskStatusSubject("w-1", "job-1"), []byte("{}")); err != nil {
		t.Fatalf("anonymous JetStream publish to sqi's stream: %v", err)
	}
}

// TestEmbedded_TogglingAuthKeepsStreams pins the reason both modes share one
// account: turning auth on later finds the messages written with it off.
func TestEmbedded_TogglingAuthKeepsStreams(t *testing.T) {
	dataDir := t.TempDir() + "/nats"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	off := startEmbedded(t, EmbeddedConfig{DataDir: dataDir})
	c := newClient(t, off)
	if err := c.PublishTaskStatus(ctx, "w-1", "job-1", []byte("written with auth off")); err != nil {
		t.Fatalf("PublishTaskStatus: %v", err)
	}
	c.Close()
	off.Shutdown()

	on := startAuthEmbedded(t, newCredentialMap(), func(cfg *EmbeddedConfig) { cfg.DataDir = dataDir })
	c2 := newClient(t, on)
	stream, err := c2.js.Stream(ctx, StreamTask)
	if err != nil {
		t.Fatalf("stream %s after enabling auth: %v", StreamTask, err)
	}
	msg, err := stream.GetLastMsgForSubject(ctx, TaskStatusSubject("w-1", "job-1"))
	if err != nil {
		t.Fatalf("the status message written with auth off is gone: %v", err)
	}
	if string(msg.Data) != "written with auth off" {
		t.Fatalf("message = %q", msg.Data)
	}
}

func TestEmbedded_StreamsAreSingleReplicaInSQI(t *testing.T) {
	for _, auth := range []bool{false, true} {
		t.Run(fmt.Sprintf("auth=%v", auth), func(t *testing.T) {
			cfg := EmbeddedConfig{MaxStoreMB: 48}
			if auth {
				cfg.Auth = AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: newCredentialMap().lookup}
			}
			b := startEmbedded(t, cfg)
			c := newClient(t, b)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			for _, def := range streamDefs(1) {
				s, err := c.js.Stream(ctx, def.Name)
				if err != nil {
					t.Fatalf("stream %s: %v", def.Name, err)
				}
				if r := s.CachedInfo().Config.Replicas; r != 1 {
					t.Errorf("stream %s replicas = %d, want 1", def.Name, r)
				}
			}

			b.mu.Lock()
			ns := b.ns
			b.mu.Unlock()
			acc, err := ns.LookupAccount(DefaultAccount)
			if err != nil {
				t.Fatal(err)
			}
			usage := acc.JetStreamUsage()
			if usage.Streams != len(streamDefs(1)) {
				t.Errorf("account %s holds %d streams, want %d", DefaultAccount, usage.Streams, len(streamDefs(1)))
			}
			if want := int64(48) << 20; usage.Limits.MaxStore != want {
				t.Errorf("account %s max store = %d, want %d", DefaultAccount, usage.Limits.MaxStore, want)
			}
			if g := ns.GlobalAccount(); g.JetStreamEnabled() {
				t.Error("JetStream is enabled on the global account; streams must live only in SQI")
			}
		})
	}
}

// TestEmbedded_TLSStillRequiredAfterRevoke checks that revoking a worker
// leaves the listener as it was configured: TLS is still required.
func TestEmbedded_TLSStillRequiredAfterRevoke(t *testing.T) {
	dir, _ := farmCerts(t)
	creds := newCredentialMap()
	seedA, pubA := creds.enroll(t, "worker-a")
	_, pubB := creds.enroll(t, "worker-b")
	b := startAuthEmbedded(t, creds, func(c *EmbeddedConfig) { c.TLS = brokerTLSOf(dir, false) })
	secure := nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: caPool(t, dir)})

	nc, err := nats.Connect(b.ClientURL(), secure, nkeyOption(t, seedA, pubA), nats.NoReconnect())
	if err != nil {
		t.Fatalf("worker-a could not connect over TLS: %v", err)
	}
	nc.Close()

	creds.remove(pubB)
	if err := b.Revoke(context.Background(), "worker-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// The plaintext client carries a VALID credential, so the only reason
	// left to refuse it is TLS.
	plain, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seedA, pubA))
	if err == nil {
		plain.Close()
		t.Fatal("a plaintext client with a valid credential connected after a revocation")
	}
	if !strings.Contains(err.Error(), "secure") && !strings.Contains(err.Error(), "TLS") && !strings.Contains(err.Error(), "tls") {
		t.Errorf("plaintext client was refused for the wrong reason: %v", err)
	}

	nc2, err := nats.Connect(b.ClientURL(), secure, nkeyOption(t, seedA, pubA), nats.NoReconnect())
	if err != nil {
		t.Fatalf("worker-a could not connect over TLS after the revocation: %v", err)
	}
	nc2.Close()
}

// TestEmbedded_StoreOutageThenRecovery covers a credential store that is
// unreachable while a worker connects (a PostgreSQL restart): the connect is
// refused, and once the store is back the same retrying worker connects on
// its own, with no broker restart.
//
// IgnoreAuthErrorAbort is load-bearing. Every refusal reaches nats.go as the
// same "Authorization Violation", and by default nats.go closes a connection
// for good on the second identical authorization error from one server, so
// a client without it gives up during the outage rather than retrying.
func TestEmbedded_StoreOutageThenRecovery(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds)
	creds.fail(errors.New("database is down"))

	nc, err := nats.Connect(b.ClientURL(), nkeyOption(t, seed, pub),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond), nats.IgnoreAuthErrorAbort())
	if err != nil {
		t.Fatalf("Connect with RetryOnFailedConnect: %v", err)
	}
	defer nc.Close()

	// Several attempts are refused while the store is down.
	time.Sleep(time.Second)
	if st := nc.Status(); st == nats.CONNECTED {
		t.Fatal("the worker connected while the credential store was failing")
	} else if st == nats.CLOSED {
		t.Fatalf("the worker gave up during the outage: %v", nc.LastError())
	}

	creds.fail(nil)
	waitForStatus(t, nc, nats.CONNECTED, 5*time.Second)
}

// TestEmbedded_CheckRequiresCalloutConnection pins that /readyz reports the
// broker degraded when the connection that answers callouts is gone: every
// worker connect would be refused.
func TestEmbedded_CheckRequiresCalloutConnection(t *testing.T) {
	b := startAuthEmbedded(t, newCredentialMap())
	if err := b.Check(context.Background()); err != nil {
		t.Fatalf("Check on a healthy broker: %v", err)
	}
	b.mu.Lock()
	callout := b.callout
	b.mu.Unlock()
	callout.Close()
	if err := b.Check(context.Background()); err == nil {
		t.Fatal("Check passed with the callout connection closed")
	}
}

// TestEmbedded_AuthOnRefusesIncompleteConfig pins that a broker with auth on
// but no way to answer a callout fails Start rather than refusing every
// worker at runtime.
func TestEmbedded_AuthOnRefusesIncompleteConfig(t *testing.T) {
	for name, auth := range map[string]AuthConfig{
		"no lookup": {Enabled: true, UserTTL: time.Hour},
		"no ttl":    {Enabled: true, Lookup: newCredentialMap().lookup},
	} {
		t.Run(name, func(t *testing.T) {
			b := NewEmbedded(EmbeddedConfig{
				Addr:    net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
				DataDir: t.TempDir() + "/nats",
				Auth:    auth,
			}, slog.New(slog.DiscardHandler))
			if err := b.Start(context.Background()); err == nil {
				b.Shutdown()
				t.Fatal("Start succeeded with an incomplete auth configuration")
			}
			if b.ClientURL() != "" {
				t.Error("the listener is up after a failed Start")
			}
		})
	}
}

// TestEmbedded_StartWithoutDeadline pins that Start works with a context that
// has no deadline, which is how sqi-server starts the broker.
func TestEmbedded_StartWithoutDeadline(t *testing.T) {
	b := NewEmbedded(EmbeddedConfig{
		Addr:    net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir: t.TempDir() + "/nats",
		Auth:    AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: newCredentialMap().lookup},
	}, slog.New(slog.DiscardHandler))
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start with a context without a deadline: %v", err)
	}
	b.Shutdown()
}

// blockingLookup wraps creds so that the first lookup of key blocks until
// release is closed and then answers workerID, as a lookup that read the
// store just before a revocation committed would. entered closes when that
// lookup starts. Every other lookup goes to creds.
func blockingLookup(creds *credentialMap, key, workerID string) (lookup CredentialLookup, entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	lookup = func(ctx context.Context, k string) (string, error) {
		if k == key && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
			return workerID, nil
		}
		return creds.lookup(ctx, k)
	}
	return lookup, entered, release
}

// waitClosed waits for ch, failing the test after within.
func waitClosed(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("timed out after %s waiting for %s", within, what)
	}
}

// TestEmbedded_RevokeCatchesInFlightAuthorization pins the window a single
// Connz pass leaves open: an authorization whose store read came before the
// revocation, and whose connection is placed only after Revoke's first pass.
// Revoke must still have disconnected it by the time it returns.
func TestEmbedded_RevokeCatchesInFlightAuthorization(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	lookup, entered, release := blockingLookup(creds, pub, "worker-a")
	b := startEmbedded(t, EmbeddedConfig{Auth: AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: lookup}})

	type dialed struct {
		nc  *nats.Conn
		err error
	}
	dial := make(chan dialed, 1)
	go func() {
		nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub))
		dial <- dialed{nc, err}
	}()
	waitClosed(t, entered, 5*time.Second, "the worker's authorization to reach the lookup")

	// The revocation commits while that authorization is in flight.
	creds.remove(pub)
	type revoked struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan revoked, 1)
	go func() {
		began := time.Now()
		err := b.Revoke(context.Background(), "worker-a")
		done <- revoked{err, time.Since(began)}
	}()

	// Let Revoke's first pass run against a broker that has not placed the
	// connection yet, then let the stale authorization complete.
	time.Sleep(100 * time.Millisecond)
	close(release)

	d := <-dial
	if d.err != nil {
		t.Fatalf("the in-flight authorization was refused (%v); the test needs it to succeed to mean anything", d.err)
	}
	defer d.nc.Close()

	r := <-done
	if r.err != nil {
		t.Fatalf("Revoke: %v", r.err)
	}
	if got := len(connzForUser(t, b, "worker-a")); got != 0 {
		t.Fatalf("broker holds %d connections for worker-a after Revoke returned", got)
	}
	b.mu.Lock()
	authTimeout := b.authTimeout
	b.mu.Unlock()
	if r.elapsed < authTimeout {
		t.Errorf("Revoke returned after %s, sooner than the auth timeout %s", r.elapsed, authTimeout)
	}
}

func TestEmbedded_RevokeHonoursCancellation(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	b := startAuthEmbedded(t, creds)

	nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub))
	if err != nil {
		t.Fatalf("connect as worker-a: %v", err)
	}
	defer nc.Close()
	creds.remove(pub)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	began := time.Now()
	err = b.Revoke(ctx, "worker-a")
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Errorf("Revoke with a canceled context took %s", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Revoke with a canceled context = %v, want context.Canceled", err)
	}
	// The first pass still ran.
	if got := len(connzForUser(t, b, "worker-a")); got != 0 {
		t.Fatalf("broker holds %d connections for worker-a after a canceled Revoke", got)
	}
}

// TestEmbedded_AuthTimeoutIsNATSDefault pins the value Revoke waits out to
// the auth timeout nats-server actually applies.
func TestEmbedded_AuthTimeoutIsNATSDefault(t *testing.T) {
	dir, _ := farmCerts(t)
	for name, tc := range map[string]struct {
		tls  BrokerTLSConfig
		want time.Duration
	}{
		"plaintext": {BrokerTLSConfig{}, 2 * time.Second},
		"tls":       {brokerTLSOf(dir, false), 3 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			b := startAuthEmbedded(t, newCredentialMap(), func(c *EmbeddedConfig) { c.TLS = tc.tls })
			b.mu.Lock()
			ns, got := b.ns, b.authTimeout
			b.mu.Unlock()
			if got != tc.want {
				t.Errorf("authTimeout = %s, want %s", got, tc.want)
			}
			vz, err := ns.Varz(nil)
			if err != nil {
				t.Fatal(err)
			}
			if applied := time.Duration(vz.AuthTimeout * float64(time.Second)); applied != got {
				t.Errorf("nats-server applies %s, Revoke waits %s", applied, got)
			}
		})
	}
}

// TestEmbedded_ShutdownAnswersInFlightAuthorization pins that a normal
// shutdown lets an authorization already being decided finish its reply
// rather than closing the callout connection under it.
func TestEmbedded_ShutdownAnswersInFlightAuthorization(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	lookup, entered, release := blockingLookup(creds, pub, "worker-a")
	logs := &syncBuffer{}
	b := startEmbeddedWithLogger(t, EmbeddedConfig{Auth: AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: lookup}},
		slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	go func() {
		if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub)); err == nil {
			nc.Close()
		}
	}()
	waitClosed(t, entered, 5*time.Second, "the worker's authorization to reach the lookup")

	stopped := make(chan struct{})
	go func() {
		b.Shutdown()
		close(stopped)
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	waitClosed(t, stopped, 10*time.Second, "Shutdown")

	if out := logs.String(); strings.Contains(out, "could not send its reply") || strings.Contains(out, "still unanswered") {
		t.Errorf("shutdown cut off an in-flight authorization:\n%s", out)
	}
}

func TestEmbedded_MaxStoreZeroIsUnlimited(t *testing.T) {
	keys, err := newEmbeddedKeys(false)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEmbedded(EmbeddedConfig{
		Addr:    net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir: t.TempDir() + "/nats",
	}, slog.New(slog.DiscardHandler))
	opts, err := e.serverOptions(Layout{Account: DefaultAccount, DataUserPub: keys.dataPub, JetStreamMaxStore: e.maxStoreBytes()})
	if err != nil {
		t.Fatal(err)
	}
	if opts.JetStreamMaxStore != -1 {
		t.Errorf("server JetStreamMaxStore = %d, want -1 (unlimited)", opts.JetStreamMaxStore)
	}

	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(e.Shutdown)
	e.mu.Lock()
	ns := e.ns
	e.mu.Unlock()
	acc, err := ns.LookupAccount(DefaultAccount)
	if err != nil {
		t.Fatal(err)
	}
	if got := acc.JetStreamUsage().Limits.MaxStore; got != -1 {
		t.Errorf("account %s max store = %d, want -1 (unlimited)", DefaultAccount, got)
	}
}

func TestEmbedded_StartTwiceFails(t *testing.T) {
	b := startBroker(t)
	// Without the guard a second Start still fails, on the port the first
	// one holds, but only after building a second server; the reason tells
	// the two apart.
	if err := b.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("second Start = %v, want an \"already started\" error", err)
	}
	if err := b.Check(context.Background()); err != nil {
		t.Fatalf("the running broker is unhealthy after a refused second Start: %v", err)
	}
}
