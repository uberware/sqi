// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
)

func TestBrokerInProcess_ServerReachesJetStreamUnderTLSAndAuth(t *testing.T) {
	dir, _ := farmCerts(t)
	enrolled, _ := enrolledWorker(t, "worker-01")
	b := startBrokerTLS(
		t,
		brokerTLSOf(dir, false),
		BrokerAuthConfig{Enabled: true, Credentials: []WorkerCredentialRef{enrolled}},
	)

	// The server's own client carries no TLS configuration at all: the
	// connection is an in-process pipe, which nats-server exempts from the TLS
	// requirement, so there is nothing to trust and nothing to encrypt.
	c, err := b.NewClient()
	if err != nil {
		t.Fatalf("NewClient under TLS + auth: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Check(ctx); err != nil {
		t.Errorf("broker health check failed with TLS and auth on: %v", err)
	}
}

func TestBrokerInProcess_NetworkClientsStillNeedCertificates(t *testing.T) {
	dir, _ := farmCerts(t)
	b := startBrokerTLS(t, brokerTLSOf(dir, true), BrokerAuthConfig{})

	// The server's own connection is in-process and therefore plaintext, which
	// bypasses the client-certificate requirement by never doing a handshake.
	// A real network client must not get the same pass.
	nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nats.Secure(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    caPool(t, dir),
	}))
	if err == nil {
		nc.Close()
		t.Fatal("a TCP client without a certificate connected to an mTLS broker")
	}
}

func TestBrokerInProcess_PlaintextStillRefusedForNetworkClients(t *testing.T) {
	dir, _ := farmCerts(t)
	b := startBrokerTLS(t, brokerTLSOf(dir, false), BrokerAuthConfig{})

	if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect()); err == nil {
		nc.Close()
		t.Fatal("plaintext TCP client connected to a TLS-required broker")
	}
}
