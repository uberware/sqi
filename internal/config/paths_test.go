// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// pathFieldExclusions are the path-valued Config fields NamedPaths
// deliberately leaves out, by field path, each with the reason. None today.
var pathFieldExclusions = map[string]string{}

// TestNamedPaths_CoverEveryPathField pins that every path-valued field of
// Config reaches NamedPaths, which service install checks on Windows, or is
// excluded above with a reason. A field is path-valued by name: File, or a
// name ending in File, Dir, Path or Glob, or Exe; and a string or []string
// ending in Command, whose program is the path. A field added later fails
// here until it is checked or excluded.
func TestNamedPaths_CoverEveryPathField(t *testing.T) {
	var cfg Config
	s := newPathSeeder()
	s.walk(reflect.ValueOf(&cfg).Elem(), "")
	got := map[string]bool{}
	for _, p := range cfg.NamedPaths() {
		got[p.Path] = true
	}
	for field, sentinel := range s.want {
		_, excluded := pathFieldExclusions[field]
		switch {
		case excluded && got[sentinel]:
			t.Errorf("%s is excluded but NamedPaths names it; remove the exclusion", field)
		case !excluded && !got[sentinel]:
			t.Errorf("%s holds a path and NamedPaths does not name it: add it to NamedPaths, "+
				"or to pathFieldExclusions with the reason it need not be checked", field)
		}
	}
	for field := range pathFieldExclusions {
		if _, ok := s.want[field]; !ok {
			t.Errorf("exclusion %s is not a path-valued Config field; remove it", field)
		}
	}
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

// pathSeeder sets every path-valued field it walks to a unique sentinel and
// records, by field path, the sentinel NamedPaths must return for it.
type pathSeeder struct {
	n    int
	want map[string]string
}

func newPathSeeder() *pathSeeder { return &pathSeeder{want: map[string]string{}} }

func (s *pathSeeder) sentinel(field string) string {
	s.n++
	v := fmt.Sprintf("/sentinel/%d", s.n)
	s.want[field] = v
	return v
}

// walk descends into v, giving each slice or map of structs one element.
func (s *pathSeeder) walk(v reflect.Value, path string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		s.walk(v.Elem(), path)
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				s.field(v.Field(i), f.Name, strings.TrimPrefix(path+"."+f.Name, "."))
			}
		}
	case reflect.Slice:
		if isStructLike(v.Type().Elem()) {
			e := reflect.New(v.Type().Elem()).Elem()
			s.walk(e, path+"[]")
			v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), e))
		}
	case reflect.Map:
		if isStructLike(v.Type().Elem()) {
			e := reflect.New(v.Type().Elem()).Elem()
			s.walk(e, path+"{}")
			m := reflect.MakeMap(v.Type())
			m.SetMapIndex(reflect.ValueOf("k").Convert(v.Type().Key()), e)
			v.Set(m)
		}
	default:
	}
}

func (s *pathSeeder) field(v reflect.Value, name, path string) {
	isString := v.Kind() == reflect.String
	isCommand := strings.HasSuffix(name, "Command")
	switch {
	case isString && isPathName(name):
		v.SetString(s.sentinel(path))
	case isString && isCommand:
		v.SetString(s.sentinel(path) + " {src} {dest}")
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String && isCommand:
		v.Set(reflect.ValueOf([]string{s.sentinel(path), "--arg"}).Convert(v.Type()))
	default:
		s.walk(v, path)
	}
}

func isPathName(name string) bool {
	if name == "File" || name == "Exe" {
		return true
	}
	for _, suffix := range []string{"File", "Dir", "Path", "Glob"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func isStructLike(t reflect.Type) bool {
	return t.Kind() == reflect.Struct || (t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct)
}
