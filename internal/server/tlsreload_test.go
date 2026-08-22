// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store/fake"
)

// startTestBrokerTLS boots a real embedded broker with BOTH authentication
// and TLS enabled, mirroring startTestBroker in revokeworker_test.go.
func startTestBrokerTLS(t *testing.T, creds []bus.WorkerCredentialRef, certFile, keyFile string) *bus.Broker {
	t.Helper()
	b := bus.New(bus.BrokerConfig{
		Addr:       freeLoopbackAddr(t),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
		Auth:       bus.BrokerAuthConfig{Enabled: true, Credentials: creds},
		TLS:        bus.BrokerTLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile},
	}, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startTestBrokerTLS: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// tlsPoolFor returns a root pool trusting the CA that sits beside certFile.
func tlsPoolFor(t *testing.T, certFile string) *x509.CertPool {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(filepath.Dir(certFile), "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("ca.crt did not append to pool")
	}
	return pool
}

// TestRevokeWorker_TLSSurvivesCredentialReload pins the one behavior that
// lives only at the seam between H1 and H2, and that neither component's own
// tests would ever exercise.
//
// RevokeWorker calls bus.Broker.ReloadCredentials, which clones the pristine
// boot Options and hands them to nats-server's ReloadOptions. Options.Clone()
// deep-copies TLSConfig (nats-server opts.go:828), so TLS survives. If that
// ever stopped being true — or if broker.go set opts.TLSConfig AFTER taking
// the bootOpts clone — then revoking any single worker would silently drop
// the entire broker to plaintext, and every other test in the tree would
// still pass.
func TestRevokeWorker_TLSSurvivesCredentialReload(t *testing.T) {
	certFile, keyFile, _ := writeServerCerts(t, 365*24*time.Hour)
	st := fake.New()
	refA, seedA := enrolledCredential(t, st, "worker-a")
	refB, _ := enrolledCredential(t, st, "worker-b")

	broker := startTestBrokerTLS(t, []bus.WorkerCredentialRef{refA, refB}, certFile, keyFile)
	s := &Server{cfg: Config{NATSAuthEnabled: true}, store: st, broker: broker, logger: testLogger()}
	pool := tlsPoolFor(t, certFile)

	secure := nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool})

	// A connects over TLS before the reload.
	ncA, err := nats.Connect(broker.ClientURL(), secure, nkeyOption(t, seedA, refA.PublicKey), nats.NoReconnect())
	if err != nil {
		t.Fatalf("worker A could not connect over TLS: %v", err)
	}
	ncA.Close()

	// Revoke B. This is the call that reloads the broker's options.
	if err := s.RevokeWorker(context.Background(), refB.WorkerID); err != nil {
		t.Fatalf("RevokeWorker: %v", err)
	}

	// THE assertion: TLS is still required after the reload.
	if plain, err := nats.Connect(broker.ClientURL(), nats.NoReconnect()); err == nil {
		plain.Close()
		t.Fatal("a plaintext client connected after a credential reload; the reload dropped broker TLS")
	}

	// And A can still connect over TLS afterwards.
	ncA2, err := nats.Connect(broker.ClientURL(), secure, nkeyOption(t, seedA, refA.PublicKey), nats.NoReconnect())
	if err != nil {
		t.Fatalf("worker A could not reconnect over TLS after the reload: %v", err)
	}
	ncA2.Close()
}
