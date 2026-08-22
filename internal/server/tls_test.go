// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
)

// writeServerCerts generates a farm CA and a server keypair covering
// localhost/127.0.0.1 into a temp dir. validFor is threaded through so a
// caller can mint a certificate that is expiring soon, or already expired.
func writeServerCerts(t *testing.T, validFor time.Duration) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	ca, err := certgen.NewCA("test CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"localhost", "127.0.0.1", "::1"}, validFor)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}
	return filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), filepath.Join(dir, "ca.crt")
}

// freeTestPort returns a port that was free a moment ago.
func freeTestPort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not a *net.TCPAddr", l.Addr())
	}
	return addr.Port
}

// get performs a GET and returns the response. It exists because the lint
// config forbids http.Client.Get (noctx) in favor of an explicit request.
func get(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	return client.Do(req)
}

// startTLSServer boots a real Server with TLS enabled and returns its
// https:// base URL plus the CA that signed its certificate.
func startTLSServer(t *testing.T, certFile, keyFile string) string {
	t.Helper()
	httpPort, natsPort := freeTestPort(t), freeTestPort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	tmpDir := t.TempDir()

	cfg := Config{
		HTTPAddr:           httpAddr,
		HTTPTLS:            config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile},
		CORSOrigins:        []string{"*"},
		NATSAddr:           fmt.Sprintf("127.0.0.1:%d", natsPort),
		NATSDataDir:        tmpDir + "/nats",
		NATSMaxStoreMB:     64,
		SQLitePath:         tmpDir + "/test.db",
		CheckpointInterval: time.Minute,
		Scheduler: scheduler.Config{
			AssignInterval:         100 * time.Millisecond,
			AssignBatchSize:        10,
			AssignWorkers:          2,
			WorkerTimeout:          30 * time.Second,
			HeartbeatSweepInterval: 15 * time.Second,
		},
		DiscoveryEnabled: false,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(cfg, logger, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down within 30s")
		}
	})

	select {
	case err := <-done:
		t.Fatalf("server exited during startup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	base := "https://" + httpAddr
	// Wait for the TLS listener to accept.
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for !time.Now().After(deadline) {
		c, err := dialer.DialContext(context.Background(), "tcp", httpAddr)
		if err == nil {
			_ = c.Close()
			return base
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("TLS listener did not come up on %s within 10s", httpAddr)
	return ""
}

// caClient returns an HTTP client trusting only caFile.
func caClient(t *testing.T, caFile string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("CA did not append to pool")
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

func TestServer_ServesHTTPSWithFarmCA(t *testing.T) {
	certFile, keyFile, caFile := writeServerCerts(t, 365*24*time.Hour)
	base := startTLSServer(t, certFile, keyFile)

	resp, err := get(t, caClient(t, caFile), base+"/healthz")
	if err != nil {
		t.Fatalf("GET %s/healthz over TLS: %v", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestServer_HTTPSRejectedByUntrustingClient(t *testing.T) {
	certFile, keyFile, _ := writeServerCerts(t, 365*24*time.Hour)
	base := startTLSServer(t, certFile, keyFile)

	// System roots only: the farm CA is not among them, so this must fail.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := get(t, client, base+"/healthz")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request succeeded with the system root pool; the server is not presenting the farm certificate")
	}
	// The failure must be certificate verification, not a transport error that
	// would also "pass" this test for the wrong reason.
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error = %v, want a certificate verification failure", err)
	}
}

func TestServer_PlaintextRequestToTLSPortIsRefusedNotHung(t *testing.T) {
	certFile, keyFile, _ := writeServerCerts(t, 365*24*time.Hour)
	base := startTLSServer(t, certFile, keyFile)

	// Same address, http:// scheme. Go's server answers this with a 400 and a
	// readable body rather than hanging; the timeout makes a regression to a
	// hang fail fast instead of stalling CI.
	plain := "http://" + base[len("https://"):]
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := get(t, client, plain+"/healthz")
	if err != nil {
		t.Fatalf("plaintext request to the TLS port errored instead of returning a status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServer_ReadyzUnderTLSInsideHealthBudget(t *testing.T) {
	certFile, keyFile, caFile := writeServerCerts(t, 365*24*time.Hour)
	base := startTLSServer(t, certFile, keyFile)
	client := caClient(t, caFile)

	// /readyz gates on SQLite and NATS. internal/health budgets 5s; the TLS
	// handshake must not push it over.
	deadline := time.Now().Add(15 * time.Second)
	for {
		start := time.Now()
		resp, err := get(t, client, base+"/readyz")
		elapsed := time.Since(start)
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code == http.StatusOK {
				if elapsed >= 5*time.Second {
					t.Errorf("/readyz took %s over TLS, want well inside the 5s health budget", elapsed)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never returned 200 over TLS within 15s (last err: %v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestTLSStartupWarnings(t *testing.T) {
	expiringCert, expiringKey, _ := writeServerCerts(t, 10*24*time.Hour) // inside the 30-day window
	goodCert, goodKey, _ := writeServerCerts(t, 365*24*time.Hour)

	tests := []struct {
		name     string
		httpTLS  config.TLSConfig
		natsTLS  config.NATSTLSConfig
		wantWarn string
	}{
		{
			name:     "http certificates configured but tls disabled",
			httpTLS:  config.TLSConfig{Enabled: false, CertFile: goodCert, KeyFile: goodKey},
			wantWarn: "http.tls.enabled",
		},
		{
			name:     "nats certificates configured but tls disabled",
			natsTLS:  config.NATSTLSConfig{Enabled: false, CertFile: goodCert, KeyFile: goodKey},
			wantWarn: "nats.tls.enabled",
		},
		{
			name:     "http certificate expiring soon",
			httpTLS:  config.TLSConfig{Enabled: true, CertFile: expiringCert, KeyFile: expiringKey},
			wantWarn: "expires soon",
		},
		{
			name:     "nats certificate expiring soon",
			natsTLS:  config.NATSTLSConfig{Enabled: true, CertFile: expiringCert, KeyFile: expiringKey},
			wantWarn: "expires soon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			warnTLSConfig(context.Background(), logger, tt.httpTLS, tt.natsTLS)
			if !strings.Contains(buf.String(), tt.wantWarn) {
				t.Errorf("warnings = %q, want one containing %q", buf.String(), tt.wantWarn)
			}
		})
	}
}

func TestTLSStartupWarnings_SilentOnDefaults(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	warnTLSConfig(context.Background(), logger, config.TLSConfig{}, config.NATSTLSConfig{})
	if buf.Len() != 0 {
		t.Errorf("default configuration produced warnings: %q", buf.String())
	}
}
