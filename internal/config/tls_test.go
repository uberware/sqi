// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
	"github.com/uberware/sqi/internal/config"
)

// writeCerts generates a farm CA and a server keypair into a temp dir and
// returns (certFile, keyFile, caFile). validFor is passed through so a
// caller can mint an already-expired certificate.
func writeCerts(t *testing.T, validFor time.Duration) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	ca, err := certgen.NewCA("test CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"localhost", "127.0.0.1"}, validFor)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}
	return filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), filepath.Join(dir, "ca.crt")
}

// writeFutureCerts mints a certificate whose validity window has not started,
// for the clock-skew case. certgen backdates NotBefore by an hour, so the
// window has to be pushed further out than that.
func writeFutureCerts(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	ca, err := certgen.NewCA("test CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewServerCertNotBefore([]string{"localhost"}, time.Now().Add(48*time.Hour), 72*time.Hour)
	if err != nil {
		t.Fatalf("NewServerCertNotBefore: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "future", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}
	return filepath.Join(dir, "future.crt"), filepath.Join(dir, "future.key")
}

// findErr returns the ValidationError for field, or nil.
func findErr(errs []config.ValidationError, field string) *config.ValidationError {
	for i := range errs {
		if errs[i].Field == field {
			return &errs[i]
		}
	}
	return nil
}

func TestValidate_HTTPTLS(t *testing.T) {
	goodCert, goodKey, _ := writeCerts(t, time.Hour)
	expiredCert, expiredKey, _ := writeCerts(t, -time.Hour)
	futureCert, futureKey := writeFutureCerts(t)
	otherCert, _, _ := writeCerts(t, time.Hour)

	tests := []struct {
		name      string
		tlsCfg    config.TLSConfig
		wantField string
		wantMatch string
	}{
		{
			name:      "enabled with no cert or key",
			tlsCfg:    config.TLSConfig{Enabled: true},
			wantField: "http.tls.cert_file",
			wantMatch: "http.tls.key_file",
		},
		{
			name:      "enabled with cert but no key",
			tlsCfg:    config.TLSConfig{Enabled: true, CertFile: goodCert},
			wantField: "http.tls.key_file",
			wantMatch: "must be set",
		},
		{
			name:      "missing file",
			tlsCfg:    config.TLSConfig{Enabled: true, CertFile: goodCert + ".nope", KeyFile: goodKey},
			wantField: "http.tls.cert_file",
			wantMatch: "no such file",
		},
		{
			name:      "key does not match certificate",
			tlsCfg:    config.TLSConfig{Enabled: true, CertFile: otherCert, KeyFile: goodKey},
			wantField: "http.tls.cert_file",
			wantMatch: "does not match",
		},
		{
			name:      "not yet valid certificate",
			tlsCfg:    config.TLSConfig{Enabled: true, CertFile: futureCert, KeyFile: futureKey},
			wantField: "http.tls.cert_file",
			wantMatch: "not valid until",
		},
		{
			name:      "expired certificate",
			tlsCfg:    config.TLSConfig{Enabled: true, CertFile: expiredCert, KeyFile: expiredKey},
			wantField: "http.tls.cert_file",
			wantMatch: "expired",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.HTTP.TLS = tt.tlsCfg
			errs := config.Validate(cfg)
			ve := findErr(errs, tt.wantField)
			if ve == nil {
				t.Fatalf("no ValidationError for %q; got %v", tt.wantField, errs)
			}
			if !strings.Contains(ve.Message, tt.wantMatch) {
				t.Errorf("message = %q, want it to contain %q", ve.Message, tt.wantMatch)
			}
		})
	}
}

func TestValidate_HTTPTLSValidConfigPasses(t *testing.T) {
	cert, key, _ := writeCerts(t, time.Hour)
	cfg := config.DefaultConfig()
	cfg.HTTP.TLS = config.TLSConfig{Enabled: true, CertFile: cert, KeyFile: key}
	if errs := config.Validate(cfg); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

func TestValidate_NATSTLSClientCARequiresTLSEnabled(t *testing.T) {
	_, _, caFile := writeCerts(t, time.Hour)
	cfg := config.DefaultConfig()
	cfg.NATS.TLS = config.NATSTLSConfig{Enabled: false, ClientCAFile: caFile}

	ve := findErr(config.Validate(cfg), "nats.tls.client_ca_file")
	if ve == nil {
		t.Fatal("client_ca_file without nats.tls.enabled was accepted; mTLS without TLS is meaningless")
	}
	if !strings.Contains(ve.Message, "nats.tls.enabled") {
		t.Errorf("message = %q, want it to name nats.tls.enabled", ve.Message)
	}
}

func TestValidate_NATSTLSClientCAMustParse(t *testing.T) {
	cert, key, _ := writeCerts(t, time.Hour)
	notACA := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(notACA, []byte("this is not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.NATS.TLS = config.NATSTLSConfig{
		Enabled: true, CertFile: cert, KeyFile: key, ClientCAFile: notACA,
	}
	if ve := findErr(config.Validate(cfg), "nats.tls.client_ca_file"); ve == nil {
		t.Fatal("unparseable client CA file was accepted")
	}
}

func TestValidate_DefaultConfigHasTLSOff(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.HTTP.TLS.Enabled {
		t.Error("http.tls.enabled defaults to true, want false")
	}
	if cfg.NATS.TLS.Enabled {
		t.Error("nats.tls.enabled defaults to true, want false")
	}
	if errs := config.Validate(cfg); len(errs) != 0 {
		t.Errorf("default config does not validate: %v", errs)
	}
}

// TestLoad_TLSFromFileAndEnv covers the loader wiring for all seven new keys.
//
// Validation tests exercise the structs directly, so nothing else here would
// notice a wrong `yaml:` tag, a key omitted from fileConfig, or a missing env
// binding — the value would simply never arrive and TLS would stay off with no
// error. nats.tls.client_ca_file in particular has no other coverage in any
// suite.
func TestLoad_TLSFromFileAndEnv(t *testing.T) {
	cert, key, ca := writeCerts(t, time.Hour)
	path := filepath.Join(t.TempDir(), "sqi.yaml")
	body := "http:\n  tls:\n    enabled: true\n    cert_file: " + cert + "\n    key_file: " + key +
		"\nnats:\n  tls:\n    enabled: true\n    cert_file: " + cert + "\n    key_file: " + key +
		"\n    client_ca_file: " + ca + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HTTP.TLS.Enabled || cfg.HTTP.TLS.CertFile != cert || cfg.HTTP.TLS.KeyFile != key {
		t.Errorf("http.tls from file = %+v, want enabled with %s/%s", cfg.HTTP.TLS, cert, key)
	}
	if !cfg.NATS.TLS.Enabled || cfg.NATS.TLS.CertFile != cert || cfg.NATS.TLS.KeyFile != key {
		t.Errorf("nats.tls from file = %+v, want enabled with %s/%s", cfg.NATS.TLS, cert, key)
	}
	if cfg.NATS.TLS.ClientCAFile != ca {
		t.Errorf("nats.tls.client_ca_file = %q, want %q", cfg.NATS.TLS.ClientCAFile, ca)
	}

	// Env must override the file, for every key that has a binding.
	t.Setenv("SQI_HTTP_TLS_ENABLED", "false")
	t.Setenv("SQI_HTTP_TLS_CERT_FILE", "/env/http.crt")
	t.Setenv("SQI_HTTP_TLS_KEY_FILE", "/env/http.key")
	t.Setenv("SQI_NATS_TLS_ENABLED", "false")
	t.Setenv("SQI_NATS_TLS_CERT_FILE", "/env/nats.crt")
	t.Setenv("SQI_NATS_TLS_KEY_FILE", "/env/nats.key")
	t.Setenv("SQI_NATS_TLS_CLIENT_CA_FILE", "/env/ca.pem")

	cfg, err = config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	for _, tc := range []struct{ got, want, name string }{
		{cfg.HTTP.TLS.CertFile, "/env/http.crt", "SQI_HTTP_TLS_CERT_FILE"},
		{cfg.HTTP.TLS.KeyFile, "/env/http.key", "SQI_HTTP_TLS_KEY_FILE"},
		{cfg.NATS.TLS.CertFile, "/env/nats.crt", "SQI_NATS_TLS_CERT_FILE"},
		{cfg.NATS.TLS.KeyFile, "/env/nats.key", "SQI_NATS_TLS_KEY_FILE"},
		{cfg.NATS.TLS.ClientCAFile, "/env/ca.pem", "SQI_NATS_TLS_CLIENT_CA_FILE"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s did not override the config file: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if cfg.HTTP.TLS.Enabled {
		t.Error("SQI_HTTP_TLS_ENABLED=false did not override the config file")
	}
	if cfg.NATS.TLS.Enabled {
		t.Error("SQI_NATS_TLS_ENABLED=false did not override the config file")
	}
}
