// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"reflect"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/config/pathseedtest"
)

// pathFieldExclusions are the path-valued Config fields NamedPaths
// leaves out, by field path, each with the reason. It is empty: every
// path-valued field is checked.
var pathFieldExclusions = map[string]string{}

// TestNamedPaths_CoverEveryPathField pins that every path-valued field of
// Config reaches NamedPaths, which service install checks on Windows, or is
// excluded above with a reason. A field is path-valued by name: File, or a
// name ending in File, Dir, Path or Glob, or Exe; and a string or []string
// ending in Command, whose program is the path. A field added later fails
// here until it is checked or excluded.
func TestNamedPaths_CoverEveryPathField(t *testing.T) {
	var cfg Config
	pathseedtest.CheckCoverage(t, reflect.ValueOf(&cfg).Elem(), "Config", pathFieldExclusions, func() []string {
		var paths []string
		for _, p := range cfg.NamedPaths() {
			paths = append(paths, p.Path)
		}
		return paths
	})
}

func TestNamedPaths_SkipsEmpty(t *testing.T) {
	var cfg Config
	cfg.Store.SQLitePath = "sqi.db"
	cfg.Capacity.Providers = map[string]CapacityProviderConfig{"wol": {Command: []string{"wake.exe", "--all"}}, "none": {}}
	want := []NamedPath{{"store.sqlite_path", "sqi.db"}}
	if got := cfg.NamedPaths(); !slices.Equal(got, want) {
		t.Errorf("NamedPaths() = %q, want %q (empty keys and commands found on PATH are skipped)", got, want)
	}
}

func TestNamedPaths_Keys(t *testing.T) {
	var cfg Config
	cfg.HTTP.TLS = TLSConfig{CertFile: "a", KeyFile: "b"}
	cfg.NATS.Embedded.DataDir = "c"
	cfg.NATS.Embedded.TLS = NATSTLSConfig{CertFile: "d", KeyFile: "e", ClientCAFile: "f"}
	cfg.NATS.External.NkeySeedFile = "g"
	cfg.NATS.External.CredsFile = "h"
	cfg.NATS.External.TLS = NATSClientTLSConfig{CAFile: "i", CertFile: "j", KeyFile: "k"}
	cfg.NATS.External.Callout = NATSCalloutConfig{NkeySeedFile: "l", IssuerSeedFile: "m", SystemCredsFile: "n"}
	cfg.Store.SQLitePath = "o"
	cfg.Log.File = "p"
	cfg.Auth.LDAP.CAFile = "q"
	cfg.Capacity.Providers = map[string]CapacityProviderConfig{
		"zeta":  {Command: []string{`C:\sqi\zeta.exe`, "up"}},
		"alpha": {Command: []string{"scripts/alpha.sh"}},
	}
	want := []NamedPath{
		{"http.tls.cert_file", "a"},
		{"http.tls.key_file", "b"},
		{"nats.embedded.data_dir", "c"},
		{"nats.embedded.tls.cert_file", "d"},
		{"nats.embedded.tls.key_file", "e"},
		{"nats.embedded.tls.client_ca_file", "f"},
		{"nats.external.nkey_seed_file", "g"},
		{"nats.external.creds_file", "h"},
		{"nats.external.tls.ca_file", "i"},
		{"nats.external.tls.cert_file", "j"},
		{"nats.external.tls.key_file", "k"},
		{"nats.external.callout.nkey_seed_file", "l"},
		{"nats.external.callout.issuer_seed_file", "m"},
		{"nats.external.callout.system_creds_file", "n"},
		{"store.sqlite_path", "o"},
		{"log.file", "p"},
		{"auth.ldap.ca_file", "q"},
		{"capacity.providers.alpha.command", "scripts/alpha.sh"},
		{"capacity.providers.zeta.command", `C:\sqi\zeta.exe`},
	}
	if got := cfg.NamedPaths(); !slices.Equal(got, want) {
		t.Errorf("NamedPaths():\n got %q\nwant %q", got, want)
	}
}
