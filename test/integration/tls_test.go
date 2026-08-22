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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	dir := t.TempDir()
	ca, err := certgen.NewCA("integration farm CA", 10*365*24*time.Hour)
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
	return dir
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

	return startBrokerAuthServer(t, dbPath, func(cfg *server.Config) {
		cfg.HTTPTLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSTLS = config.NATSTLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
		cfg.NATSAuthEnrollmentEndpointEnabled = true
	})
}

// waitForOnlineWorkerTLS polls GET /api/v1/workers over HTTPS until a worker
// in farmID reports "online", returning its ID. Returns "" on timeout.
func waitForOnlineWorkerTLS(t *testing.T, client *http.Client, httpAddr, farmID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"https://"+httpAddr+"/api/v1/workers", nil)
		if err != nil {
			t.Fatalf("build workers request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			var body struct {
				Items []struct {
					ID     string `json:"id"`
					FarmID string `json:"farm_id"`
					Status string `json:"status"`
				} `json:"items"`
			}
			decErr := json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if decErr == nil {
				for _, w := range body.Items {
					if w.FarmID == farmID && w.Status == "online" {
						return w.ID
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}

// seedFarmAndQueueTLS creates a farm and a queue over HTTPS and returns their
// server-generated IDs. The plaintext seedFarmAndQueue in e2e_test.go goes
// through apiURL/mustDoJSON, both of which hardcode http://.
func seedFarmAndQueueTLS(t *testing.T, client *http.Client, httpAddr string) (farmID, queueID string) {
	t.Helper()

	post := func(path string, body any) string {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s body: %v", path, err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://"+httpAddr+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("build %s request: %v", path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s over HTTPS: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST %s status = %d, want 201", path, resp.StatusCode)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		if out.ID == "" {
			t.Fatalf("POST %s returned an empty id", path)
		}
		return out.ID
	}

	farmID = post("/api/v1/farms", map[string]any{"name": "TLS Farm"})
	queueID = post("/api/v1/queues", map[string]any{"farm_id": farmID, "name": "TLS Queue"})
	return farmID, queueID
}

func TestTLSEndToEnd(t *testing.T) {
	dir := tlsMaterial(t)
	dbPath := filepath.Join(t.TempDir(), "sqi.db")
	joinToken := seedJoinToken(t, dbPath, "tls-e2e")
	ts := startTLSServer(t, dbPath, dir)
	client := tlsClient(t, dir)

	// The API really is HTTPS, verified against the farm CA.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://"+ts.HTTPAddr+"/readyz", nil)
	if err != nil {
		t.Fatalf("build readyz request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz over HTTPS: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz status = %d, want 200", resp.StatusCode)
	}

	farmID, queueID := seedFarmAndQueueTLS(t, client, ts.HTTPAddr)
	caFile := filepath.Join(dir, "ca.crt")
	startRealWorkerNoWait(t, ts, farmID, queueID, []string{
		// Broker TLS: this URL overrides the harness default because exec
		// resolves duplicate environment entries to the last one.
		"SQI_WORKER_NATS_URL=nats://" + ts.NATSAddr,
		"SQI_WORKER_NATS_TLS_CA_FILE=" + caFile,
		// Enrollment over HTTPS — the bootstrap-critical half.
		"SQI_WORKER_NATS_SERVER_URL=https://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_SERVER_TLS_CA_FILE=" + caFile,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	})

	if id := waitForOnlineWorkerTLS(t, client, ts.HTTPAddr, farmID, 45*time.Second); id == "" {
		t.Fatal("worker never came online over TLS: enrollment over HTTPS or the TLS broker connection failed")
	}
}

func TestTLSWorkerWithoutCAIsRefused(t *testing.T) {
	dir := tlsMaterial(t)
	dbPath := filepath.Join(t.TempDir(), "sqi.db")
	joinToken := seedJoinToken(t, dbPath, "tls-nocert")
	ts := startTLSServer(t, dbPath, dir)
	client := tlsClient(t, dir)

	farmID, queueID := seedFarmAndQueueTLS(t, client, ts.HTTPAddr)
	// No CA configured anywhere: the worker can neither verify the server's
	// certificate for enrollment nor speak TLS to the broker.
	startRealWorkerNoWait(t, ts, farmID, queueID, []string{
		"SQI_WORKER_NATS_URL=nats://" + ts.NATSAddr,
		"SQI_WORKER_NATS_SERVER_URL=https://" + ts.HTTPAddr,
		"SQI_WORKER_NATS_JOIN_TOKEN=" + joinToken,
	})

	if id := waitForOnlineWorkerTLS(t, client, ts.HTTPAddr, farmID, 10*time.Second); id != "" {
		t.Fatalf("worker %s registered without any CA configured; TLS is not actually being enforced", id)
	}
}

// readyzTLSClient builds a client trusting the CA that sits beside certFile.
// It is used by startBrokerAuthServer's readiness probe when the server under
// test is TLS-terminated.
func readyzTLSClient(t *testing.T, certFile string) *http.Client {
	t.Helper()
	return tlsClient(t, filepath.Dir(certFile))
}
