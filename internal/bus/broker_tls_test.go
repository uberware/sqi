// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/certgen"
)

// farmCerts generates a CA plus a broker server keypair covering loopback,
// and returns the directory holding ca.crt / server.crt / server.key along
// with the CA itself so a test can also mint client certificates.
func farmCerts(t *testing.T) (dir string, ca *certgen.CA) {
	t.Helper()
	dir = t.TempDir()
	ca, err := certgen.NewCA("test farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"localhost", "127.0.0.1", "::1"}, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}
	return dir, ca
}

// startBrokerTLS boots an embedded broker with the given TLS and auth
// configuration on an OS-assigned loopback port.
func startBrokerTLS(t *testing.T, tlsCfg BrokerTLSConfig, auth BrokerAuthConfig) *Broker {
	t.Helper()
	cfg := BrokerConfig{
		Addr:       net.JoinHostPort("127.0.0.1", itoa(freePort(t))),
		DataDir:    t.TempDir() + "/nats",
		MaxStoreMB: 64,
		Auth:       auth,
		TLS:        tlsCfg,
	}
	b := New(cfg, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("startBrokerTLS: Start: %v", err)
	}
	t.Cleanup(b.Shutdown)
	return b
}

// caPool returns a root pool trusting only the CA in dir.
func caPool(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("ca.crt did not append to pool")
	}
	return pool
}

// brokerTLSOf builds the BrokerTLSConfig pointing at dir's server keypair.
func brokerTLSOf(dir string, clientCA bool) BrokerTLSConfig {
	cfg := BrokerTLSConfig{
		Enabled:  true,
		CertFile: filepath.Join(dir, "server.crt"),
		KeyFile:  filepath.Join(dir, "server.key"),
	}
	if clientCA {
		cfg.ClientCAFile = filepath.Join(dir, "ca.crt")
	}
	return cfg
}

// clientCertOption loads a generated client keypair as a nats TLS option.
func clientCertOption(t *testing.T, ca *certgen.CA, pool *x509.CertPool, commonName string) nats.Option {
	t.Helper()
	leaf, err := ca.NewClientCert(commonName, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewClientCert: %v", err)
	}
	pair, err := tls.X509KeyPair(leaf.CertPEM, leaf.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return nats.Secure(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
		Certificates: []tls.Certificate{pair},
	})
}

func TestBrokerTLS_RequiredWhenEnabled(t *testing.T) {
	dir, _ := farmCerts(t)
	b := startBrokerTLS(t, brokerTLSOf(dir, false), BrokerAuthConfig{})

	nc, err := nats.Connect(b.ClientURL(), nats.Secure(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    caPool(t, dir),
	}))
	if err != nil {
		t.Fatalf("TLS client could not connect: %v", err)
	}
	nc.Close()

	// Setting Options.TLSConfig is what makes nats-server require TLS; a
	// plaintext client must be refused.
	if plain, err := nats.Connect(b.ClientURL(), nats.NoReconnect()); err == nil {
		plain.Close()
		t.Fatal("plaintext client connected to a TLS-required broker")
	}
}

func TestBrokerTLS_WrongCARefused(t *testing.T) {
	dir, _ := farmCerts(t)
	otherDir, _ := farmCerts(t)
	b := startBrokerTLS(t, brokerTLSOf(dir, false), BrokerAuthConfig{})

	_, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nats.Secure(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    caPool(t, otherDir),
	}))
	if err == nil {
		t.Fatal("client trusting a different CA connected successfully")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error = %v, want a certificate verification failure", err)
	}
}

func TestBrokerTLS_MTLSRequiresClientCert(t *testing.T) {
	dir, ca := farmCerts(t)
	b := startBrokerTLS(t, brokerTLSOf(dir, true), BrokerAuthConfig{})
	pool := caPool(t, dir)

	// Correct server trust but no client certificate: refused.
	if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nats.Secure(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	})); err == nil {
		nc.Close()
		t.Fatal("client without a certificate connected to an mTLS broker")
	}

	// With a farm-issued client certificate: accepted.
	nc, err := nats.Connect(b.ClientURL(), clientCertOption(t, ca, pool, "worker-01"))
	if err != nil {
		t.Fatalf("client with a valid certificate was refused: %v", err)
	}
	nc.Close()
}

func TestBrokerTLS_MTLSLayersOnNkeyRatherThanReplacingIt(t *testing.T) {
	dir, ca := farmCerts(t)
	enrolled, _ := enrolledWorker(t, "worker-01")
	b := startBrokerTLS(
		t,
		brokerTLSOf(dir, true),
		BrokerAuthConfig{Enabled: true, Credentials: []WorkerCredentialRef{enrolled}},
	)
	pool := caPool(t, dir)

	// A valid farm client certificate, but no enrolled nkey. A certificate
	// alone must not be sufficient: mTLS gates the transport, the nkey is
	// still the identity.
	nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), clientCertOption(t, ca, pool, "impostor"))
	if err == nil {
		nc.Close()
		t.Fatal("a farm client certificate alone authenticated; mTLS must layer on the nkey, not replace it")
	}
}

func TestBrokerTLS_OffIsUnchanged(t *testing.T) {
	// The zero-value TLS field is the default path: plaintext, exactly as it
	// behaved before TLS existed.
	b := startBrokerTLS(t, BrokerTLSConfig{}, BrokerAuthConfig{})
	nc, err := nats.Connect(b.ClientURL())
	if err != nil {
		t.Fatalf("plaintext client could not connect to a default broker: %v", err)
	}
	nc.Close()
}
