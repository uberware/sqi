// SPDX-License-Identifier: AGPL-3.0-or-later

package natsclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

func TestExponentialBackoff_GrowsAndCaps(t *testing.T) {
	base := 100 * time.Millisecond
	fn := exponentialBackoff(base)

	// Attempt 0 ≈ base ± 20%.
	d0 := fn(0)
	if d0 < time.Duration(float64(base)*0.8) || d0 > time.Duration(float64(base)*1.2) {
		t.Errorf("attempt 0 delay %v outside base ±20%%", d0)
	}

	// Large attempt counts are capped at maxBackoff (+ up to +20% jitter).
	dBig := fn(1000)
	if dBig > time.Duration(float64(maxBackoff)*1.2) {
		t.Errorf("attempt 1000 delay %v exceeds capped maxBackoff+jitter", dBig)
	}
	if dBig < 0 {
		t.Errorf("delay must never be negative, got %v", dBig)
	}
}

func TestExponentialBackoff_NeverNegative(t *testing.T) {
	fn := exponentialBackoff(time.Millisecond)
	for i := range 50 {
		if d := fn(i); d < 0 {
			t.Fatalf("attempt %d produced negative delay %v", i, d)
		}
	}
}

func TestBuildTLSOptions(t *testing.T) {
	cases := []struct {
		name     string
		cfg      workerconfig.NATSConfig
		wantOpts bool
		wantErr  bool
	}{
		{"no tls", workerconfig.NATSConfig{}, false, false},
		{"insecure skip verify", workerconfig.NATSConfig{InsecureSkipVerify: true}, true, false},
		{"cert without key errors", workerconfig.NATSConfig{TLSCertFile: "/x.crt"}, false, true},
		{"bad ca file errors", workerconfig.NATSConfig{TLSCAFile: "/does/not/exist.pem"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := buildTLSOptions(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr {
				if tc.wantOpts && len(opts) == 0 {
					t.Error("expected TLS options, got none")
				}
				if !tc.wantOpts && len(opts) != 0 {
					t.Errorf("expected no TLS options, got %d", len(opts))
				}
			}
		})
	}
}

func TestConnect_DialErrorIsReturned(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := workerconfig.NATSConfig{
		URL:                  "nats://127.0.0.1:1", // nothing listens on port 1
		MaxReconnectAttempts: 0,
		ReconnectWait:        10 * time.Millisecond,
	}
	_, _, err := Connect(context.Background(), cfg, "worker-a", nil, "", logger)
	if err == nil {
		t.Fatal("Connect to dead port: want error, got nil")
	}
}

func TestBuildOptions_AddsNkeyOptionWhenSeedPresent(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}

	opts, err := buildOptions(context.Background(), workerconfig.NATSConfig{}, "worker-a", seed, pub, logger, make(chan struct{}))
	if err != nil {
		t.Fatalf("buildOptions: %v", err)
	}

	applied := &nats.Options{}
	for _, opt := range opts {
		if err := opt(applied); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
	if applied.Nkey != pub {
		t.Errorf("Options.Nkey = %q, want %q", applied.Nkey, pub)
	}
	if applied.SignatureCB == nil {
		t.Fatal("Options.SignatureCB is nil; want a signing callback")
	}

	// The callback must actually sign with the given seed, not just be
	// present — verify a nonce signs against the matching public key.
	nonce := []byte("test-nonce")
	sig, err := applied.SignatureCB(nonce)
	if err != nil {
		t.Fatalf("SignatureCB: %v", err)
	}
	kp, err := nkeys.FromPublicKey(pub)
	if err != nil {
		t.Fatalf("FromPublicKey: %v", err)
	}
	if err := kp.Verify(nonce, sig); err != nil {
		t.Errorf("signature does not verify against %s: %v", pub, err)
	}
}

func TestBuildOptions_NoNkeyOptionWhenSeedEmpty(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	opts, err := buildOptions(context.Background(), workerconfig.NATSConfig{}, "worker-a", nil, "", logger, make(chan struct{}))
	if err != nil {
		t.Fatalf("buildOptions: %v", err)
	}

	applied := &nats.Options{}
	for _, opt := range opts {
		if err := opt(applied); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
	if applied.Nkey != "" {
		t.Errorf("Options.Nkey = %q, want empty", applied.Nkey)
	}
	if applied.SignatureCB != nil {
		t.Error("Options.SignatureCB is set; want nil when no seed is configured")
	}
}

// freePort asks the OS for an unused loopback TCP port, for booting a
// throwaway embedded broker.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("freePort: listener address is %T, want *net.TCPAddr", l.Addr())
	}
	return addr.Port
}

// startTestBroker boots a real embedded NATS broker with auth enabled and
// exactly one enrolled worker credential, on a throwaway loopback port and
// JetStream dir. It exists to prove [Connect]'s classification of a rejected
// credential against a REAL broker, not a mock — an nkey auth handshake is
// exactly the kind of wire behavior a fake cannot reproduce faithfully.
func startTestBroker(t *testing.T, enrolled bus.WorkerCredentialRef) *bus.Broker {
	t.Helper()
	b := bus.New(bus.BrokerConfig{
		Addr:    net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir: t.TempDir() + "/nats",
		Auth: bus.BrokerAuthConfig{
			Enabled:     true,
			Credentials: []bus.WorkerCredentialRef{enrolled},
		},
	}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startTestBroker: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

func itoa(p int) string {
	return (&net.TCPAddr{Port: p}).String()[1:] // ":<port>"[1:] == "<port>"
}

func TestConnect_ClassifiesRejectedCredentialAsFatal(t *testing.T) {
	_, enrolledPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	b := startTestBroker(t, bus.WorkerCredentialRef{WorkerID: "worker-a", PublicKey: enrolledPub})

	// A freshly generated keypair that was never enrolled with the broker.
	strangerSeed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	strangerPub, err := brokerauth.PublicKeyFromSeed(strangerSeed)
	if err != nil {
		t.Fatalf("PublicKeyFromSeed: %v", err)
	}

	cfg := workerconfig.NATSConfig{
		URL:                  b.ClientURL(),
		MaxReconnectAttempts: 0,
		ReconnectWait:        10 * time.Millisecond,
	}
	logger := slog.New(slog.DiscardHandler)
	_, _, connErr := Connect(context.Background(), cfg, "worker-a", strangerSeed, strangerPub, logger)
	if connErr == nil {
		t.Fatal("Connect with an unenrolled nkey: want error, got nil")
	}
	if !errors.Is(connErr, nats.ErrAuthorization) && !errors.Is(connErr, nats.ErrAuthExpired) {
		t.Errorf("Connect error = %v, want it to wrap nats.ErrAuthorization or nats.ErrAuthExpired", connErr)
	}
}

func TestConnect_AuthOffFarmConnectsWithNoCredential(t *testing.T) {
	b := bus.New(bus.BrokerConfig{
		Addr:    net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir: t.TempDir() + "/nats",
		Auth:    bus.BrokerAuthConfig{Enabled: false},
	}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.Shutdown)

	cfg := workerconfig.NATSConfig{
		URL:                  b.ClientURL(),
		MaxReconnectAttempts: 0,
		ReconnectWait:        10 * time.Millisecond,
	}
	logger := slog.New(slog.DiscardHandler)
	nc, _, err := Connect(context.Background(), cfg, "worker-a", nil, "", logger)
	if err != nil {
		t.Fatalf("Connect with no credential against an auth-off broker: %v", err)
	}
	defer nc.Close()
	if !nc.IsConnected() {
		t.Error("connection is not in the connected state")
	}
}

// TestConnect_LiveRevocationNamesCauseAndRemediation covers the common
// revocation shape in this project, not just a rejected initial dial:
// internal/bus.Broker.ReloadCredentials drops an enrolled key and
// synchronously disconnects the client that was using it (authViolation on
// the server side). The worker is already connected when this happens, so
// the classification has to happen in the ClosedHandler, not at the initial
// nats.Connect call — this test drives that path against a real broker and
// asserts the SAME crafted cause+remediation text reaches the logger, not a
// generic "connection closed" line.
func TestConnect_LiveRevocationNamesCauseAndRemediation(t *testing.T) {
	enrolledSeed, enrolledPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	b := startTestBroker(t, bus.WorkerCredentialRef{WorkerID: "worker-a", PublicKey: enrolledPub})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := workerconfig.NATSConfig{
		URL: b.ClientURL(),
		// Reconnect enabled (unlimited, a short wait) deliberately —
		// matching a real worker's config, NOT nats.NoReconnect(). nats.go
		// only lands nats.ErrAuthorization in nc.LastError() by way of the
		// RECONNECT handshake itself hitting the same rejected credential
		// twice (see nats.go's processConnectInit / nc.ar): the very first
		// disconnect races an async "-ERR authorization violation" against
		// the raw TCP close, and that race's loser is whichever error
		// happened to land in nc.err last — with reconnect enabled, that
		// race is irrelevant because a fresh CONNECT attempt reproduces the
		// auth error deterministically, which is what makes this
		// classification reliable in production rather than a coin flip.
		MaxReconnectAttempts: -1,
		ReconnectWait:        5 * time.Millisecond,
	}
	nc, closedCh, err := Connect(context.Background(), cfg, "worker-a", enrolledSeed, enrolledPub, logger)
	if err != nil {
		t.Fatalf("Connect with an enrolled credential: %v", err)
	}
	defer nc.Close()

	// Revoke the only enrolled credential. Revocation is synchronous on the
	// broker side; the deadline below tolerates client-side scheduling
	// jitter in observing the resulting close, not a slow revocation.
	if err := b.ReloadCredentials(nil); err != nil {
		t.Fatalf("ReloadCredentials: %v", err)
	}

	select {
	case <-closedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("connection was not closed after credential revocation")
	}

	logs := buf.String()
	if !strings.Contains(logs, "the broker rejected this worker's credential") {
		t.Errorf("logs do not name the cause; got:\n%s", logs)
	}
	if !strings.Contains(logs, "re-enroll with a new join token") {
		t.Errorf("logs do not name the remediation; got:\n%s", logs)
	}
}

// TestBuildOptions_SetsPerWorkerInboxPrefix pins the option that keeps a
// worker's lease replies out of every other worker's reach: without it the
// connection takes nats.go's process-global "_INBOX", and the only grant
// that could cover a reply would also cover every other client's inbox on
// the same broker. The matching permission is asserted in
// internal/brokerauth; the end-to-end consequence in internal/bus.
func TestBuildOptions_SetsPerWorkerInboxPrefix(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	const workerID = "0f1d2c3b-4a59-6879-8a9b-0c1d2e3f4a5b"

	opts, err := buildOptions(context.Background(), workerconfig.NATSConfig{}, workerID, nil, "", logger, make(chan struct{}))
	if err != nil {
		t.Fatalf("buildOptions: %v", err)
	}
	applied := &nats.Options{}
	for _, opt := range opts {
		if err := opt(applied); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
	if want := brokerauth.InboxPrefix(workerID); applied.InboxPrefix != want {
		t.Errorf("Options.InboxPrefix = %q, want %q", applied.InboxPrefix, want)
	}
}

// TestBuildOptions_RejectsWorkerIDThatIsNotASubjectToken covers the
// defensive check: worker.id holds a UUID, but it is a file an operator can
// edit. An absent ID would leave the connection on nats.go's shared "_INBOX"
// prefix, and one carrying a "." or a wildcard would widen the subtree the
// broker grant built from it covers.
func TestBuildOptions_RejectsWorkerIDThatIsNotASubjectToken(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for _, workerID := range []string{"", "a.b", "a b", "*", ">"} {
		if _, err := buildOptions(context.Background(), workerconfig.NATSConfig{}, workerID, nil, "", logger, make(chan struct{})); err == nil {
			t.Errorf("buildOptions with worker id %q: want error, got nil", workerID)
		}
	}
}
