// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/config"
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

	// Same boot helper the TLS tests use, so the only difference between this
	// test and those is the TLS configuration itself.
	srv, base := startTestServer(t, "http", func(cfg *Config) {
		cfg.HTTPTLS = defaults.HTTP.TLS
		cfg.NATSTLS = defaults.NATS.TLS
	})
	httpAddr := strings.TrimPrefix(base, "http://")
	natsAddr := srv.cfg.NATSAddr

	client := &http.Client{Timeout: 5 * time.Second}

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
			// InsecureSkipVerify is deliberate: the assertion is that no TLS
			// listener exists at all, so verification must not be what fails.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
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

	// 5. Session cookies: NOT asserted here.
	//
	//    The default is cookie_secure "auto", which resolves from r.TLS, so on
	//    this plaintext listener a cookie must not be Secure. Checking the
	//    default string here would assert a constant, not a behavior;
	//    TestSessionCookie_NotSecureOnPlaintext in internal/api drives an actual
	//    login over an actual plaintext listener and inspects the real cookie.

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
