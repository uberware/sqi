// SPDX-License-Identifier: AGPL-3.0-or-later

package tlsutil_test

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
	"github.com/uberware/sqi/internal/tlsutil"
)

// farmCerts generates a CA and a server keypair into a temp dir and returns
// (certFile, keyFile, caFile).
func farmCerts(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	ca, err := certgen.NewCA("test CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}
	return filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), filepath.Join(dir, "ca.crt")
}

func TestServerConfig_NoClientCAMeansNoClientAuth(t *testing.T) {
	cert, key, _ := farmCerts(t)
	tc, err := tlsutil.ServerConfig(cert, key, "")
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if tc.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert", tc.ClientAuth)
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", tc.MinVersion)
	}
}

func TestServerConfig_ClientCARequiresAndVerifies(t *testing.T) {
	cert, key, caFile := farmCerts(t)
	tc, err := tlsutil.ServerConfig(cert, key, caFile)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if tc.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", tc.ClientAuth)
	}
	if tc.ClientCAs == nil {
		t.Error("ClientCAs is nil, want the configured CA pool")
	}
}

func TestServerConfig_BadPathIsReported(t *testing.T) {
	cert, key, _ := farmCerts(t)
	if _, err := tlsutil.ServerConfig(cert+".nope", key, ""); err == nil {
		t.Error("a missing certificate file was accepted")
	}
}

func TestCertPool_RejectsUnparseableBundle(t *testing.T) {
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := tlsutil.CertPool(garbage); err == nil {
		t.Error("an unparseable CA bundle was accepted")
	}
	if _, err := tlsutil.CertPool(filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Error("a missing CA file was accepted")
	}
}

func TestLeafOf_ReturnsTheLeaf(t *testing.T) {
	cert, key, _ := farmCerts(t)
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	leaf, err := tlsutil.LeafOf(pair)
	if err != nil {
		t.Fatalf("LeafOf: %v", err)
	}
	if leaf.NotAfter.IsZero() {
		t.Error("leaf has a zero NotAfter")
	}

	// The GODEBUG=x509keypairleaf=0 path: Leaf nil, re-parsed from DER.
	pair.Leaf = nil
	reparsed, err := tlsutil.LeafOf(pair)
	if err != nil {
		t.Fatalf("LeafOf with a nil Leaf: %v", err)
	}
	if !reparsed.NotAfter.Equal(leaf.NotAfter) {
		t.Error("re-parsed leaf disagrees with the populated one")
	}
}
