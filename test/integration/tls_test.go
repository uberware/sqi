// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// TestTLSEndToEnd proves the whole TLS path with real binaries and real
// generated certificates: sqi-server serves HTTPS, its embedded broker
// requires TLS, and a real sqi-worker subprocess enrolls over HTTPS with a
// join token and then connects to the TLS broker.
//
// Enrollment is the part that unit tests cannot prove is sufficient. It runs
// over REST BEFORE the worker holds any broker credential, so it is the first
// thing a farm does and the first thing that breaks if the worker cannot
// trust the server's certificate.
//
// The counterpart is TestTLSWorkerWithoutCAIsRefused: the same server, a
// worker given no CA, which must never register. Without it this suite could
// pass while TLS was quietly optional.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
)

// tlsMaterial generates a farm CA and a loopback server certificate into a
// temp directory, returning the directory. It uses the same internal/certgen
// package that `sqi-server tls init` uses, so this exercises the shipped
// generator rather than a test-only substitute.
func tlsMaterial(t *testing.T) string {
	t.Helper()
	return tlsMaterialFor(t, []string{"localhost", "127.0.0.1", "::1"})
}

// tlsMaterialFor is [tlsMaterial] with an explicit SAN list, for a server that
// has to be reachable by a name other than loopback — see the mDNS discovery
// tests, where the advertisement carries this machine's hostname.
func tlsMaterialFor(t *testing.T, sans []string) string {
	t.Helper()
	dir := t.TempDir()
	ca, err := certgen.NewCA("integration farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}
	leaf, err := ca.NewServerCert(sans, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}
	return dir
}

// tlsMaterialWithClient generates the farm CA, a server certificate and a
// worker CLIENT certificate in one pass, returning the directory and the
// client keypair's paths.
//
// It mints all three together because `sqi-server tls init` refuses to
// overwrite an existing CA, so a second invocation cannot issue a client
// certificate against a CA it already wrote.
func tlsMaterialWithClient(t *testing.T, workerID string) (dir, certFile, keyFile string) {
	t.Helper()
	dir = t.TempDir()
	ca, err := certgen.NewCA("integration farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}
	server, err := ca.NewServerCert([]string{"localhost", "127.0.0.1", "::1"}, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", server); err != nil {
		t.Fatalf("WriteLeaf(server): %v", err)
	}
	client, err := ca.NewClientCert(workerID, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewClientCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "client-"+workerID, client); err != nil {
		t.Fatalf("WriteLeaf(client): %v", err)
	}
	return dir, filepath.Join(dir, "client-"+workerID+".crt"), filepath.Join(dir, "client-"+workerID+".key")
}

// tlsClient returns an HTTP client trusting only the CA in dir.
func tlsClient(t *testing.T, dir string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("ca.crt did not append to pool")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// startTLSServer boots a full sqi-server with HTTPS, broker TLS, broker auth
// and the REST enrollment endpoint all enabled, against certificates in dir.
func startTLSServer(t *testing.T, dbPath, dir string) *testServer {
	t.Helper()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")

	ts := startBrokerAuthServer(t, dbPath, func(cfg *server.Config) {
		cfg.HTTPTLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSTLS = config.NATSTLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSAuthEnrollmentEndpointEnabled = true
	})
	// startBrokerAuthServer stamps ts.Scheme/ts.Client from cfg.HTTPTLS, so
	// every shared helper already speaks https here.
	return ts
}

func TestTLSEndToEnd(t *testing.T) {
	dir := tlsMaterial(t)
	dbPath := filepath.Join(t.TempDir(), "sqi.db")
	joinToken := seedJoinToken(t, dbPath, "tls-e2e")
	ts := startTLSServer(t, dbPath, dir)

	// The API really is HTTPS, verified against the farm CA.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://"+ts.HTTPAddr+"/readyz", nil)
	if err != nil {
		t.Fatalf("build readyz request: %v", err)
	}
	resp, err := ts.Client.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz over HTTPS: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz status = %d, want 200", resp.StatusCode)
	}

	farmID, queueID := seedFarmAndQueue(t, ts)
	caFile := filepath.Join(dir, "ca.crt")
	startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		// Broker TLS: this URL overrides the harness default because exec
		// resolves duplicate environment entries to the last one.
		"SQI_WORKER_NATS_URL=nats://" + ts.NATSAddr,
		"SQI_WORKER_NATS_TLS_CA_FILE=" + caFile,
		// Enrollment over HTTPS — the bootstrap-critical half.
		"SQI_WORKER_NATS_SERVER_URL=https://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_SERVER_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	}, true)

	if id := findOnlineWorker(t, ts, farmID, 45*time.Second); id == "" {
		t.Fatal("worker never came online over TLS: enrollment over HTTPS or the TLS broker connection failed")
	}
}

func TestTLSWorkerWithoutCAIsRefused(t *testing.T) {
	dir := tlsMaterial(t)
	dbPath := filepath.Join(t.TempDir(), "sqi.db")
	joinToken := seedJoinToken(t, dbPath, "tls-nocert")
	ts := startTLSServer(t, dbPath, dir)

	farmID, queueID := seedFarmAndQueue(t, ts)
	// No CA configured anywhere: the worker can neither verify the server's
	// certificate for enrollment nor speak TLS to the broker.
	startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_NATS_URL=nats://" + ts.NATSAddr,
		"SQI_WORKER_NATS_SERVER_URL=https://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	}, true)

	if id := findOnlineWorker(t, ts, farmID, 10*time.Second); id != "" {
		t.Fatalf("worker %s registered without any CA configured; TLS is not actually being enforced", id)
	}
}

// TestTLSMutualAuthEndToEnd covers the mTLS flow docs/tls.md promotes, with a
// real worker binary: `sqi-server tls init --client <id>` issues a client
// keypair, the worker presents it via nats.tls_cert_file/tls_key_file, and the
// broker accepts it.
//
// The unit tests in internal/bus hand-build the client tls.Config, so they
// never traverse natsclient.buildTLSOptions — which is the code an operator's
// configuration actually reaches, and where a wrong ExtKeyUsage on the
// generated certificate would bite.
func TestTLSMutualAuthEndToEnd(t *testing.T) {
	dir, clientCert, clientKey := tlsMaterialWithClient(t, "render-01")

	dbPath := filepath.Join(t.TempDir(), "sqi.db")
	joinToken := seedJoinToken(t, dbPath, "mtls-e2e")

	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	caFile := filepath.Join(dir, "ca.crt")

	ts := startBrokerAuthServer(t, dbPath, func(cfg *server.Config) {
		cfg.HTTPTLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSTLS = config.NATSTLSConfig{
			Enabled: true, CertFile: certFile, KeyFile: keyFile,
			// Require a client certificate from every worker.
			ClientCAFile: caFile,
		}
		cfg.NATSAuthEnrollmentEndpointEnabled = true
	})

	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_NATS_URL=nats://" + ts.NATSAddr,
		"SQI_WORKER_NATS_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_TLS_CERT_FILE=" + clientCert,
		"SQI_WORKER_NATS_TLS_KEY_FILE=" + clientKey,
		"SQI_WORKER_NATS_SERVER_URL=https://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_SERVER_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	}, true)

	if id := findOnlineWorker(t, ts, farmID, 45*time.Second); id == "" {
		t.Fatal("worker never came online under mutual TLS: the generated client certificate was not accepted")
	}
}

// TestTLSMutualAuthRefusesWorkerWithoutCertificate is the negative half: the
// same broker must reject a worker that presents no client certificate.
// Without it, a broker that silently stopped requiring one would still pass
// the test above.
func TestTLSMutualAuthRefusesWorkerWithoutCertificate(t *testing.T) {
	dir := tlsMaterial(t)
	dbPath := filepath.Join(t.TempDir(), "sqi.db")
	joinToken := seedJoinToken(t, dbPath, "mtls-negative")

	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	caFile := filepath.Join(dir, "ca.crt")

	ts := startBrokerAuthServer(t, dbPath, func(cfg *server.Config) {
		cfg.HTTPTLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSTLS = config.NATSTLSConfig{
			Enabled: true, CertFile: certFile, KeyFile: keyFile, ClientCAFile: caFile,
		}
		cfg.NATSAuthEnrollmentEndpointEnabled = true
	})

	farmID, queueID := seedFarmAndQueue(t, ts)
	// Correct CA, correct join token, no client certificate.
	startRealWorkerNoWait(t, ts, farmID, queueID, nil, []string{
		"SQI_WORKER_NATS_URL=nats://" + ts.NATSAddr,
		"SQI_WORKER_NATS_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_SERVER_URL=https://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_SERVER_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	}, true)

	if id := findOnlineWorker(t, ts, farmID, 10*time.Second); id != "" {
		t.Fatalf("worker %s registered without a client certificate against an mTLS broker", id)
	}
}
