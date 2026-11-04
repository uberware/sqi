// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
)

func TestHAConfig_Defaults(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.HA.Enabled {
		t.Error("ha.enabled: default must be false")
	}
	if cfg.HA.LeaseTTL != 15*time.Second {
		t.Errorf("ha.lease_ttl: got %s, want 15s", cfg.HA.LeaseTTL)
	}
	if cfg.HA.InstanceName != "" {
		t.Errorf("ha.instance_name: got %q, want empty", cfg.HA.InstanceName)
	}
}

func TestHAConfig_FileAndEnv(t *testing.T) {
	writeFile := func(t *testing.T, body string) string {
		t.Helper()
		f := filepath.Join(t.TempDir(), "sqi-server.yaml")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}

	t.Run("file", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := writeFile(t, "ha:\n  enabled: true\n  lease_ttl: 30s\n  instance_name: sched-a\n")
		cfg, err := config.Load(f, config.FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.HA.Enabled || cfg.HA.LeaseTTL != 30*time.Second || cfg.HA.InstanceName != "sched-a" {
			t.Errorf("ha = %+v, want enabled, 30s, sched-a", cfg.HA)
		}
	})

	t.Run("partial file keeps defaults", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := writeFile(t, "ha:\n  enabled: true\n")
		cfg, err := config.Load(f, config.FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HA.LeaseTTL != 15*time.Second {
			t.Errorf("ha.lease_ttl: got %s, want the 15s default", cfg.HA.LeaseTTL)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := writeFile(t, "ha:\n  enabled: false\n  lease_ttl: 30s\n  instance_name: from-file\n")
		t.Setenv("SQI_HA_ENABLED", "true")
		t.Setenv("SQI_HA_LEASE_TTL", "20s")
		t.Setenv("SQI_HA_INSTANCE_NAME", "from-env")
		cfg, err := config.Load(f, config.FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.HA.Enabled || cfg.HA.LeaseTTL != 20*time.Second || cfg.HA.InstanceName != "from-env" {
			t.Errorf("ha = %+v, want enabled, 20s, from-env", cfg.HA)
		}
	})

	t.Run("malformed env is a load error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("SQI_HA_LEASE_TTL", "soon")
		if _, err := config.Load("", config.FlagOverrides{}); err == nil ||
			!strings.Contains(err.Error(), "SQI_HA_LEASE_TTL") {
			t.Errorf("Load error = %v, want one naming SQI_HA_LEASE_TTL", err)
		}
	})

	t.Run("malformed file duration is a load error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := writeFile(t, "ha:\n  lease_ttl: 15\n")
		if _, err := config.Load(f, config.FlagOverrides{}); err == nil {
			t.Error("Load accepted a bare-number lease_ttl")
		}
	})
}

func TestHAConfig_Name(t *testing.T) {
	if got := (config.HAConfig{InstanceName: "sched-a"}).Name(); got != "sched-a" {
		t.Errorf("Name() = %q, want the configured instance_name", got)
	}
	got := (config.HAConfig{}).Name()
	if got == "" {
		t.Fatal("Name() with no instance_name returned empty")
	}
	if host, err := os.Hostname(); err == nil && host != "" && got != host {
		t.Errorf("Name() = %q, want the hostname %q", got, host)
	}
}

func TestValidateHA(t *testing.T) {
	haValid := func() config.Config {
		cfg := config.DefaultConfig()
		cfg.HA.Enabled = true
		cfg.Store.Driver = config.StoreDriverPostgres
		cfg.Store.Postgres.DSN = "postgres://u:p@db:5432/sqi"
		cfg.NATS.Mode = config.NATSModeExternal
		cfg.NATS.External.URLs = []string{"nats://nats:4222"}
		return cfg
	}

	tests := []struct {
		name      string
		mutate    func(*config.Config)
		wantField string   // empty = no ha.* error expected
		wantInMsg []string // substrings of the message
	}{
		{name: "enabled with postgres and external nats", mutate: func(*config.Config) {}},
		{
			name:      "enabled with sqlite",
			mutate:    func(c *config.Config) { c.Store.Driver = config.StoreDriverSQLite },
			wantField: "ha.enabled",
			wantInMsg: []string{"store.driver: postgres", "got sqlite", "SQI_STORE_DRIVER"},
		},
		{
			name:      "enabled with empty store driver",
			mutate:    func(c *config.Config) { c.Store.Driver = "" },
			wantField: "ha.enabled",
			wantInMsg: []string{"store.driver: postgres"},
		},
		{
			name:      "enabled with embedded nats",
			mutate:    func(c *config.Config) { c.NATS.Mode = config.NATSModeEmbedded },
			wantField: "ha.enabled",
			wantInMsg: []string{"nats.mode: external", "got embedded", "SQI_NATS_MODE"},
		},
		{
			name:      "lease_ttl below minimum",
			mutate:    func(c *config.Config) { c.HA.LeaseTTL = 5 * time.Second },
			wantField: "ha.lease_ttl",
			wantInMsg: []string{"must be between 6s and 1m0s, got 5s", "SQI_HA_LEASE_TTL", "ha.lease_ttl"},
		},
		{
			name:      "lease_ttl above maximum",
			mutate:    func(c *config.Config) { c.HA.LeaseTTL = 61 * time.Second },
			wantField: "ha.lease_ttl",
			wantInMsg: []string{"must be between 6s and 1m0s, got 1m1s"},
		},
		{
			name:      "lease_ttl zero",
			mutate:    func(c *config.Config) { c.HA.LeaseTTL = 0 },
			wantField: "ha.lease_ttl",
			wantInMsg: []string{"got 0s"},
		},
		{name: "lease_ttl at minimum", mutate: func(c *config.Config) { c.HA.LeaseTTL = config.MinHALeaseTTL }},
		{name: "lease_ttl at maximum", mutate: func(c *config.Config) { c.HA.LeaseTTL = config.MaxHALeaseTTL }},
		{
			name: "disabled ignores everything under ha",
			mutate: func(c *config.Config) {
				*c = config.DefaultConfig()
				c.HA.LeaseTTL = time.Second
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := haValid()
			tc.mutate(&cfg)
			var got []config.ValidationError
			for _, e := range config.Validate(cfg) {
				if strings.HasPrefix(e.Field, "ha.") {
					got = append(got, e)
				}
			}
			if tc.wantField == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected ha errors: %v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Field != tc.wantField {
				t.Fatalf("ha errors = %v, want exactly one on %s", got, tc.wantField)
			}
			for _, s := range tc.wantInMsg {
				if !strings.Contains(got[0].Message, s) {
					t.Errorf("message %q missing %q", got[0].Message, s)
				}
			}
		})
	}
}

func TestValidateHA_ReportsBothConflicts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.HA.Enabled = true
	n := 0
	for _, e := range config.Validate(cfg) {
		if e.Field == "ha.enabled" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("got %d ha.enabled errors on the default store and broker, want 2 (one per conflicting key)", n)
	}
}
