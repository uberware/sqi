// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/config"
)

// writeNATSFile writes body as a config file and returns its path.
func writeNATSFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sqi-server.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// writeSeedFile writes an nkey seed (with a trailing newline, as an editor or
// `nk -gen` leaves it) and returns its path.
func writeSeedFile(t *testing.T, kp nkeys.KeyPair) string {
	t.Helper()
	seed, err := kp.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "seed.nk")
	if err := os.WriteFile(path, append(seed, '\n'), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

func newSeed(t *testing.T, account bool) string {
	t.Helper()
	var (
		kp  nkeys.KeyPair
		err error
	)
	if account {
		kp, err = nkeys.CreateAccount()
	} else {
		kp, err = nkeys.CreateUser()
	}
	if err != nil {
		t.Fatalf("create nkey: %v", err)
	}
	return writeSeedFile(t, kp)
}

func TestLoad_NATSDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := cfg.NATS
	if n.Mode != config.NATSModeEmbedded {
		t.Errorf("mode = %q, want %q", n.Mode, config.NATSModeEmbedded)
	}
	if n.Embedded.Addr != "0.0.0.0:4222" {
		t.Errorf("embedded.addr = %q", n.Embedded.Addr)
	}
	if n.Embedded.DataDir != "data/nats" {
		t.Errorf("embedded.data_dir = %q", n.Embedded.DataDir)
	}
	if n.Embedded.MaxStoreMB != 1024 {
		t.Errorf("embedded.max_store_mb = %d", n.Embedded.MaxStoreMB)
	}
	if n.Auth.UserTTL != time.Hour {
		t.Errorf("auth.user_ttl = %s, want 1h", n.Auth.UserTTL)
	}
	if n.External.Account != "SQI" || n.External.StreamReplicas != 1 || n.External.ConnectTimeout != 30*time.Second {
		t.Errorf("external defaults = %+v", n.External)
	}
	if config.NATSModeExternal != "external" {
		t.Errorf("NATSModeExternal = %q", config.NATSModeExternal)
	}
}

func TestLoad_RemovedNATSKeys(t *testing.T) {
	tests := []struct {
		key  string
		body string
	}{
		{"nats.addr", "nats:\n  addr: 0.0.0.0:4222\n"},
		{"nats.data_dir", "nats:\n  data_dir: /var/nats\n"},
		{"nats.max_store_mb", "nats:\n  max_store_mb: 512\n"},
		{"nats.tls", "nats:\n  tls:\n    enabled: true\n"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			_, err := config.Load(writeNATSFile(t, tt.body), config.FlagOverrides{})
			if err == nil {
				t.Fatalf("Load accepted removed key %s", tt.key)
			}
			want := "nats.embedded." + strings.TrimPrefix(tt.key, "nats.")
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		})
	}

	t.Run("nats.embedded keys load", func(t *testing.T) {
		body := "nats:\n  embedded:\n    addr: 127.0.0.1:4333\n    max_store_mb: 7\n"
		cfg, err := config.Load(writeNATSFile(t, body), config.FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.NATS.Embedded.Addr != "127.0.0.1:4333" || cfg.NATS.Embedded.MaxStoreMB != 7 {
			t.Errorf("embedded = %+v", cfg.NATS.Embedded)
		}
	})
}

func TestLoad_UnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // empty: the file loads
	}{
		{"unknown top-level section", "bogus:\n  x: 1\n", "bogus"},
		{"unknown nested key", "nats:\n  bogus: 1\n", "bogus"},
		{"misspelled key", "scheduler:\n  heartbeat_timeot: 1m\n", "unknown key scheduler.heartbeat_timeot"},
		{"unknown key in a later section", "http:\n  addr: :9090\nstore:\n  drivr: sqlite\n", "unknown key store.drivr"},
		{"known keys load", "http:\n  addr: 127.0.0.1:9090\n", ""},
		{"empty file loads", "", ""},
		{"comment-only file loads", "# nothing set\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeNATSFile(t, tt.body), config.FlagOverrides{})
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Load: %v", err)
			case tt.want != "" && err == nil:
				t.Fatalf("Load accepted the unknown key %q", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Errorf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestLoad_RemovedNATSEnvVars(t *testing.T) {
	tests := []struct{ old, want string }{
		{"SQI_NATS_ADDR", "SQI_NATS_EMBEDDED_ADDR"},
		{"SQI_NATS_DATA_DIR", "SQI_NATS_EMBEDDED_DATA_DIR"},
		{"SQI_NATS_MAX_STORE_MB", "SQI_NATS_EMBEDDED_MAX_STORE_MB"},
		{"SQI_NATS_TLS_ENABLED", "SQI_NATS_EMBEDDED_TLS_ENABLED"},
		{"SQI_NATS_TLS_CERT_FILE", "SQI_NATS_EMBEDDED_TLS_CERT_FILE"},
		{"SQI_NATS_TLS_KEY_FILE", "SQI_NATS_EMBEDDED_TLS_KEY_FILE"},
		{"SQI_NATS_TLS_CLIENT_CA_FILE", "SQI_NATS_EMBEDDED_TLS_CLIENT_CA_FILE"},
	}
	for _, tt := range tests {
		t.Run(tt.old, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv(tt.old, "x")
			_, err := config.Load("", config.FlagOverrides{})
			if err == nil {
				t.Fatalf("Load accepted removed env var %s", tt.old)
			}
			if !strings.Contains(err.Error(), tt.old) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q must name %s and %s", err, tt.old, tt.want)
			}
		})
	}
}

func TestLoad_NATSNewKeysFromFileAndEnv(t *testing.T) {
	body := `
nats:
  mode: external
  auth:
    user_ttl: 2h
  embedded:
    advertise_url: tls://farm.example:4222
  external:
    urls: [tls://a:4222, tls://b:4222]
    advertise_urls: [tls://pub:4222]
    account: FARM
    nkey_seed_file: /f/data.nk
    stream_replicas: 3
    connect_timeout: 45s
    tls:
      ca_file: /f/ca.pem
      cert_file: /f/c.pem
      key_file: /f/k.pem
      server_name: nats.example
    callout:
      nkey_seed_file: /f/callout.nk
      issuer_seed_file: /f/issuer.nk
      system_creds_file: /f/sys.creds
`
	cfg, err := config.Load(writeNATSFile(t, body), config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := cfg.NATS
	e := n.External
	if n.Mode != "external" || n.Auth.UserTTL != 2*time.Hour || n.Embedded.AdvertiseURL != "tls://farm.example:4222" {
		t.Errorf("mode/auth/embedded = %q %s %q", n.Mode, n.Auth.UserTTL, n.Embedded.AdvertiseURL)
	}
	if len(e.URLs) != 2 || e.URLs[1] != "tls://b:4222" || len(e.AdvertiseURLs) != 1 {
		t.Errorf("urls = %v advertise = %v", e.URLs, e.AdvertiseURLs)
	}
	if e.Account != "FARM" || e.NkeySeedFile != "/f/data.nk" || e.StreamReplicas != 3 || e.ConnectTimeout != 45*time.Second {
		t.Errorf("external = %+v", e)
	}
	if e.TLS != (config.NATSClientTLSConfig{CAFile: "/f/ca.pem", CertFile: "/f/c.pem", KeyFile: "/f/k.pem", ServerName: "nats.example"}) {
		t.Errorf("external.tls = %+v", e.TLS)
	}
	if e.Callout != (config.NATSCalloutConfig{NkeySeedFile: "/f/callout.nk", IssuerSeedFile: "/f/issuer.nk", SystemCredsFile: "/f/sys.creds"}) {
		t.Errorf("external.callout = %+v", e.Callout)
	}

	// Every key has an environment binding, and env beats the file.
	env := map[string]string{
		"SQI_NATS_MODE":                               "embedded",
		"SQI_NATS_AUTH_USER_TTL":                      "3h",
		"SQI_NATS_EMBEDDED_ADDR":                      "127.0.0.1:1",
		"SQI_NATS_EMBEDDED_ADVERTISE_URL":             "nats://x:1",
		"SQI_NATS_EMBEDDED_DATA_DIR":                  "/e/data",
		"SQI_NATS_EMBEDDED_MAX_STORE_MB":              "9",
		"SQI_NATS_EMBEDDED_TLS_ENABLED":               "true",
		"SQI_NATS_EMBEDDED_TLS_CERT_FILE":             "/e/c",
		"SQI_NATS_EMBEDDED_TLS_KEY_FILE":              "/e/k",
		"SQI_NATS_EMBEDDED_TLS_CLIENT_CA_FILE":        "/e/ca",
		"SQI_NATS_EXTERNAL_URLS":                      "tls://x:1, tls://y:2",
		"SQI_NATS_EXTERNAL_ADVERTISE_URLS":            "tls://z:3",
		"SQI_NATS_EXTERNAL_ACCOUNT":                   "ENV",
		"SQI_NATS_EXTERNAL_NKEY_SEED_FILE":            "/x/n",
		"SQI_NATS_EXTERNAL_CREDS_FILE":                "/x/c",
		"SQI_NATS_EXTERNAL_STREAM_REPLICAS":           "5",
		"SQI_NATS_EXTERNAL_CONNECT_TIMEOUT":           "9s",
		"SQI_NATS_EXTERNAL_TLS_CA_FILE":               "/x/ca",
		"SQI_NATS_EXTERNAL_TLS_CERT_FILE":             "/x/cc",
		"SQI_NATS_EXTERNAL_TLS_KEY_FILE":              "/x/ck",
		"SQI_NATS_EXTERNAL_TLS_SERVER_NAME":           "sn",
		"SQI_NATS_EXTERNAL_CALLOUT_NKEY_SEED_FILE":    "/x/cn",
		"SQI_NATS_EXTERNAL_CALLOUT_ISSUER_SEED_FILE":  "/x/ci",
		"SQI_NATS_EXTERNAL_CALLOUT_SYSTEM_CREDS_FILE": "/x/cs",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err = config.Load(writeNATSFile(t, body), config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	n = cfg.NATS
	e = n.External
	switch {
	case n.Mode != "embedded", n.Auth.UserTTL != 3*time.Hour:
		t.Errorf("mode/user_ttl env not applied: %q %s", n.Mode, n.Auth.UserTTL)
	case n.Embedded.Addr != "127.0.0.1:1", n.Embedded.AdvertiseURL != "nats://x:1", n.Embedded.DataDir != "/e/data", n.Embedded.MaxStoreMB != 9:
		t.Errorf("embedded env not applied: %+v", n.Embedded)
	case !n.Embedded.TLS.Enabled, n.Embedded.TLS.CertFile != "/e/c", n.Embedded.TLS.KeyFile != "/e/k", n.Embedded.TLS.ClientCAFile != "/e/ca":
		t.Errorf("embedded.tls env not applied: %+v", n.Embedded.TLS)
	case len(e.URLs) != 2 || e.URLs[0] != "tls://x:1" || e.URLs[1] != "tls://y:2", len(e.AdvertiseURLs) != 1 || e.AdvertiseURLs[0] != "tls://z:3":
		t.Errorf("external urls env not applied: %v %v", e.URLs, e.AdvertiseURLs)
	case e.Account != "ENV", e.NkeySeedFile != "/x/n", e.CredsFile != "/x/c", e.StreamReplicas != 5, e.ConnectTimeout != 9*time.Second:
		t.Errorf("external env not applied: %+v", e)
	case e.TLS != (config.NATSClientTLSConfig{CAFile: "/x/ca", CertFile: "/x/cc", KeyFile: "/x/ck", ServerName: "sn"}):
		t.Errorf("external.tls env not applied: %+v", e.TLS)
	case e.Callout != (config.NATSCalloutConfig{NkeySeedFile: "/x/cn", IssuerSeedFile: "/x/ci", SystemCredsFile: "/x/cs"}):
		t.Errorf("external.callout env not applied: %+v", e.Callout)
	}
}

func TestValidate_NATSExternal(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.nk")
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string // empty = valid
	}{
		{"valid, auth off", func(*config.Config) {}, ""},
		{"account AUTH", func(c *config.Config) { c.NATS.External.Account = "AUTH" }, "nats.external.account"},
		{"account SYS", func(c *config.Config) { c.NATS.External.Account = "SYS" }, "nats.external.account"},
		{"account $G", func(c *config.Config) { c.NATS.External.Account = "$G" }, "nats.external.account"},
		{"account with space", func(c *config.Config) { c.NATS.External.Account = "SQI prod" }, "letters, digits"},
		{"account with quote", func(c *config.Config) { c.NATS.External.Account = `SQI"x` }, "letters, digits"},
		{"account empty", func(c *config.Config) { c.NATS.External.Account = "" }, "must not be empty"},
		{"account plain", func(c *config.Config) { c.NATS.External.Account = "SQI" }, ""},
		{"account with underscore and dash", func(c *config.Config) { c.NATS.External.Account = "sqi_prod-2" }, ""},
		{"urls empty", func(c *config.Config) { c.NATS.External.URLs = nil }, "nats.external.urls"},
		{"seed and creds both set", func(c *config.Config) {
			c.NATS.External.NkeySeedFile = newSeed(t, false)
			c.NATS.External.CredsFile = newSeed(t, false)
		}, "mutually exclusive"},
		{"replicas 2", func(c *config.Config) { c.NATS.External.StreamReplicas = 2 }, "1, 3 or 5"},
		{"replicas 3", func(c *config.Config) { c.NATS.External.StreamReplicas = 3 }, ""},
		{"replicas 5", func(c *config.Config) { c.NATS.External.StreamReplicas = 5 }, ""},
		{"auth on, no issuer seed", func(c *config.Config) {
			c.NATS.Auth.Enabled = true
			c.NATS.External.Callout.NkeySeedFile = newSeed(t, false)
		}, "issuer_seed_file"},
		{"auth on, no callout nkey", func(c *config.Config) {
			c.NATS.Auth.Enabled = true
			c.NATS.External.Callout.IssuerSeedFile = newSeed(t, true)
		}, "callout.nkey_seed_file"},
		{"auth on, valid callout", func(c *config.Config) {
			c.NATS.Auth.Enabled = true
			c.NATS.External.Callout.NkeySeedFile = newSeed(t, false)
			c.NATS.External.Callout.IssuerSeedFile = newSeed(t, true)
		}, ""},
		{"issuer seed is a user seed", func(c *config.Config) {
			c.NATS.Auth.Enabled = true
			c.NATS.External.Callout.NkeySeedFile = newSeed(t, false)
			c.NATS.External.Callout.IssuerSeedFile = newSeed(t, false)
		}, "account nkey seed"},
		{"issuer seed is garbage", func(c *config.Config) {
			c.NATS.Auth.Enabled = true
			c.NATS.External.Callout.NkeySeedFile = newSeed(t, false)
			p := filepath.Join(t.TempDir(), "junk")
			if err := os.WriteFile(p, []byte("not a seed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			c.NATS.External.Callout.IssuerSeedFile = p
		}, "account nkey seed"},
		{"missing nkey seed path", func(c *config.Config) { c.NATS.External.NkeySeedFile = missing }, missing},
		{"missing creds path", func(c *config.Config) { c.NATS.External.CredsFile = missing }, missing},
		{"missing ca path", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			c.NATS.External.TLS.CAFile = missing
		}, missing},
		{"missing issuer path", func(c *config.Config) {
			c.NATS.Auth.Enabled = true
			c.NATS.External.Callout.NkeySeedFile = newSeed(t, false)
			c.NATS.External.Callout.IssuerSeedFile = missing
		}, missing},
		{"unknown mode", func(c *config.Config) { c.NATS.Mode = "cluster" }, "embedded or external"},
		{"client cert without key", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			cert, _, _ := writeCerts(t, time.Hour)
			c.NATS.External.TLS.CertFile = cert
		}, "nats.external.tls.key_file"},
		{"client key without cert", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			_, key, _ := writeCerts(t, time.Hour)
			c.NATS.External.TLS.KeyFile = key
		}, "nats.external.tls.cert_file"},
		{"client cert and key that do not pair", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			cert, _, _ := writeCerts(t, time.Hour)
			_, key, _ := writeCerts(t, time.Hour)
			c.NATS.External.TLS.CertFile, c.NATS.External.TLS.KeyFile = cert, key
		}, "cannot load certificate/key pair"},
		{"client cert and key that pair", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			cert, key, _ := writeCerts(t, time.Hour)
			c.NATS.External.TLS.CertFile, c.NATS.External.TLS.KeyFile = cert, key
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.NATS.Mode = config.NATSModeExternal
			cfg.NATS.External.URLs = []string{"nats://a:4222"}
			tt.mutate(&cfg)
			errs := config.Validate(cfg)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate = %v, want none", errs)
				}
				return
			}
			var all []string
			for _, e := range errs {
				all = append(all, e.Error())
			}
			if !strings.Contains(strings.Join(all, "\n"), tt.wantErr) {
				t.Fatalf("Validate = %v, want an error containing %q", errs, tt.wantErr)
			}
		})
	}
}

func TestValidate_NATSEmbeddedIgnoresExternalBlock(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NATS.External.StreamReplicas = 2 // would be invalid in external mode
	cfg.NATS.External.URLs = nil
	if errs := config.Validate(cfg); len(errs) != 0 {
		t.Fatalf("embedded mode validated the external block: %v", errs)
	}
}

func TestValidate_UserTTLBounds(t *testing.T) {
	tests := []struct {
		ttl     time.Duration
		wantErr bool
	}{
		{59 * time.Second, true},
		{25 * time.Hour, true},
		{time.Minute, false},
		{24 * time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.ttl.String(), func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.NATS.Auth.UserTTL = tt.ttl
			var found bool
			for _, e := range config.Validate(cfg) {
				if e.Field == "nats.auth.user_ttl" {
					found = true
				}
			}
			if found != tt.wantErr {
				t.Errorf("error on nats.auth.user_ttl = %v, want %v", found, tt.wantErr)
			}
		})
	}
}

func TestValidate_ExternalURLSchemesMustAgree(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		urls    []string
		ca      string
		wantErr string
	}{
		{"mixed", []string{"tls://a:4222", "nats://b:4222"}, "", "mixes tls:// and nats://"},
		{"ca with nats only", []string{"nats://a:4222"}, ca, "tls://"},
		{"all tls with ca", []string{"tls://a:4222", "tls://b:4222"}, ca, ""},
		{"all nats, no tls", []string{"nats://a:4222"}, "", ""},
		{"bad scheme", []string{"http://a:4222"}, "", "nats.external.urls"},
		// A port with no host has a non-empty url.Host (":4222") but
		// names nothing to dial.
		{"port but no host", []string{"nats://:4222"}, "", "invalid broker URL"},
		{"credentials but no host", []string{"nats://u:p@:4222"}, "", "invalid broker URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.NATS.Mode = config.NATSModeExternal
			cfg.NATS.External.URLs = tt.urls
			cfg.NATS.External.TLS.CAFile = tt.ca
			var all []string
			for _, e := range config.Validate(cfg) {
				all = append(all, e.Error())
			}
			joined := strings.Join(all, "\n")
			if tt.wantErr == "" {
				if joined != "" {
					t.Fatalf("Validate = %s, want none", joined)
				}
				return
			}
			if !strings.Contains(joined, tt.wantErr) {
				t.Fatalf("Validate = %q, want an error containing %q", joined, tt.wantErr)
			}
		})
	}
}

// The shipped example config must load, validate, and state the defaults for
// the nats block: it is the document an operator copies from.
func TestExampleConfig_NATSBlockMatchesDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "config", "sqi-server.example.yaml")
	cfg, err := config.Load(path, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("the shipped example config must load: %v", err)
	}
	want := config.DefaultConfig().NATS
	if cfg.NATS.Mode != want.Mode || cfg.NATS.Embedded != want.Embedded || cfg.NATS.Auth != want.Auth {
		t.Errorf("example nats block = %+v, want the defaults %+v", cfg.NATS, want)
	}
}

func TestValidate_ExternalTLSSettingsNeedTLSURLs(t *testing.T) {
	cert, key, ca := writeCerts(t, time.Hour)
	tests := []struct {
		name   string
		set    func(*config.NATSClientTLSConfig)
		field  string
		tlsURL bool
	}{
		{"ca_file", func(c *config.NATSClientTLSConfig) { c.CAFile = ca }, "nats.external.tls.ca_file", false},
		{"cert_file and key_file", func(c *config.NATSClientTLSConfig) { c.CertFile, c.KeyFile = cert, key }, "nats.external.tls.cert_file", false},
		{"key_file", func(c *config.NATSClientTLSConfig) { c.CertFile, c.KeyFile = cert, key }, "nats.external.tls.key_file", false},
		{"server_name", func(c *config.NATSClientTLSConfig) { c.ServerName = "nats.example" }, "nats.external.tls.server_name", false},
		{"everything with tls:// URLs", func(c *config.NATSClientTLSConfig) {
			*c = config.NATSClientTLSConfig{CAFile: ca, CertFile: cert, KeyFile: key, ServerName: "nats.example"}
		}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.NATS.Mode = config.NATSModeExternal
			cfg.NATS.External.URLs = []string{"nats://a:4222", "nats://b:4222"}
			if tt.tlsURL {
				cfg.NATS.External.URLs = []string{"tls://a:4222", "tls://b:4222"}
			}
			tt.set(&cfg.NATS.External.TLS)
			errs := config.Validate(cfg)
			if tt.field == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate = %v, want none", errs)
				}
				return
			}
			e := findErr(errs, tt.field)
			if e == nil || !strings.Contains(e.Message, "nats://") {
				t.Fatalf("Validate = %v, want an error on %s naming the nats:// URLs", errs, tt.field)
			}
		})
	}
}

func TestValidate_AdvertiseURLs(t *testing.T) {
	long := "tls://" + strings.Repeat("h", 250) + ":4222"
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string // empty = valid
	}{
		{"external: valid advertise_urls", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			c.NATS.External.AdvertiseURLs = []string{"tls://public:4222", "tls://public2:4222"}
		}, ""},
		{"external: no scheme", func(c *config.Config) {
			c.NATS.External.AdvertiseURLs = []string{"public:4222"}
		}, "nats.external.advertise_urls"},
		{"external: http scheme", func(c *config.Config) {
			c.NATS.External.AdvertiseURLs = []string{"http://public:4222"}
		}, "must use the tls:// or nats:// scheme"},
		{"external: empty host", func(c *config.Config) {
			c.NATS.External.AdvertiseURLs = []string{"tls://:4222"}
		}, "invalid advertised URL"},
		{"external: scheme disagrees with urls", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			c.NATS.External.AdvertiseURLs = []string{"nats://public:4222"}
		}, "nats.external.advertise_urls"},
		{"external: record too long", func(c *config.Config) {
			c.NATS.External.URLs = []string{"tls://a:4222"}
			c.NATS.External.AdvertiseURLs = []string{long}
		}, "255-byte DNS TXT limit"},
		{"embedded: valid", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "nats://nats.example.com:4222"
		}, ""},
		{"embedded: no scheme", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "nats.example.com:4222"
		}, "nats.embedded.advertise_url"},
		{"embedded: http scheme", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "http://nats.example.com:4222"
		}, "must use the tls:// or nats:// scheme"},
		{"embedded: empty host", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "nats://:4222"
		}, "invalid advertised URL"},
		{"embedded: tls:// without listener TLS", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "tls://nats.example.com:4222"
		}, "nats.embedded.tls.enabled is false"},
		{"embedded: nats:// with listener TLS", func(c *config.Config) {
			certFile, keyFile, _ := writeCerts(t, time.Hour)
			c.NATS.Embedded.TLS = config.NATSTLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
			c.NATS.Embedded.AdvertiseURL = "nats://nats.example.com:4222"
		}, "nats.embedded.tls.enabled is true"},
		{"embedded: record too long", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = strings.Replace(long, "tls", "nats", 1)
		}, "255-byte DNS TXT limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			if strings.HasPrefix(tt.name, "external") {
				cfg.NATS.Mode = config.NATSModeExternal
				cfg.NATS.External.URLs = []string{"nats://a:4222"}
			}
			tt.mutate(&cfg)
			var all []string
			for _, e := range config.Validate(cfg) {
				all = append(all, e.Error())
			}
			joined := strings.Join(all, "\n")
			if tt.wantErr == "" {
				if joined != "" {
					t.Fatalf("Validate = %s, want none", joined)
				}
				return
			}
			if !strings.Contains(joined, tt.wantErr) {
				t.Fatalf("Validate = %q, want an error containing %q", joined, tt.wantErr)
			}
			field := "nats.embedded.advertise_url"
			if strings.HasPrefix(tt.name, "external") {
				field = "nats.external.advertise_urls"
			}
			if !strings.Contains(joined, field) {
				t.Fatalf("Validate = %q, want the message to name %q", joined, field)
			}
		})
	}
}

// TestValidate_AdvertisedURLSafety covers the URL discovery would publish
// over mDNS: it must carry no credentials, fit the nats_url TXT record and
// name a real port. With nats.external.advertise_urls empty that URL is
// nats.external.urls[0], so the checks apply to it too, but only while
// discovery is on; credentials elsewhere in urls are never advertised.
func TestValidate_AdvertisedURLSafety(t *testing.T) {
	long := "nats://" + strings.Repeat("h", 250) + ":4222"
	tests := []struct {
		name      string
		mutate    func(*config.Config)
		wantErr   string // empty = valid
		wantField string
	}{
		{"external: user and password in urls[0], no advertise_urls", func(c *config.Config) {
			c.NATS.External.URLs = []string{"nats://sqi:hunter2@a:4222"}
		}, "advertise_urls", "nats.external.urls"},
		{"external: token in urls[0], no advertise_urls", func(c *config.Config) {
			c.NATS.External.URLs = []string{"nats://s3cr3tt0ken@a:4222"}
		}, "advertise_urls", "nats.external.urls"},
		{"external: credentials in urls[0] with a clean advertise_urls", func(c *config.Config) {
			c.NATS.External.URLs = []string{"nats://sqi:hunter2@a:4222"}
			c.NATS.External.AdvertiseURLs = []string{"nats://public:4222"}
		}, "", ""},
		{"external: credentials in urls[0] with discovery off", func(c *config.Config) {
			c.Discovery.Enabled = false
			c.NATS.External.URLs = []string{"nats://sqi:hunter2@a:4222"}
		}, "", ""},
		{"external: credentials only in urls[1]", func(c *config.Config) {
			c.NATS.External.URLs = []string{"nats://a:4222", "nats://sqi:hunter2@b:4222"}
		}, "", ""},
		{"external: credentials in an advertise_urls entry", func(c *config.Config) {
			c.NATS.External.AdvertiseURLs = []string{"nats://public:4222", "nats://s3cr3tt0ken@public2:4222"}
		}, "must not carry credentials", "nats.external.advertise_urls"},
		{"external: credentials in advertise_urls with discovery off", func(c *config.Config) {
			c.Discovery.Enabled = false
			c.NATS.External.AdvertiseURLs = []string{"nats://sqi:hunter2@public:4222"}
		}, "must not carry credentials", "nats.external.advertise_urls"},
		{"external: urls[0] too long for the record, no advertise_urls", func(c *config.Config) {
			c.NATS.External.URLs = []string{long}
		}, "255-byte DNS TXT limit", "nats.external.urls"},
		{"external: urls[0] too long but advertise_urls set", func(c *config.Config) {
			c.NATS.External.URLs = []string{long}
			c.NATS.External.AdvertiseURLs = []string{"nats://public:4222"}
		}, "", ""},
		{"external: urls[0] too long with discovery off", func(c *config.Config) {
			c.Discovery.Enabled = false
			c.NATS.External.URLs = []string{long}
		}, "", ""},
		{"external: urls[0] port 0, no advertise_urls", func(c *config.Config) {
			c.NATS.External.URLs = []string{"nats://a:0"}
		}, "port 0", "nats.external.urls"},
		{"external: advertise_urls port 0", func(c *config.Config) {
			c.NATS.External.AdvertiseURLs = []string{"nats://public:0"}
		}, "port 0", "nats.external.advertise_urls"},
		{"external: advertise_urls without a port", func(c *config.Config) {
			c.NATS.External.AdvertiseURLs = []string{"nats://public"}
		}, "", ""},
		{"embedded: credentials in advertise_url", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "nats://sqi:hunter2@nats.example.com:4222"
		}, "must not carry credentials", "nats.embedded.advertise_url"},
		{"embedded: token in advertise_url", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "nats://s3cr3tt0ken@nats.example.com:4222"
		}, "must not carry credentials", "nats.embedded.advertise_url"},
		{"embedded: advertise_url port 0", func(c *config.Config) {
			c.NATS.Embedded.AdvertiseURL = "nats://nats.example.com:0"
		}, "port 0", "nats.embedded.advertise_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Discovery.Enabled = true
			if strings.HasPrefix(tt.name, "external") {
				cfg.NATS.Mode = config.NATSModeExternal
				cfg.NATS.External.URLs = []string{"nats://a:4222"}
			}
			tt.mutate(&cfg)
			var all []string
			for _, e := range config.Validate(cfg) {
				all = append(all, e.Error())
			}
			joined := strings.Join(all, "\n")
			for _, secret := range []string{"hunter2", "s3cr3tt0ken"} {
				if strings.Contains(joined, secret) {
					t.Fatalf("Validate repeats the credential %q:\n%s", secret, joined)
				}
			}
			if tt.wantErr == "" {
				if joined != "" {
					t.Fatalf("Validate = %s, want none", joined)
				}
				return
			}
			if !strings.Contains(joined, tt.wantErr) || !strings.Contains(joined, tt.wantField) {
				t.Fatalf("Validate = %q, want an error on %s containing %q", joined, tt.wantField, tt.wantErr)
			}
		})
	}
}

// TestValidate_BrokerURLErrorsRedactCredentials checks that an invalid
// nats.external.urls entry is never echoed with its credentials.
func TestValidate_BrokerURLErrorsRedactCredentials(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Discovery.Enabled = false
	cfg.NATS.Mode = config.NATSModeExternal
	cfg.NATS.External.URLs = []string{"http://sqi:hunter2@a:4222", "nats://s3cr3tt0ken@b"}
	var all []string
	for _, e := range config.Validate(cfg) {
		all = append(all, e.Error())
	}
	joined := strings.Join(all, "\n")
	if !strings.Contains(joined, "nats.external.urls") {
		t.Fatalf("Validate = %q, want an error on nats.external.urls", joined)
	}
	for _, secret := range []string{"hunter2", "s3cr3tt0ken"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("Validate repeats the credential %q:\n%s", secret, joined)
		}
	}
}

func TestCheckFileKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string // each must appear in the error; none: the keys are valid
	}{
		{"valid", "http:\n  enable_pprof: true\nscheduler:\n  heartbeat_timeout: 45s\n", nil},
		{"unknown", "scheduler:\n  heartbeat_timout: 1s\n", []string{"unknown key scheduler.heartbeat_timout"}},
		{"section where a value belongs", "http:\n  enable_pprof:\n    x: 1\n", []string{"http.enable_pprof"}},
		{"value where a section belongs", "http: true\n", []string{"http"}},
		{"removed nats key", "nats:\n  data_dir: x\n", []string{"nats.embedded.data_dir"}},
		{"every unknown key is named", "bogus: 1\nhttp:\n  nope: 2\n", []string{"unknown key bogus", "unknown key http.nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := config.CheckFileKeys([]byte(tt.body))
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("CheckFileKeys: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckFileKeys accepted %q", tt.body)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}
