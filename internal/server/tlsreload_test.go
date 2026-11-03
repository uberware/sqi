// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store/fake"
)

// TestRevokeWorker_TLSSurvivesRevocation pins a behavior that lives only
// where broker authentication meets TLS: revoking a worker through the
// server must leave the broker's listener requiring TLS.
func TestRevokeWorker_TLSSurvivesRevocation(t *testing.T) {
	certFile, keyFile, caFile := writeServerCerts(t, 365*24*time.Hour)
	st := fake.New()
	refA, seedA := enrolledCredential(t, st, "worker-a")
	refB, _ := enrolledCredential(t, st, "worker-b")

	broker := startTestBroker(t, st, func(c *bus.EmbeddedConfig) {
		c.TLS = bus.BrokerTLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	})
	s := &Server{cfg: brokerAuthConfig(true), store: st, broker: broker, logger: testLogger()}
	pool := caPool(t, caFile)

	secure := nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool})

	// A connects over TLS before the revocation.
	ncA, err := nats.Connect(broker.ClientURL(), secure, nkeyOption(t, seedA, refA.PublicKey), nats.NoReconnect())
	if err != nil {
		t.Fatalf("worker A could not connect over TLS: %v", err)
	}
	ncA.Close()

	// Revoke B.
	if err := s.RevokeWorker(context.Background(), refB.WorkerID); err != nil {
		t.Fatalf("RevokeWorker: %v", err)
	}

	// TLS is still required after the revocation.
	//
	// The plaintext client carries credential A's valid nkey. Without it the
	// broker refuses the connection for want of authentication whatever the TLS
	// state, and the check cannot tell "TLS still required" from "auth still
	// required". With a valid credential, refusal can only mean TLS.
	plain, err := nats.Connect(broker.ClientURL(), nats.NoReconnect(),
		nkeyOption(t, seedA, refA.PublicKey))
	if err == nil {
		plain.Close()
		t.Fatal("a plaintext client with a valid credential connected after a revocation; broker TLS was dropped")
	}
	if !strings.Contains(err.Error(), "secure") && !strings.Contains(err.Error(), "TLS") && !strings.Contains(err.Error(), "tls") {
		t.Errorf("plaintext client was refused for the wrong reason: %v — this assertion only means anything if TLS is what refused it", err)
	}

	// And A can still connect over TLS afterwards.
	ncA2, err := nats.Connect(broker.ClientURL(), secure, nkeyOption(t, seedA, refA.PublicKey), nats.NoReconnect())
	if err != nil {
		t.Fatalf("worker A could not reconnect over TLS after the revocation: %v", err)
	}
	ncA2.Close()
}
