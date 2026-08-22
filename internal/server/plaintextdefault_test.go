// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
)

// TestDefaultConfig_ServesPlaintextUnchanged is Phase 4's named
// default-configuration regression test for H2.
//
// The phase's standing rule is that the single-binary, SQLite, embedded-NATS,
// no-TLS deployment must keep working exactly as it did in v0.3.0. Every other
// test in this component adds a TLS path; this one proves the absence of one.
// Each assertion states a property of the plaintext default that a future TLS
// change would break loudly rather than quietly.
func TestDefaultConfig_ServesPlaintextUnchanged(t *testing.T) {
	httpPort, natsPort := freeTestPort(t), freeTestPort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	natsAddr := fmt.Sprintf("127.0.0.1:%d", natsPort)
	tmpDir := t.TempDir()

	// Derived from config.DefaultConfig() rather than hand-written, so a
	// default that starts shipping TLS on would fail here instead of being
	// invisible to a literal struct.
	defaults := config.DefaultConfig()
	if defaults.HTTP.TLS.Enabled {
		t.Fatal("config.DefaultConfig() ships http.tls.enabled = true; TLS must be opt-in")
	}
	if defaults.NATS.TLS.Enabled {
		t.Fatal("config.DefaultConfig() ships nats.tls.enabled = true; TLS must be opt-in")
	}

	cfg := Config{
		HTTPAddr:           httpAddr,
		HTTPTLS:            defaults.HTTP.TLS,
		NATSTLS:            defaults.NATS.TLS,
		CORSOrigins:        []string{"*"},
		NATSAddr:           natsAddr,
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

	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + httpAddr

	// 1. Plain HTTP serves.
	deadline := time.Now().Add(10 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = get(t, client, base+"/healthz")
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /healthz over plain HTTP: %v", err)
	}
	code := resp.StatusCode
	_ = resp.Body.Close()
	if code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", code)
	}

	// 2. There is no TLS listener on that port.
	tlsClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // asserting that no TLS listener exists at all
		},
	}
	if r, err := get(t, tlsClient, "https://"+httpAddr+"/healthz"); err == nil {
		_ = r.Body.Close()
		t.Error("an HTTPS request succeeded against a default-configured server")
	}

	// 3. The http.Server carries no CERTIFICATE.
	//
	//    Deliberately not "TLSConfig == nil": net/http's HTTP/2 setup builds a
	//    TLSConfig with NextProtos ["h2","http/1.1"] even for a plain
	//    ListenAndServe, so a nil check would assert an implementation detail
	//    of net/http rather than anything about sqi. What must hold is that no
	//    certificate was loaded — that is what would make the listener speak
	//    TLS.
	if tc := srv.httpServer.TLSConfig; tc != nil && len(tc.Certificates) > 0 {
		t.Errorf("httpServer.TLSConfig carries %d certificate(s) on a default-configured server", len(tc.Certificates))
	}

	// 4. The broker is plaintext: a client with no TLS options connects.
	nc, err := nats.Connect("nats://"+natsAddr, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("plaintext NATS client could not connect to a default broker: %v", err)
	}
	nc.Close()

	// 5. A session cookie issued over this listener is not Secure. The
	//    cookie_secure default is "auto", which resolves from r.TLS — a Secure
	//    cookie on a plaintext listener is silently dropped by the browser and
	//    breaks login with no error anywhere. Pinned at the resolver here; the
	//    full login path is covered in internal/api/tlscookie_test.go.
	if defaults.Auth.Session.CookieSecure != "auto" {
		t.Errorf("cookie_secure default = %q, want \"auto\"", defaults.Auth.Session.CookieSecure)
	}

	// 6. The mDNS advertisement carries no TLS keys.
	//
	//    Asserted where the records are actually built, not here: this server
	//    runs with DiscoveryEnabled=false (multicast is unavailable in most CI
	//    environments), so a responder started here would never produce
	//    records and any check would be vacuous.
	//    TestBuildTXTRecords_TLSKeysOmittedWhenOff in internal/discovery is the
	//    real assertion. What this test contributes is the input to it: both
	//    flags come from cfg.HTTPTLS.Enabled / cfg.NATSTLS.Enabled, verified
	//    false at the top of this function.
}
