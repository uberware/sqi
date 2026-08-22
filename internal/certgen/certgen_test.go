// SPDX-License-Identifier: AGPL-3.0-or-later

package certgen_test

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
)

func parseLeaf(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("no PEM block in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func TestNewCA_IsACertificateAuthority(t *testing.T) {
	ca, err := certgen.NewCA("sqi farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	cert := parseLeaf(t, ca.CertPEM)
	if !cert.IsCA {
		t.Error("CA certificate has IsCA = false, want true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA certificate lacks KeyUsageCertSign")
	}
}

func TestNewServerCert_SANsAndExtKeyUsage(t *testing.T) {
	ca, err := certgen.NewCA("sqi farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"sqi.example", "127.0.0.1", "::1"}, 2*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	cert := parseLeaf(t, leaf.CertPEM)

	if got := cert.DNSNames; len(got) != 1 || got[0] != "sqi.example" {
		t.Errorf("DNSNames = %v, want [sqi.example]", got)
	}
	if len(cert.IPAddresses) != 2 {
		t.Fatalf("IPAddresses = %v, want 2 entries", cert.IPAddresses)
	}
	if !cert.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("IPAddresses[0] = %v, want 127.0.0.1", cert.IPAddresses[0])
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ExtKeyUsage = %v, want [ServerAuth]", cert.ExtKeyUsage)
	}
}

func TestNewClientCert_ExtKeyUsageClientAuth(t *testing.T) {
	ca, err := certgen.NewCA("sqi farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewClientCert("worker-01", 2*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewClientCert: %v", err)
	}
	cert := parseLeaf(t, leaf.CertPEM)
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [ClientAuth]", cert.ExtKeyUsage)
	}
	if cert.Subject.CommonName != "worker-01" {
		t.Errorf("CommonName = %q, want %q", cert.Subject.CommonName, "worker-01")
	}
}

func TestNewServerCert_ChainVerifiesAgainstCA(t *testing.T) {
	ca, err := certgen.NewCA("sqi farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"sqi.example"}, 2*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("CA PEM did not append to pool")
	}
	cert := parseLeaf(t, leaf.CertPEM)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "sqi.example",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf does not verify against its CA: %v", err)
	}
}

func TestNewServerCert_ExpiredWhenValidForIsNegative(t *testing.T) {
	ca, err := certgen.NewCA("sqi farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"sqi.example"}, -1*time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	cert := parseLeaf(t, leaf.CertPEM)
	if !cert.NotAfter.Before(time.Now()) {
		t.Errorf("NotAfter = %v, want a time in the past", cert.NotAfter)
	}
}

func TestLoadCA_RoundTripsAndIssues(t *testing.T) {
	original, err := certgen.NewCA("sqi farm CA", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	// Reload from PEM exactly as an operator's ca.crt/ca.key would be.
	loaded, err := certgen.LoadCA(original.CertPEM, original.KeyPEM)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if loaded.Cert.Subject.CommonName != "sqi farm CA" {
		t.Errorf("CommonName = %q, want %q", loaded.Cert.Subject.CommonName, "sqi farm CA")
	}

	// The whole point: a certificate issued from the RELOADED CA must verify
	// against the ORIGINAL one. Anything less means an operator adding a worker
	// later gets a certificate their farm rejects.
	leaf, err := loaded.NewClientCert("render-07", 2*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewClientCert from a loaded CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(original.CertPEM) {
		t.Fatal("original CA PEM did not append to pool")
	}
	cert := parseLeaf(t, leaf.CertPEM)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("certificate issued from the reloaded CA does not verify against the original: %v", err)
	}
}

func TestLoadCA_RejectsBadInput(t *testing.T) {
	good, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	other, err := certgen.NewCA("other CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	tests := []struct {
		name            string
		certPEM, keyPEM []byte
	}{
		{"garbage certificate", []byte("not a pem"), good.KeyPEM},
		{"garbage key", good.CertPEM, []byte("not a pem")},
		{"key from a different CA", good.CertPEM, other.KeyPEM},
		{"empty", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := certgen.LoadCA(tt.certPEM, tt.keyPEM); err == nil {
				t.Error("LoadCA accepted invalid material")
			}
		})
	}
}

func TestLoadCA_RejectsANonCACertificate(t *testing.T) {
	ca, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"localhost"}, time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	// A leaf cannot sign. Loading one as a CA would produce certificates that
	// nothing accepts, discovered only at the first handshake.
	if _, err := certgen.LoadCA(leaf.CertPEM, leaf.KeyPEM); err == nil {
		t.Error("LoadCA accepted a non-CA certificate")
	}
}
