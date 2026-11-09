// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/config"
)

// absCmd returns a path filepath.IsAbs accepts on the host running the test,
// so the validation rows pass on Windows and POSIX alike.
func absCmd(name string) string {
	if runtime.GOOS == "windows" {
		return `C:\sqi\providers\` + name
	}
	return "/opt/sqi/providers/" + name
}

// yamlQuote single-quotes s as a YAML scalar, so a Windows path's backslashes
// survive.
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func writeCapacityConfig(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "sqi-server.yaml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCapacityConfig_Defaults(t *testing.T) {
	cfg := config.DefaultConfig()
	if len(cfg.Capacity.Providers) != 0 {
		t.Errorf("capacity.providers = %v, want none", cfg.Capacity.Providers)
	}
	if cfg.Capacity.WakeOnLAN.Port != 9 || len(cfg.Capacity.WakeOnLAN.SendTo) != 0 {
		t.Errorf("capacity.wake_on_lan = %+v, want port 9 and no send_to", cfg.Capacity.WakeOnLAN)
	}
	if errs := config.Validate(cfg); len(errs) != 0 {
		t.Errorf("the default config must validate: %v", errs)
	}
}

func TestLoad_CapacityProviders(t *testing.T) {
	t.Chdir(t.TempDir())
	wake := absCmd("wake.sh")
	cool := absCmd("cool.sh")
	f := writeCapacityConfig(t, `capacity:
  providers:
    cloud:
      command: [`+yamlQuote(wake)+`, "--region", "eu"]
      env:
        API_TOKEN: s3cret
        REGION: eu
      timeout: 90s
    plain:
      command: [`+yamlQuote(cool)+`]
  wake_on_lan:
    port: 7
    send_to: ["192.168.1.255:9", "[fe80::1]:7"]
`)
	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if errs := config.Validate(cfg); len(errs) != 0 {
		t.Fatalf("Validate: %v", errs)
	}
	want := config.CapacityConfig{
		Providers: map[string]config.CapacityProviderConfig{
			"cloud": {
				Command: []string{wake, "--region", "eu"},
				Env:     map[string]string{"API_TOKEN": "s3cret", "REGION": "eu"},
				Timeout: 90 * time.Second,
			},
			// timeout omitted: the 2m default.
			"plain": {Command: []string{cool}, Timeout: 2 * time.Minute},
		},
		WakeOnLAN: config.WakeOnLANConfig{Port: 7, SendTo: []string{"192.168.1.255:9", "[fe80::1]:7"}},
	}
	if !reflect.DeepEqual(cfg.Capacity, want) {
		t.Errorf("capacity = %+v\nwant     %+v", cfg.Capacity, want)
	}
}

func TestLoad_CapacityPartialKeepsDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	f := writeCapacityConfig(t, "capacity:\n  wake_on_lan:\n    send_to: [\"10.0.0.255:9\"]\n")
	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Capacity.WakeOnLAN.Port != 9 {
		t.Errorf("wake_on_lan.port = %d, want the default 9", cfg.Capacity.WakeOnLAN.Port)
	}
}

func TestLoad_CapacityProviderInvalid(t *testing.T) {
	abs := absCmd("x.sh")
	okProvider := func() config.CapacityProviderConfig {
		return config.CapacityProviderConfig{Command: []string{abs}, Timeout: 2 * time.Minute}
	}
	with := func(name string, mut func(*config.CapacityProviderConfig)) func(*config.Config) {
		return func(c *config.Config) {
			p := okProvider()
			if mut != nil {
				mut(&p)
			}
			c.Capacity.Providers = map[string]config.CapacityProviderConfig{name: p}
		}
	}
	tests := []struct {
		name      string
		mutate    func(*config.Config)
		wantField string // empty: the config must be valid
	}{
		{"valid", with("aws", nil), ""},
		{"valid name with dots underscores and dashes", with("a.b_c-d9", nil), ""},
		{"name with a space", with("bad name", nil), "capacity.providers.bad name"},
		{"name starting with a dash", with("-lead", nil), "capacity.providers.-lead"},
		{"empty name", with("", nil), "capacity.providers."},
		{"name over 63 characters", with(strings.Repeat("a", 64), nil), "capacity.providers." + strings.Repeat("a", 64)},
		{"no command", with("p", func(p *config.CapacityProviderConfig) { p.Command = nil }), "capacity.providers.p.command"},
		{"empty command", with("p", func(p *config.CapacityProviderConfig) { p.Command = []string{} }), "capacity.providers.p.command"},
		{"relative command", with("p", func(p *config.CapacityProviderConfig) { p.Command = []string{"wake.sh"} }), "capacity.providers.p.command[0]"},
		{"bare command name", with("p", func(p *config.CapacityProviderConfig) { p.Command = []string{"ipmitool", "x"} }), "capacity.providers.p.command[0]"},
		{"empty command[0]", with("p", func(p *config.CapacityProviderConfig) { p.Command = []string{""} }), "capacity.providers.p.command[0]"},
		{"timeout below 5s", with("p", func(p *config.CapacityProviderConfig) { p.Timeout = 4 * time.Second }), "capacity.providers.p.timeout"},
		{"timeout zero", with("p", func(p *config.CapacityProviderConfig) { p.Timeout = 0 }), "capacity.providers.p.timeout"},
		{"timeout above 30m", with("p", func(p *config.CapacityProviderConfig) { p.Timeout = 31 * time.Minute }), "capacity.providers.p.timeout"},
		{"timeout at 5s", with("p", func(p *config.CapacityProviderConfig) { p.Timeout = 5 * time.Second }), ""},
		{"timeout at 30m", with("p", func(p *config.CapacityProviderConfig) { p.Timeout = 30 * time.Minute }), ""},
		{"port 0", func(c *config.Config) { c.Capacity.WakeOnLAN.Port = 0 }, "capacity.wake_on_lan.port"},
		{"port 65536", func(c *config.Config) { c.Capacity.WakeOnLAN.Port = 65536 }, "capacity.wake_on_lan.port"},
		{"port 65535", func(c *config.Config) { c.Capacity.WakeOnLAN.Port = 65535 }, ""},
		{"send_to a hostname", func(c *config.Config) { c.Capacity.WakeOnLAN.SendTo = []string{"router.lan:9"} }, "capacity.wake_on_lan.send_to[0]"},
		{"send_to without a port", func(c *config.Config) { c.Capacity.WakeOnLAN.SendTo = []string{"10.0.0.255"} }, "capacity.wake_on_lan.send_to[0]"},
		{"send_to port out of range", func(c *config.Config) { c.Capacity.WakeOnLAN.SendTo = []string{"10.0.0.255:0"} }, "capacity.wake_on_lan.send_to[0]"},
		{"second send_to entry bad", func(c *config.Config) {
			c.Capacity.WakeOnLAN.SendTo = []string{"10.0.0.255:9", "nope:9"}
		}, "capacity.wake_on_lan.send_to[1]"},
		{"send_to IPv4 and IPv6 literals", func(c *config.Config) {
			c.Capacity.WakeOnLAN.SendTo = []string{"10.0.0.255:9", "[ff02::1]:9"}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			tt.mutate(&cfg)
			errs := config.Validate(cfg)
			if tt.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate = %v, want none", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Field == tt.wantField {
					return
				}
			}
			t.Fatalf("Validate = %v, want an error on %q", errs, tt.wantField)
		})
	}
}

func TestMarshalYAML_CapacityProviderEnvRedacted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Capacity.Providers = map[string]config.CapacityProviderConfig{
		"cloud": {
			Command: []string{absCmd("wake.sh"), "--flag"},
			Env:     map[string]string{"API_TOKEN": "hunter2-token", "REGION": "eu-west-1"},
			Timeout: 90 * time.Second,
		},
	}
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, leaked := range []string{"hunter2-token", "eu-west-1"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("marshaled config leaks the env value %q:\n%s", leaked, got)
		}
	}
	if strings.Count(got, "<redacted>") != 2 {
		t.Fatalf("want both env values redacted, got:\n%s", got)
	}
	// Names survive, so the operator can see what is set.
	for _, keep := range []string{"API_TOKEN", "REGION", "--flag"} {
		if !strings.Contains(got, keep) {
			t.Errorf("marshaled config lost %q:\n%s", keep, got)
		}
	}
	// Redaction must not touch the loaded value.
	if cfg.Capacity.Providers["cloud"].Env["API_TOKEN"] != "hunter2-token" {
		t.Fatal("MarshalYAML changed the loaded value")
	}
}

func TestLoad_CapacityUnknownKeyRejected(t *testing.T) {
	t.Chdir(t.TempDir())
	cmd := yamlQuote(absCmd("x.sh"))
	tests := []struct {
		name, body, want string
	}{
		{"capacity key", "capacity:\n  provider: {}\n", "unknown key capacity.provider"},
		{"provider key", "capacity:\n  providers:\n    a:\n      command: [" + cmd + "]\n      comand: x\n", "unknown key capacity.providers.a.comand"},
		{"wake_on_lan key", "capacity:\n  wake_on_lan:\n    prot: 9\n", "unknown key capacity.wake_on_lan.prot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeCapacityConfig(t, tt.body), config.FlagOverrides{})
			if err == nil {
				t.Fatalf("Load accepted %q", tt.body)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// capacity.* has no environment overlay: providers hold commands and secrets,
// so they live in the config file only.
func TestLoad_CapacityIgnoresEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_CAPACITY_WAKE_ON_LAN_PORT", "7")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Capacity.WakeOnLAN.Port != 9 {
		t.Errorf("wake_on_lan.port = %d, want 9: capacity.* has no env overlay", cfg.Capacity.WakeOnLAN.Port)
	}
}

func TestExampleConfig_CapacityBlockMatchesDefaults(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config", "sqi-server.example.yaml"), config.FlagOverrides{})
	if err != nil {
		t.Fatalf("the shipped example config must load: %v", err)
	}
	if want := config.DefaultConfig().Capacity; !reflect.DeepEqual(cfg.Capacity, want) {
		t.Errorf("example capacity block = %+v, want the defaults %+v", cfg.Capacity, want)
	}
}
