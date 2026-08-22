// SPDX-License-Identifier: AGPL-3.0-or-later

package enroll_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
	"github.com/uberware/sqi/internal/worker/enroll"
)

// enrollCerts generates a farm CA and a server keypair covering loopback,
// writes them into a temp dir, and returns (dir, tls.Certificate).
func enrollCerts(t *testing.T) (dir string, pair tls.Certificate) {
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
	pair, err = tls.X509KeyPair(leaf.CertPEM, leaf.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return dir, pair
}

// enrollServer starts an HTTPS server presenting pair that answers the
// enrollment endpoint with 201.
func enrollServer(t *testing.T, pair tls.Certificate) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workers/enroll", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // draining a request body in a test fixture
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "enrolled"}) //nolint:errcheck // test fixture response
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestEnsureCredential_HTTPSWithFarmCA(t *testing.T) {
	dir, pair := enrollCerts(t)
	srv := enrollServer(t, pair)
	credFile := filepath.Join(t.TempDir(), "worker.nk")

	seed, pub, err := enroll.EnsureCredential(context.Background(), enroll.Config{
		WorkerID:       "worker-01",
		CredentialFile: credFile,
		JoinToken:      "tok",
		ServerURL:      srv.URL,
		TLSCAFile:      filepath.Join(dir, "ca.crt"),
	}, testLogger())
	if err != nil {
		t.Fatalf("EnsureCredential over HTTPS with the farm CA: %v", err)
	}
	if len(seed) == 0 || pub == "" {
		t.Fatal("enrollment returned an empty credential")
	}
	if _, err := os.Stat(credFile); err != nil {
		t.Errorf("credential file was not written: %v", err)
	}
}

func TestEnsureCredential_HTTPSWrongCAFails(t *testing.T) {
	_, pair := enrollCerts(t)
	otherDir, _ := enrollCerts(t) // a different CA
	srv := enrollServer(t, pair)
	credFile := filepath.Join(t.TempDir(), "worker.nk")

	_, _, err := enroll.EnsureCredential(context.Background(), enroll.Config{
		WorkerID:       "worker-01",
		CredentialFile: credFile,
		JoinToken:      "tok",
		ServerURL:      srv.URL,
		TLSCAFile:      filepath.Join(otherDir, "ca.crt"),
	}, testLogger())
	if err == nil {
		t.Fatal("enrollment succeeded against a server signed by an untrusted CA")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error = %v, want a certificate verification failure", err)
	}
	// A failed enrollment must never leave a seed behind that a later boot
	// would silently reuse.
	if _, statErr := os.Stat(credFile); statErr == nil {
		t.Error("a credential file was written despite the enrollment failing")
	}
}

func TestEnsureCredential_HTTPSInsecureSkipVerify(t *testing.T) {
	_, pair := enrollCerts(t)
	otherDir, _ := enrollCerts(t)
	srv := enrollServer(t, pair)
	credFile := filepath.Join(t.TempDir(), "worker.nk")

	_, _, err := enroll.EnsureCredential(context.Background(), enroll.Config{
		WorkerID:           "worker-01",
		CredentialFile:     credFile,
		JoinToken:          "tok",
		ServerURL:          srv.URL,
		TLSCAFile:          filepath.Join(otherDir, "ca.crt"),
		InsecureSkipVerify: true,
	}, testLogger())
	if err != nil {
		t.Fatalf("EnsureCredential with InsecureSkipVerify: %v", err)
	}
}

func TestEnsureCredential_BadCAFileIsReported(t *testing.T) {
	_, pair := enrollCerts(t)
	srv := enrollServer(t, pair)
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := enroll.EnsureCredential(context.Background(), enroll.Config{
		WorkerID:       "worker-01",
		CredentialFile: filepath.Join(t.TempDir(), "worker.nk"),
		JoinToken:      "tok",
		ServerURL:      srv.URL,
		TLSCAFile:      garbage,
	}, testLogger())
	if err == nil {
		t.Fatal("an unparseable CA file was accepted")
	}
	if !strings.Contains(err.Error(), "CA file") {
		t.Errorf("error = %v, want it to name the CA file", err)
	}
}
