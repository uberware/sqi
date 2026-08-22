// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// End-to-end coverage of the mDNS discovery path over REAL multicast.
//
// Everything else in this suite sets DiscoveryEnabled: false, so until these
// tests existed the discovery path had unit coverage on both halves and
// nothing joining them: the server advertised TXT records that no test ever
// received, and the worker parsed TXT records that no test ever sent. The
// specific gap that mattered was the TLS records — the server advertised
// nats_tls=1 and, for a while, nothing on the worker read it at all.
//
// These tests use real multicast on a real interface. See multicast_test.go
// for the preflight and for why a foreign sqi-server on the network makes the
// real-binary test refuse to run.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	serverdiscovery "github.com/uberware/sqi/internal/discovery"
	"github.com/uberware/sqi/internal/server"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	workerdiscovery "github.com/uberware/sqi/internal/worker/discovery"
)

// advertise starts a real mDNS responder describing a server with the given
// TLS posture, and returns its instance name.
func advertise(t *testing.T, httpTLS, natsTLS bool) string {
	t.Helper()
	instance := instanceName(t, "sqi-disco")
	resp, err := serverdiscovery.New(serverdiscovery.Config{
		Enabled:      true,
		InstanceName: instance,
		HTTPAddr:     "127.0.0.1:18080",
		NATSAddr:     "127.0.0.1:14222",
		HTTPTLS:      httpTLS,
		NATSTLS:      natsTLS,
		// Loopback only: a test must not announce a service on the network it
		// happens to be running on.
		Interfaces: loopbackIfaces(),
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("responder New: %v", err)
	}
	if err := resp.Start(context.Background()); err != nil {
		t.Fatalf("responder Start: %v", err)
	}
	t.Cleanup(resp.Shutdown)
	return instance
}

// browseOnce browses for this test's advertisement and returns the result.
func browseOnce(t *testing.T) workerdiscovery.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	found, err := workerdiscovery.Browse(ctx, 10*time.Second, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	return found
}

// TestDiscovery_TLSRecordsCrossTheWire is the join the unit tests could not
// make: the server's own config decides the TXT records, they travel over
// real multicast, and the worker's parser reads them back.
func TestDiscovery_TLSRecordsCrossTheWire(t *testing.T) {
	requireMulticast(t)
	noForeignServer(t)

	tests := []struct {
		name             string
		httpTLS, natsTLS bool
	}{
		{"plaintext farm", false, false},
		{"tls on both listeners", true, true},
		{"broker tls only", false, true},
		{"api tls only", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := advertise(t, tt.httpTLS, tt.natsTLS)
			found := browseOnce(t)

			// Guard against having discovered something else entirely: a
			// wrong-server result would otherwise look like a parsing bug.
			if found.InstanceName != instance {
				t.Fatalf("discovered %q, want this test's %q", found.InstanceName, instance)
			}
			if found.NATSTLS != tt.natsTLS {
				t.Errorf("NATSTLS = %v, want %v (nats_tls TXT record did not survive the wire)", found.NATSTLS, tt.natsTLS)
			}
			if found.HTTPTLS != tt.httpTLS {
				t.Errorf("HTTPTLS = %v, want %v (tls TXT record did not survive the wire)", found.HTTPTLS, tt.httpTLS)
			}
			if !strings.HasPrefix(found.NATSURL, "nats://") {
				t.Errorf("NATSURL = %q, want a nats:// URL", found.NATSURL)
			}
		})
	}
}

// TestDiscovery_AdvertisedBrokerTLSReachesWorkerConfig runs the whole consumer
// chain over real multicast: server config → TXT records → wire → parse →
// the worker's TLS decision.
//
// This is the regression that matters. The records were advertised for a
// while with nothing reading them, so a worker discovering a TLS farm
// attempted plaintext and failed with an error naming neither the cause nor
// the setting that fixes it.
func TestDiscovery_AdvertisedBrokerTLSReachesWorkerConfig(t *testing.T) {
	requireMulticast(t)
	noForeignServer(t)

	t.Run("auto enables TLS from the advertisement alone", func(t *testing.T) {
		advertise(t, true, true)
		found := browseOnce(t)

		// A worker with NOTHING configured: no URL, no CA, no tls_enabled.
		// Only the advertisement can turn TLS on here.
		cfg := workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto}
		if cfg.UseTLS(false) {
			t.Fatal("fixture is wrong: this config already wanted TLS before discovery")
		}
		workerdiscovery.ApplyTLS(context.Background(), &cfg, found, slog.New(slog.DiscardHandler))
		if !cfg.UseTLS(false) {
			t.Error("a discovered TLS-required broker did not enable TLS under auto")
		}
	})

	t.Run("plaintext farm leaves the worker plaintext", func(t *testing.T) {
		advertise(t, false, false)
		found := browseOnce(t)

		cfg := workerconfig.NATSConfig{TLSEnabled: workerconfig.TLSAuto}
		workerdiscovery.ApplyTLS(context.Background(), &cfg, found, slog.New(slog.DiscardHandler))
		if cfg.UseTLS(false) {
			t.Error("a plaintext farm turned TLS on; the default deployment would break")
		}
	})
}

// TestDiscovery_RealBinaryFindsItsServerOverMDNS is the full stack: a real
// sqi-server advertising over mDNS with TLS on both listeners, and a real
// sqi-worker subprocess given NO nats.url at all, which has to discover the
// server, act on nats_tls, and register.
func TestDiscovery_RealBinaryFindsItsServerOverMDNS(t *testing.T) {
	// This is the ONE test here that opens a listener beyond loopback, so it
	// runs only when discovery testing was asked for explicitly — `make
	// test-discovery` or the CI job — never as a side effect of `make
	// test-integration`.
	//
	// It cannot avoid the listener. The advertisement carries this machine's
	// HOSTNAME (entryToResult prefers entry.HostName), so the worker dials that
	// name whatever interface the announcement went out on, and a
	// loopback-bound broker is unreachable there. Verified by trying: bound to
	// 127.0.0.1 the worker discovers the server and then fails with "no servers
	// available". Changing the product to advertise a loopback literal would be
	// bending production behavior to suit a test.
	if !multicastRequired() {
		t.Skip("binds the test broker to all interfaces; run `make test-discovery` to include it")
	}
	requireMulticast(t)
	noForeignServer(t)

	// Two consequences of the advertisement carrying a hostname, both of them
	// what a real deployment does anyway:
	//
	//  1. The broker must bind all interfaces, per the note above.
	//  2. The certificate must cover that hostname, or the TLS handshake fails
	//     on a SAN mismatch. `sqi-server tls init` includes os.Hostname() by
	//     default for exactly this reason.
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	host = strings.TrimSuffix(host, ".local")
	dir := tlsMaterialFor(t, []string{host, host + ".local", "localhost", "127.0.0.1", "::1"})

	dbPath := filepath.Join(t.TempDir(), "sqi.db")
	joinToken := seedJoinToken(t, dbPath, "discovery-e2e")

	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	instance := instanceName(t, "sqi-e2e")

	ts := startBrokerAuthServer(t, dbPath, func(cfg *server.Config) {
		cfg.HTTPTLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSTLS = config.NATSTLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSAuthEnrollmentEndpointEnabled = true
		// Reachable at the advertised hostname, per (1) above.
		cfg.NATSAddr = strings.Replace(cfg.NATSAddr, "127.0.0.1", "0.0.0.0", 1)
		// The point of this test: advertise, so the worker can find us.
		cfg.DiscoveryEnabled = true
		cfg.DiscoveryInstanceName = instance
		cfg.DiscoveryInterfaces = loopbackIfaces()
	})

	// Only now, with the server advertising: the mDNS name is answered by the
	// responder, so this is the first moment the check means anything.
	requireLocalHostnameResolves(t)

	farmID, queueID := seedFarmAndQueue(t, ts)
	caFile := filepath.Join(dir, "ca.crt")

	// No SQI_WORKER_NATS_URL: the worker must find the broker over mDNS, and
	// the discovered nats_tls record is what tells it the transport.
	startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		// Blank out the harness default so the worker has no URL to fall back
		// on: applyNATSEnv ignores an empty value, so nats.url stays unset and
		// mDNS is the only way this worker can find its broker.
		"SQI_WORKER_NATS_URL=",
		"SQI_WORKER_DISCOVERY_ENABLE_MDNS=true",
		"SQI_WORKER_DISCOVERY_MDNS_TIMEOUT=15s",
		"SQI_WORKER_NATS_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_SERVER_URL=https://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_SERVER_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	}, true)

	if id := findOnlineWorker(t, ts, farmID, 60*time.Second); id == "" {
		t.Fatal("worker never came online: mDNS discovery, TLS from the advertisement, or enrollment failed")
	}
}
