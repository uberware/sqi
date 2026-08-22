// SPDX-License-Identifier: AGPL-3.0-or-later

package obs_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
	"github.com/uberware/sqi/internal/health"
	workmetrics "github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/obs"
)

// The worker's observability listener serves metrics, health and optionally
// pprof. It defaults to loopback, but an operator scraping it from Prometheus
// has to bind it wider — and until this, there was no way to protect it. It was
// the one listener sqi opens that TLS did not reach, which made the top line of
// docs/tls.md false.

func obsCerts(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	ca, err := certgen.NewCA("worker obs CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"localhost", "127.0.0.1", "::1"}, time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "obs", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}
	return filepath.Join(dir, "obs.crt"), filepath.Join(dir, "obs.key"), filepath.Join(dir, "ca.crt")
}

func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// runObs starts a server and returns its address once it accepts connections.
func runObs(t *testing.T, tlsCfg obs.TLSConfig) string {
	t.Helper()
	addr := freeAddr(t)
	srv := obs.New(addr, false, slog.New(slog.DiscardHandler),
		workmetrics.New(), health.NewRegistry(), tlsCfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Run(ctx) }()
	t.Cleanup(func() {
		// Run blocks on the listener and does not watch ctx; Shutdown is what
		// stops it. Canceling alone would hang here.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		srv.Shutdown(shutCtx)
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("obs server did not stop")
		}
	})

	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for !time.Now().After(deadline) {
		if c, err := dialer.DialContext(context.Background(), "tcp", addr); err == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("obs listener did not come up on %s", addr)
	return ""
}

func get(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return client.Do(req)
}

func TestObs_PlaintextByDefault(t *testing.T) {
	// The default path must be byte-for-byte what it always was.
	addr := runObs(t, obs.TLSConfig{})
	resp, err := get(t, &http.Client{Timeout: 5 * time.Second}, "http://"+addr+"/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over plain HTTP: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestObs_ServesHTTPSWhenEnabled(t *testing.T) {
	certFile, keyFile, caFile := obsCerts(t)
	addr := runObs(t, obs.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile})

	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("ca did not append")
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}

	for _, path := range []string{"/healthz", "/metrics"} {
		resp, err := get(t, client, "https://"+addr+path)
		if err != nil {
			t.Fatalf("GET %s over TLS: %v", path, err)
		}
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, code)
		}
	}

	// And plaintext must no longer be served on that port.
	if r, err := get(t, &http.Client{Timeout: 5 * time.Second}, "http://"+addr+"/healthz"); err == nil {
		code := r.StatusCode
		_ = r.Body.Close()
		if code == http.StatusOK {
			t.Error("plaintext request succeeded against a TLS-enabled obs listener")
		}
	}
}
