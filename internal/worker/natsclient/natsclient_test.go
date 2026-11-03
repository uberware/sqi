// SPDX-License-Identifier: AGPL-3.0-or-later

package natsclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestExponentialBackoff_GrowsAndCaps(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	fn := exponentialBackoff(base)

	// Attempt 0 ≈ base ± 20%.
	d0 := fn(0)
	if d0 < time.Duration(float64(base)*0.8) || d0 > time.Duration(float64(base)*1.2) {
		t.Errorf("attempt 0 delay %v outside base ±20%%", d0)
	}

	// Large attempt counts are capped at MaxReconnectBackoff (+ up to +20% jitter).
	dBig := fn(1000)
	if dBig > time.Duration(float64(protocol.MaxReconnectBackoff)*(1+protocol.ReconnectJitterFraction)) {
		t.Errorf("attempt 1000 delay %v exceeds capped MaxReconnectBackoff+jitter", dBig)
	}
	if dBig < 0 {
		t.Errorf("delay must never be negative, got %v", dBig)
	}
}

func TestExponentialBackoff_NeverNegative(t *testing.T) {
	t.Parallel()
	fn := exponentialBackoff(time.Millisecond)
	for i := range 50 {
		if d := fn(i); d < 0 {
			t.Fatalf("attempt %d produced negative delay %v", i, d)
		}
	}
}

func TestBuildTLSOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		cfg      workerconfig.NATSConfig
		wantOpts bool
		wantErr  string // a substring the error must contain; "" for none
	}{
		{"no tls", workerconfig.NATSConfig{}, false, ""},
		// "auto" is the default and infers from the other tls_* settings.
		{"auto with a ca file infers tls", workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto, InsecureSkipVerify: true}, true, ""},
		{"auto with nothing set stays plaintext", workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto}, false, ""},
		// "true" forces TLS for a broker with a publicly-trusted certificate,
		// where no CA file would signal intent.
		{"true forces tls with system roots", workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSOn}, true, ""},
		// "false" forces plaintext even against configured TLS material —
		// the state a force-on-only boolean could not express.
		{"false overrides a configured ca file", workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSOff, TLSCAFile: "/does/not/exist.pem"}, false, ""},
		{"false overrides insecure_skip_verify", workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSOff, InsecureSkipVerify: true}, false, ""},
		{"insecure skip verify", workerconfig.NATSConfig{InsecureSkipVerify: true}, true, ""},
		{"cert without key errors", workerconfig.NATSConfig{TLSCertFile: "/x.crt"}, false, "tls_cert_file and tls_key_file must both be set"},
		{"bad ca file errors", workerconfig.NATSConfig{TLSCAFile: "/does/not/exist.pem"}, false, "/does/not/exist.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := buildTLSOptions(tc.cfg)
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr == "" {
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
	t.Parallel()
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

// TestConnect_DialErrorRedactsCredentials checks that a dial failure names
// the broker URL without the token or password it may carry.
func TestConnect_DialErrorRedactsCredentials(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"nats://s3cr3tt0ken@127.0.0.1:1", "nats://sqi:hunter2@127.0.0.1:1"} {
		cfg := workerconfig.NATSConfig{URL: raw, ReconnectWait: 10 * time.Millisecond}
		_, _, err := Connect(context.Background(), cfg, "worker-a", nil, "", slog.New(slog.DiscardHandler))
		if err == nil {
			t.Fatal("Connect to dead port: want error, got nil")
		}
		for _, secret := range []string{"s3cr3tt0ken", "hunter2"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("Connect error %q repeats the credential %q", err, secret)
			}
		}
		if !strings.Contains(err.Error(), "nats://REDACTED@127.0.0.1:1") {
			t.Fatalf("Connect error %q does not name the redacted URL", err)
		}
	}
}

func TestBuildOptions_AddsNkeyOptionWhenSeedPresent(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}

	opts, err := buildOptions(workerconfig.NATSConfig{}, "worker-a", seed, pub, newLifecycle(context.Background(), logger, true, 0, authRejectGrace))
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
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	opts, err := buildOptions(workerconfig.NATSConfig{}, "worker-a", nil, "", newLifecycle(context.Background(), logger, false, 0, 0))
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

// randomPortAddr has nats-server bind a port the OS picks (-1 is its
// RANDOM_PORT), so tests running in parallel never race for one. The broker's
// ClientURL reports the port it got.
const randomPortAddr = "127.0.0.1:-1"

// freePort asks the OS for an unused loopback TCP port, for a test that must
// bring a broker back up at the same address. Such a test must not run in
// parallel: between the probe and the bind, or across a restart, another
// test's broker could take the port.
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

// enrolment is the one worker credential a test broker's auth callout
// accepts, until it is revoked.
type enrolment struct {
	mu       sync.Mutex
	workerID string
	pub      string
	revoked  bool

	// outageUntil makes every lookup fail with a store error until then,
	// as a database restart or an unreachable sqi-server does.
	outageUntil time.Time
}

// errStoreUnavailable is the lookup error a store outage produces.
var errStoreUnavailable = errors.New("store unavailable")

func (e *enrolment) lookup(_ context.Context, pub string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Now().Before(e.outageUntil) {
		return "", errStoreUnavailable
	}
	if e.revoked || pub != e.pub {
		return "", bus.ErrUnknownCredential
	}
	return e.workerID, nil
}

// outage makes the credential store unavailable for d, starting now.
func (e *enrolment) outage(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.outageUntil = time.Now().Add(d)
}

func (e *enrolment) revoke() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked = true
}

// startTestBroker boots a real embedded NATS broker with auth enabled and
// exactly one enrolled worker credential, on a throwaway loopback port and
// JetStream dir. It exists to prove [Connect]'s classification of a rejected
// credential against a REAL broker, not a mock — an nkey auth handshake is
// exactly the kind of wire behavior a fake cannot reproduce faithfully.
func startTestBroker(t *testing.T, enrolled *enrolment) *bus.Embedded {
	t.Helper()
	return startTestBrokerWithTTL(t, enrolled, time.Hour)
}

// startTestBrokerWithTTL is [startTestBroker] with the lifetime of the user
// JWTs the auth callout issues.
func startTestBrokerWithTTL(t *testing.T, enrolled *enrolment, userTTL time.Duration) *bus.Embedded {
	t.Helper()
	return startTestBrokerAt(t, randomPortAddr, t.TempDir()+"/nats", enrolled, userTTL)
}

// startTestBrokerAt is [startTestBrokerWithTTL] on a given address and
// JetStream dir, so a test can stop a broker and bring another up in its
// place.
func startTestBrokerAt(t *testing.T, addr, dataDir string, enrolled *enrolment, userTTL time.Duration) *bus.Embedded {
	t.Helper()
	b := bus.NewEmbedded(bus.EmbeddedConfig{
		Addr:    addr,
		DataDir: dataDir,
		Auth: bus.AuthConfig{
			Enabled: true,
			UserTTL: userTTL,
			Lookup:  enrolled.lookup,
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

// logBuffer is a log sink safe to read while nats.go's callback goroutine
// is still writing to it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines returns the logged lines at level that contain substr.
func (b *logBuffer) lines(level slog.Level, substr string) []string {
	var out []string
	for line := range strings.SplitSeq(b.String(), "\n") {
		if strings.Contains(line, "level="+level.String()) && strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

func newTestLogger() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// enrolledWorker generates a key and an enrolment that accepts it.
func enrolledWorker(t *testing.T) (seed []byte, pub string, enrolled *enrolment) {
	t.Helper()
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	return seed, pub, &enrolment{workerID: "worker-a", pub: pub}
}

// reconnectingConfig is a real worker's reconnect policy (unlimited
// attempts) with a short base wait.
func reconnectingConfig(url string) workerconfig.NATSConfig {
	return workerconfig.NATSConfig{URL: url, MaxReconnectAttempts: -1, ReconnectWait: 5 * time.Millisecond}
}

func TestConnect_ClassifiesRejectedCredentialAsFatal(t *testing.T) {
	t.Parallel()
	_, enrolledPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	b := startTestBroker(t, &enrolment{workerID: "worker-a", pub: enrolledPub})

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
	// A refused credential is retried for authRejectGrace before it is
	// final; keep that short here.
	grace := 500 * time.Millisecond
	logger := slog.New(slog.DiscardHandler)
	_, _, connErr := connect(context.Background(), cfg, "worker-a", strangerSeed, strangerPub, logger, grace)
	if connErr == nil {
		t.Fatal("Connect with an unenrolled nkey: want error, got nil")
	}
	if !errors.Is(connErr, nats.ErrAuthorization) && !errors.Is(connErr, nats.ErrAuthExpired) {
		t.Errorf("Connect error = %v, want it to wrap nats.ErrAuthorization or nats.ErrAuthExpired", connErr)
	}
}

func TestConnect_AuthOffFarmConnectsWithNoCredential(t *testing.T) {
	t.Parallel()
	b := bus.NewEmbedded(bus.EmbeddedConfig{
		Addr:    randomPortAddr,
		DataDir: t.TempDir() + "/nats",
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

// TestConnect_LiveRevocationNamesCauseAndRemediation covers a revocation
// while the worker is connected: the credential is revoked,
// internal/bus.Broker.Revoke disconnects the client that was using it, and
// the auth callout refuses its reconnect. The refusal is classified by the
// connection's lifecycle callbacks, not at the initial dial. Against a real
// broker, this test asserts that the crafted cause and remediation text
// reaches the logger, not a generic "connection closed" line.
func TestConnect_LiveRevocationNamesCauseAndRemediation(t *testing.T) {
	t.Parallel()
	enrolledSeed, enrolledPub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	enrolled := &enrolment{workerID: "worker-a", pub: enrolledPub}
	b := startTestBroker(t, enrolled)
	grace := 2 * time.Second

	logger, buf := newTestLogger()

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
	nc, closedCh, err := connect(context.Background(), cfg, "worker-a", enrolledSeed, enrolledPub, logger, grace)
	if err != nil {
		t.Fatalf("Connect with an enrolled credential: %v", err)
	}
	defer nc.Close()

	// Revoke the only enrolled credential: the store write, then the
	// broker call. Revocation is synchronous on the broker side; the worker
	// then retries the refused credential for authRejectGrace, and the
	// deadline below covers that grace plus scheduling jitter.
	enrolled.revoke()
	if err := b.Revoke(context.Background(), "worker-a"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	select {
	case <-closedCh:
	case <-time.After(10 * time.Second):
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
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	const workerID = "0f1d2c3b-4a59-6879-8a9b-0c1d2e3f4a5b"

	opts, err := buildOptions(workerconfig.NATSConfig{}, workerID, nil, "", newLifecycle(context.Background(), logger, false, 0, 0))
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
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	for _, workerID := range []string{"", "a.b", "a b", "*", ">"} {
		if _, err := buildOptions(workerconfig.NATSConfig{}, workerID, nil, "", newLifecycle(context.Background(), logger, false, 0, 0)); err == nil {
			t.Errorf("buildOptions with worker id %q: want error, got nil", workerID)
		}
	}
}

// TestConnect_RevokedWorkerStopsAfterGrace pins the far side of the grace: a
// revoked worker's reconnects are refused for as long as it keeps trying,
// and once the refusals have lasted authRejectGrace the worker gives up and
// names both causes a refusal can have.
func TestConnect_RevokedWorkerStopsAfterGrace(t *testing.T) {
	t.Parallel()
	seed, pub, enrolled := enrolledWorker(t)
	b := startTestBroker(t, enrolled)
	grace := 2 * time.Second
	logger, logs := newTestLogger()

	nc, closedCh, err := connect(context.Background(), reconnectingConfig(b.ClientURL()), "worker-a", seed, pub, logger, grace)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer nc.Close()

	closedAt := make(chan time.Time, 1)
	go func() {
		<-closedCh
		closedAt <- time.Now()
	}()

	enrolled.revoke()
	revokedAt := time.Now()
	if err := b.Revoke(context.Background(), "worker-a"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	select {
	case at := <-closedAt:
		if elapsed := at.Sub(revokedAt); elapsed < 2*time.Second {
			t.Errorf("connection closed %s after revocation, before the 2s grace", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("connection was not closed within 10s of revocation")
	}

	errs := logs.lines(slog.LevelError, "re-enroll")
	if len(errs) == 0 {
		t.Fatalf("no ERROR naming re-enrolment; logs:\n%s", logs)
	}
	if len(errs) != 1 {
		t.Errorf("rejection logged %d times, want once: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], "unavailable") {
		t.Errorf("rejection error does not name an outage as the other cause: %s", errs[0])
	}
	warns := logs.lines(slog.LevelWarn, "unavailable")
	if len(warns) == 0 {
		t.Fatalf("no WARN for the refusals inside the grace; logs:\n%s", logs)
	}
	if !strings.Contains(warns[0], "authentication off") {
		t.Errorf("in-grace WARN does not name an auth-off farm refusing the key: %s", warns[0])
	}
}

// TestConnect_StoreOutageShorterThanGraceRecovers is the case the grace
// exists for: the auth callout refuses every connection while the credential
// store is down, exactly as it refuses a revoked key, and a worker that has to
// reconnect during that outage must still be there when it ends.
func TestConnect_StoreOutageShorterThanGraceRecovers(t *testing.T) {
	t.Parallel()
	seed, pub, enrolled := enrolledWorker(t)
	b := startTestBroker(t, enrolled)
	grace := 2 * time.Second
	logger, logs := newTestLogger()

	nc, closedCh, err := connect(context.Background(), reconnectingConfig(b.ClientURL()), "worker-a", seed, pub, logger, grace)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer nc.Close()
	connected := nc.StatusChanged(nats.CONNECTED)
	defer nc.RemoveStatusListener(connected)

	// The key stays enrolled; Revoke here only forces the reconnect, which
	// lands inside the outage.
	enrolled.outage(time.Second)
	if err := b.Revoke(context.Background(), "worker-a"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	select {
	case <-connected:
	case <-closedCh:
		t.Fatalf("connection closed during a 1s store outage; logs:\n%s", logs)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reconnect after the store outage ended")
	}
	if len(logs.lines(slog.LevelWarn, "unavailable")) == 0 {
		t.Fatalf("the reconnect was never refused, so the outage was not exercised; logs:\n%s", logs)
	}

	select {
	case <-closedCh:
		t.Fatalf("connection closed after recovering from the outage; logs:\n%s", logs)
	case <-time.After(3 * time.Second):
	}
	if !nc.IsConnected() {
		t.Errorf("connection status = %v, want CONNECTED", nc.Status())
	}
}

// TestConnect_InitialDialRetriesThroughShortOutage covers a worker that boots
// while the credential store is down.
func TestConnect_InitialDialRetriesThroughShortOutage(t *testing.T) {
	t.Parallel()
	seed, pub, enrolled := enrolledWorker(t)
	b := startTestBroker(t, enrolled)
	grace := 2 * time.Second
	logger, logs := newTestLogger()

	enrolled.outage(time.Second)
	start := time.Now()
	nc, _, err := connect(context.Background(), reconnectingConfig(b.ClientURL()), "worker-a", seed, pub, logger, grace)
	if err != nil {
		t.Fatalf("Connect through a 1s store outage: %v; logs:\n%s", err, logs)
	}
	defer nc.Close()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Connect took %s, want under 5s", elapsed)
	}
	if len(logs.lines(slog.LevelWarn, "unavailable")) == 0 {
		t.Errorf("no WARN for the refused dials; logs:\n%s", logs)
	}
}

// TestConnect_InitialDialRejectedAfterGrace pins that a credential the
// broker keeps refusing at boot is still fatal, once the grace is spent.
func TestConnect_InitialDialRejectedAfterGrace(t *testing.T) {
	t.Parallel()
	_, _, enrolled := enrolledWorker(t)
	b := startTestBroker(t, enrolled)
	grace := 2 * time.Second
	strangerSeed, strangerPub, _ := enrolledWorker(t)

	start := time.Now()
	_, _, err := connect(context.Background(), reconnectingConfig(b.ClientURL()), "worker-a", strangerSeed, strangerPub, slog.New(slog.DiscardHandler), grace)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Connect with a never-enrolled key: want error, got nil")
	}
	if elapsed < 2*time.Second {
		t.Errorf("Connect gave up after %s, before the 2s grace", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Connect gave up after %s, long after the 2s grace", elapsed)
	}
	if !errors.Is(err, nats.ErrAuthorization) {
		t.Errorf("error %v does not wrap nats.ErrAuthorization", err)
	}
	for _, want := range []string{"re-enroll", "unavailable", "authentication off"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestConnect_InitialDialHonoursCancel covers a service Stop that lands while
// Connect is retrying a refused credential.
func TestConnect_InitialDialHonoursCancel(t *testing.T) {
	t.Parallel()
	_, _, enrolled := enrolledWorker(t)
	b := startTestBroker(t, enrolled)
	grace := time.Minute
	strangerSeed, strangerPub, _ := enrolledWorker(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := connect(ctx, reconnectingConfig(b.ClientURL()), "worker-a", strangerSeed, strangerPub, slog.New(slog.DiscardHandler), grace)
	if err == nil {
		t.Fatal("Connect after cancel: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Connect returned %s after start, want promptly after the 300ms cancel", elapsed)
	}
}

// TestConnect_NoCredentialRefusedWithoutGrace pins that the grace applies only
// to a worker that presents a credential: a store outage cannot be why an
// anonymous connection is refused, so retrying it would only delay the
// "no credential" diagnosis at boot.
func TestConnect_NoCredentialRefusedWithoutGrace(t *testing.T) {
	t.Parallel()
	_, _, enrolled := enrolledWorker(t)
	b := startTestBroker(t, enrolled)
	grace := time.Minute

	start := time.Now()
	_, _, err := connect(context.Background(), reconnectingConfig(b.ClientURL()), "worker-a", nil, "", slog.New(slog.DiscardHandler), grace)
	if err == nil {
		t.Fatal("Connect with no credential against an auth-on broker: want error, got nil")
	}
	if !errors.Is(err, nats.ErrAuthorization) {
		t.Errorf("error %v does not wrap nats.ErrAuthorization", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Connect took %s to refuse a connection with no credential, want no grace", elapsed)
	}
}

// TestConnect_RoutineUserJWTExpiryIsQuiet covers the reconnect every worker
// makes once per nats.auth.user_ttl: the broker ends the session with
// "authentication expired" and the reconnect is authorized afresh. That is
// routine and must not read as a failure.
func TestConnect_RoutineUserJWTExpiryIsQuiet(t *testing.T) {
	t.Parallel()
	seed, pub, enrolled := enrolledWorker(t)
	b := startTestBrokerWithTTL(t, enrolled, 2*time.Second)
	grace := 2 * time.Second
	logger, logs := newTestLogger()

	nc, closedCh, err := connect(context.Background(), reconnectingConfig(b.ClientURL()), "worker-a", seed, pub, logger, grace)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer nc.Close()
	connected := nc.StatusChanged(nats.CONNECTED)
	defer nc.RemoveStatusListener(connected)

	// Two expiries: the second proves the first left no streak behind.
	for i := range 2 {
		select {
		case <-connected:
		case <-closedCh:
			t.Fatalf("connection closed at user JWT expiry %d; logs:\n%s", i+1, logs)
		case <-time.After(10 * time.Second):
			t.Fatalf("no reconnect after user JWT expiry %d; logs:\n%s", i+1, logs)
		}
	}

	if len(logs.lines(slog.LevelInfo, "expired")) == 0 {
		t.Errorf("no INFO for the expiry; logs:\n%s", logs)
	}
	for _, level := range []slog.Level{slog.LevelWarn, slog.LevelError} {
		if got := logs.lines(level, ""); len(got) != 0 {
			t.Errorf("routine expiry logged at %s: %v", level, got)
		}
	}
}

// waitFor polls cond until it holds or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestConnect_RefusalThenUnreachableThenBackDoesNotSpin covers a refusal
// streak that the broker going away interrupts. The refusals are no longer
// what keeps the worker out, so the streak must end: otherwise its spent
// grace caps every reconnect delay at nothing and nats.go redials the dead
// broker in a tight loop, and the first refusal after the broker returns is
// judged against a start that predates the outage.
func TestConnect_RefusalThenUnreachableThenBackDoesNotSpin(t *testing.T) {
	seed, pub, enrolled := enrolledWorker(t)
	addr := net.JoinHostPort("127.0.0.1", itoa(freePort(t)))
	dataDir := t.TempDir() + "/nats"
	first := startTestBrokerAt(t, addr, dataDir, enrolled, time.Hour)
	grace := time.Second
	logger, logs := newTestLogger()

	var delays atomic.Int64
	countDelays := func(o *nats.Options) error {
		next := o.CustomReconnectDelayCB
		o.CustomReconnectDelayCB = func(attempts int) time.Duration {
			delays.Add(1)
			return next(attempts)
		}
		return nil
	}
	cfg := workerconfig.NATSConfig{URL: first.ClientURL(), MaxReconnectAttempts: -1, ReconnectWait: 10 * time.Millisecond}
	nc, closedCh, err := connect(context.Background(), cfg, "worker-a", seed, pub, logger, grace, countDelays)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	connected := nc.StatusChanged(nats.CONNECTED)
	defer nc.RemoveStatusListener(connected)

	// A refusal: the broker comes back while the credential store is down.
	enrolled.outage(time.Hour)
	first.Shutdown()
	refusing := startTestBrokerAt(t, addr, dataDir, enrolled, time.Hour)
	waitFor(t, 10*time.Second, "a refused reconnect", func() bool {
		return len(logs.lines(slog.LevelWarn, "refused")) > 0
	})

	// Then the broker is unreachable for well past the grace.
	refusing.Shutdown()
	delays.Store(0)
	select {
	case <-closedCh:
		t.Fatalf("connection closed while the broker was unreachable; logs:\n%s", logs)
	case <-time.After(3 * time.Second):
	}
	if n := delays.Load(); n > 20 {
		t.Errorf("%d reconnect delays in 3s with the broker down, want a backoff, not a spin", n)
	}

	// The broker returns with the store healthy: the worker reconnects.
	enrolled.outage(0)
	startTestBrokerAt(t, addr, dataDir, enrolled, time.Hour)
	select {
	case <-connected:
	case <-closedCh:
		t.Fatalf("connection closed when the broker returned; logs:\n%s", logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("worker did not reconnect when the broker returned; logs:\n%s", logs)
	}
}

// TestConnect_RepeatedExpiryWithoutReconnectGivesUp covers a broker that sees
// every freshly issued user JWT as already expired, as clock skew past
// nats.auth.user_ttl would make it. The worker connects to a healthy broker,
// which is then replaced on the same address by one issuing JWTs with a 1ns
// lifetime, so each reconnect is refused as expired before it is CONNECTED.
// Only the first expiry is routine; the rest are refusals, so the worker
// gives up after the grace.
func TestConnect_RepeatedExpiryWithoutReconnectGivesUp(t *testing.T) {
	seed, pub, enrolled := enrolledWorker(t)
	addr := net.JoinHostPort("127.0.0.1", itoa(freePort(t)))
	dataDir := t.TempDir() + "/nats"
	healthy := startTestBrokerAt(t, addr, dataDir, enrolled, time.Hour)
	grace := time.Second
	logger, logs := newTestLogger()

	nc, closedCh, err := connect(context.Background(), reconnectingConfig(healthy.ClientURL()), "worker-a", seed, pub, logger, grace)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer nc.Close()

	healthy.Shutdown()
	startTestBrokerAt(t, addr, dataDir, enrolled, time.Nanosecond)
	skewedSince := time.Now()

	select {
	case <-closedCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("worker never gave up on a broker refusing every reconnect as expired; logs:\n%s", logs)
	}
	if elapsed := time.Since(skewedSince); elapsed < time.Second {
		t.Errorf("gave up %s after the skewed broker started, before the 1s grace", elapsed)
	}
	if errs := logs.lines(slog.LevelError, "re-enroll"); len(errs) != 1 {
		t.Errorf("rejection logged %d times, want once; logs:\n%s", len(errs), logs)
	}
}

// TestLifecycle_OnlyFirstExpiryAfterConnectedIsRoutine pins the expiry
// classifier deterministically, independent of broker timing.
func TestLifecycle_OnlyFirstExpiryAfterConnectedIsRoutine(t *testing.T) {
	t.Parallel()
	up := func() bool { return true }
	down := func() bool { return false }
	lc := newLifecycle(context.Background(), slog.New(slog.DiscardHandler), true, 0, 50*time.Millisecond)

	// A session ends at expiry and reconnects: routine, every time.
	for i := range 3 {
		if v, _ := lc.record(up, true); v != authExpired {
			t.Fatalf("expiry %d on a live connection: verdict %d, want authExpired", i+1, v)
		}
		lc.clear()
	}

	// An expiry, then the reconnect is refused as expired: a refusal.
	if v, _ := lc.record(up, true); v != authExpired {
		t.Fatalf("first expiry: verdict %d, want authExpired", v)
	}
	if !lc.expiring {
		t.Fatal("first expiry did not mark the coming disconnect as expected")
	}
	if v, _ := lc.record(down, true); v != authRefused {
		t.Fatalf("second expiry before CONNECTED: verdict %d, want authRefused", v)
	}
	time.Sleep(60 * time.Millisecond)
	if v, _ := lc.record(down, true); v != authGiveUp {
		t.Fatalf("expiry past the grace: verdict %d, want authGiveUp", v)
	}
}

// TestLifecycle_ClearForgetsExpectedExpiryDisconnect pins that CONNECTED
// drops the "expected disconnect" mark, so it cannot quiet a later,
// unrelated disconnect.
func TestLifecycle_ClearForgetsExpectedExpiryDisconnect(t *testing.T) {
	t.Parallel()
	lc := newLifecycle(context.Background(), slog.New(slog.DiscardHandler), true, 0, authRejectGrace)
	if v, _ := lc.record(func() bool { return true }, true); v != authExpired {
		t.Fatalf("verdict %d, want authExpired", v)
	}
	lc.clear()
	if lc.expiring || lc.expired {
		t.Errorf("after CONNECTED: expiring=%v expired=%v, want both false", lc.expiring, lc.expired)
	}
}

// TestLifecycle_UnreachableBrokerEndsStreak pins the ReconnectErrHandler's
// part in keeping reconnects from spinning: a reconnect that cannot reach the
// broker ends the streak, and a refusal does not.
func TestLifecycle_UnreachableBrokerEndsStreak(t *testing.T) {
	t.Parallel()
	lc := newLifecycle(context.Background(), slog.New(slog.DiscardHandler), true, 0, authRejectGrace)
	lc.record(func() bool { return false }, false)
	lc.onReconnectErr(nil, nats.ErrAuthorization)
	if lc.refusals != 1 {
		t.Fatalf("a refusal reported as a reconnect error ended the streak")
	}
	lc.onReconnectErr(nil, errors.New("dial tcp: connection refused"))
	if lc.refusals != 0 || !lc.since.IsZero() {
		t.Errorf("unreachable broker left the streak running: refusals=%d since=%v", lc.refusals, lc.since)
	}
}

// TestLifecycle_CappedDelayKeepsBackoffFloor pins the delay cap's part in
// keeping reconnects from spinning: a spent grace never makes the reconnect
// delay zero.
func TestLifecycle_CappedDelayKeepsBackoffFloor(t *testing.T) {
	t.Parallel()
	const base = 10 * time.Millisecond
	lc := newLifecycle(context.Background(), slog.New(slog.DiscardHandler), true, base, time.Millisecond)
	lc.record(func() bool { return false }, false)
	time.Sleep(5 * time.Millisecond)
	delay := lc.capDelay(func(int) time.Duration { return time.Second })
	if d := delay(1); d != base {
		t.Errorf("capped delay with the grace spent = %s, want the %s floor", d, base)
	}
}

func TestIsAuthRefusal(t *testing.T) {
	t.Parallel()
	for _, err := range []error{nats.ErrAuthorization, nats.ErrAuthExpired, nats.ErrAuthRevoked, nats.ErrAccountAuthExpired} {
		if !isAuthRefusal(err) {
			t.Errorf("isAuthRefusal(%v) = false, want true", err)
		}
	}
	// The initial dial's errors carry the server's -ERR text, not the
	// sentinels, for everything but an authorization violation.
	for _, text := range []string{
		"nats: Authorization Violation",
		"nats: User Authentication Expired",
		"nats: User Authentication Revoked",
		"nats: Account Authentication Expired",
	} {
		if !isAuthRefusal(errors.New(text)) {
			t.Errorf("isAuthRefusal(%q) = false, want true", text)
		}
	}
	for _, err := range []error{nil, nats.ErrNoServers, errors.New("EOF"), errors.New("nats: maximum connections exceeded")} {
		if isAuthRefusal(err) {
			t.Errorf("isAuthRefusal(%v) = true, want false", err)
		}
	}
}
