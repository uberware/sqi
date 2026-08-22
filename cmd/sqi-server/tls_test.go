// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runTLSInitCmd drives "tls init" with args and returns its stdout.
func runTLSInitCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Reset the package-level flag targets: cobra flag vars persist between
	// runs in the same process, and a stale --client would leak across tests.
	tlsInitOut, tlsInitHosts, tlsInitClients = "./certs", nil, nil

	prepareRoot(append([]string{"tls", "init"}, args...))
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	err := rootCmd.Execute()
	return out.String(), err
}

// parseCertFile reads and parses a PEM certificate from disk.
func parseCertFile(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("no PEM block in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cert
}

func TestTLSInit_WritesExpectedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	if _, err := runTLSInitCmd(t, "--out", dir, "--host", "sqi.example"); err != nil {
		t.Fatalf("tls init: %v", err)
	}

	for _, name := range []string{"ca.crt", "ca.key", "server.crt", "server.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	server := parseCertFile(t, filepath.Join(dir, "server.crt"))
	if !slices.Contains(server.DNSNames, "sqi.example") {
		t.Errorf("DNSNames = %v, want it to contain sqi.example", server.DNSNames)
	}
	// Loopback is always included so the server can verify its own listener.
	if !slices.Contains(server.DNSNames, "localhost") {
		t.Errorf("DNSNames = %v, want it to contain localhost", server.DNSNames)
	}
	for _, want := range []string{"127.0.0.1", "::1"} {
		if !slices.ContainsFunc(server.IPAddresses, func(ip net.IP) bool { return ip.Equal(net.ParseIP(want)) }) {
			t.Errorf("IPAddresses = %v, want it to contain %s", server.IPAddresses, want)
		}
	}
	if len(server.ExtKeyUsage) != 1 || server.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ExtKeyUsage = %v, want [ServerAuth]", server.ExtKeyUsage)
	}

	// The leaf must verify against the CA it was issued from.
	ca := parseCertFile(t, filepath.Join(dir, "ca.crt"))
	if !ca.IsCA {
		t.Error("ca.crt is not a CA certificate")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := server.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "sqi.example",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("server.crt does not verify against ca.crt: %v", err)
	}

	// Private keys must not be world- or group-readable.
	for _, name := range []string{"ca.key", "server.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}

func TestTLSInit_ClientCerts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	if _, err := runTLSInitCmd(t, "--out", dir, "--client", "worker-01", "--client", "worker-02"); err != nil {
		t.Fatalf("tls init: %v", err)
	}

	for _, id := range []string{"worker-01", "worker-02"} {
		cert := parseCertFile(t, filepath.Join(dir, "client-"+id+".crt"))
		if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
			t.Errorf("%s ExtKeyUsage = %v, want [ClientAuth]", id, cert.ExtKeyUsage)
		}
		if cert.Subject.CommonName != id {
			t.Errorf("CommonName = %q, want %q", cert.Subject.CommonName, id)
		}
		if _, err := os.Stat(filepath.Join(dir, "client-"+id+".key")); err != nil {
			t.Errorf("missing key for %s: %v", id, err)
		}
	}
}

func TestTLSInit_RefusesExistingCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	if _, err := runTLSInitCmd(t, "--out", dir); err != nil {
		t.Fatalf("first tls init: %v", err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("read ca.key: %v", err)
	}

	_, err = runTLSInitCmd(t, "--out", dir)
	if err == nil {
		t.Fatal("second tls init overwrote an existing CA")
	}
	if !strings.Contains(err.Error(), "CA already exists") {
		t.Errorf("error = %v, want it to name the existing CA", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("re-read ca.key: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Error("ca.key changed despite the refusal; every certificate issued from it would stop verifying")
	}
}

func TestTLSInit_PrintsConfigKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	out, err := runTLSInitCmd(t, "--out", dir, "--client", "worker-01")
	if err != nil {
		t.Fatalf("tls init: %v", err)
	}
	for _, want := range []string{
		"http.tls.cert_file",
		"nats.tls.cert_file",
		"nats.tls_ca_file",
		"nats.server_tls_ca_file",
		"nats.tls.client_ca_file",
		"coordinated restart",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q; an operator is left guessing what to do with the files\n%s", want, out)
		}
	}
}

// ── tls issue ────────────────────────────────────────────────────────────────
//
// `tls init` deliberately refuses to overwrite a CA, which left no way to add a
// worker to an established mTLS farm or to rotate a server certificate: the
// whole command failed, taking --client with it. `tls issue` is that path.

// runTLSIssueCmd drives "tls issue" and returns its stdout.
func runTLSIssueCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	tlsIssueOut, tlsIssueClients, tlsIssueHosts, tlsIssueForce = "./certs", nil, nil, false

	prepareRoot(append([]string{"tls", "issue"}, args...))
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	err := rootCmd.Execute()
	return out.String(), err
}

// existingFarm runs `tls init` into a fresh directory and returns it.
func existingFarm(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "certs")
	if _, err := runTLSInitCmd(t, "--out", dir, "--host", "sqi.example"); err != nil {
		t.Fatalf("tls init: %v", err)
	}
	return dir
}

func TestTLSIssue_ClientCertAgainstAnExistingCA(t *testing.T) {
	dir := existingFarm(t)
	caBefore, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("read ca.key: %v", err)
	}

	if _, err := runTLSIssueCmd(t, "--out", dir, "--client", "render-07"); err != nil {
		t.Fatalf("tls issue --client against an existing CA: %v", err)
	}

	cert := parseCertFile(t, filepath.Join(dir, "client-render-07.crt"))
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [ClientAuth]", cert.ExtKeyUsage)
	}

	// It must chain to the farm's EXISTING CA, or the broker rejects it.
	ca := parseCertFile(t, filepath.Join(dir, "ca.crt"))
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("issued certificate does not verify against the farm CA: %v", err)
	}

	// Issuing must never disturb the CA itself.
	caAfter, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("re-read ca.key: %v", err)
	}
	if !bytes.Equal(caBefore, caAfter) {
		t.Error("ca.key changed while issuing a leaf certificate")
	}

	info, err := os.Stat(filepath.Join(dir, "client-render-07.key"))
	if err != nil {
		t.Fatalf("stat client key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("client key mode = %04o, want 0600", got)
	}
}

func TestTLSIssue_ServerCertForRotation(t *testing.T) {
	dir := existingFarm(t)
	before := parseCertFile(t, filepath.Join(dir, "server.crt"))

	// Rotation replaces server.crt, so it needs --force.
	if _, err := runTLSIssueCmd(t, "--out", dir, "--host", "sqi.example"); err == nil {
		t.Fatal("overwrote server.crt without --force")
	}
	if _, err := runTLSIssueCmd(t, "--out", dir, "--host", "sqi.example", "--force"); err != nil {
		t.Fatalf("tls issue --host --force: %v", err)
	}

	after := parseCertFile(t, filepath.Join(dir, "server.crt"))
	if after.SerialNumber.Cmp(before.SerialNumber) == 0 {
		t.Error("server.crt was not reissued: same serial number")
	}
	ca := parseCertFile(t, filepath.Join(dir, "ca.crt"))
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := after.Verify(x509.VerifyOptions{
		Roots: pool, DNSName: "sqi.example",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("rotated server certificate does not verify: %v", err)
	}
}

func TestTLSIssue_RefusesToClobberAnExistingClientKey(t *testing.T) {
	dir := existingFarm(t)
	if _, err := runTLSIssueCmd(t, "--out", dir, "--client", "render-07"); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "client-render-07.key"))
	if err != nil {
		t.Fatalf("read client key: %v", err)
	}

	// A worker is already using this key; silently replacing it would take that
	// worker offline at its next restart with no indication why.
	_, err = runTLSIssueCmd(t, "--out", dir, "--client", "render-07")
	if err == nil {
		t.Fatal("reissued over an existing client key without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error does not mention --force: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "client-render-07.key"))
	if err != nil {
		t.Fatalf("re-read client key: %v", err)
	}
	if !bytes.Equal(original, after) {
		t.Error("client key changed despite the refusal")
	}
}

func TestTLSIssue_ErrorsWithoutACA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	_, err := runTLSIssueCmd(t, "--out", dir, "--client", "w1")
	if err == nil {
		t.Fatal("issued against a directory with no CA")
	}
	if !strings.Contains(err.Error(), "tls init") {
		t.Errorf("error does not point at `tls init`: %v", err)
	}
}

func TestTLSIssue_ErrorsWithNothingToIssue(t *testing.T) {
	dir := existingFarm(t)
	if _, err := runTLSIssueCmd(t, "--out", dir); err == nil {
		t.Fatal("accepted an invocation with neither --client nor --host")
	}
}

func TestTLSInit_PointsAtIssueWhenTheCAExists(t *testing.T) {
	dir := existingFarm(t)
	// The original gap: this failed with no hint that another command does it.
	_, err := runTLSInitCmd(t, "--out", dir, "--client", "render-07")
	if err == nil {
		t.Fatal("tls init overwrote an existing CA")
	}
	if !strings.Contains(err.Error(), "tls issue") {
		t.Errorf("error does not point at `tls issue`: %v", err)
	}
}
