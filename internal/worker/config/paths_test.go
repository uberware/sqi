// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/worker/commands"
)

// detectedApps is the reason the capability detectors' paths are not checked.
const detectedApps = "detected application install locations are the operator's; on a worker with " +
	"no run_as_user queue these run as the service account (documented gap)"

// pathFieldExclusions are the path-valued WorkerConfig fields NamedPaths
// deliberately leaves out, by field path, each with the reason.
var pathFieldExclusions = map[string]string{
	"Capabilities.Detect[].Checks[].Exe":             detectedApps,
	"Capabilities.Detect[].Checks[].PathGlob":        detectedApps,
	"Capabilities.Detect[].Command.Globs[].PathGlob": detectedApps,
}

// TestNamedPaths_CoverEveryPathField pins that every path-valued field of
// WorkerConfig reaches NamedPaths, which service install checks on Windows,
// or is excluded above with a reason. A field is path-valued by name: File,
// or a name ending in File, Dir, Path or Glob, or Exe; and a string or
// []string ending in Command, whose program is the path. A field added later
// fails here until it is checked or excluded.
func TestNamedPaths_CoverEveryPathField(t *testing.T) {
	var cfg WorkerConfig
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
			t.Errorf("exclusion %s is not a path-valued WorkerConfig field; remove it", field)
		}
	}
}

func TestNamedPaths_SkipsEmpty(t *testing.T) {
	var cfg WorkerConfig
	cfg.Log.File = `C:\sqi\logs\w.log`
	cfg.Worker.Power.SleepCommand = []string{"rundll32.exe", "powrprof.dll,SetSuspendState"}
	cfg.Staging.SyncCommand = "robocopy {src} {dest}"
	cfg.Worker.Commands = commands.Table{"blender": {Path: "blender-4.2", Args: []string{"-b"}}}
	want := []NamedPath{{"log.file", `C:\sqi\logs\w.log`}}
	if got := cfg.NamedPaths(); !slices.Equal(got, want) {
		t.Errorf("NamedPaths() = %q, want %q (empty keys and commands found on PATH are skipped)", got, want)
	}
}

func TestNamedPaths_Keys(t *testing.T) {
	cfg := WorkerConfig{
		Worker: WorkerSettings{
			DataDir: "g", SessionDir: "h",
			Commands: commands.Table{
				"nuke":    {Path: `C:\Program Files\Nuke15.1v1\Nuke15.1.exe`, Args: []string{"--nc"}},
				"blender": {Path: "blender"},
				"houdini": {Path: "/opt/hfs/bin/hython"},
			},
			Power: PowerConfig{SleepCommand: []string{`C:\bin\sleep.exe`, "-s"}, ShutdownCommand: []string{"tools/off"}},
		},
		NATS: NATSConfig{
			TLSCertFile: "a", TLSKeyFile: "b", TLSCAFile: "c",
			CredentialFile: "d", JoinTokenFile: "e", ServerTLSCAFile: "f",
		},
		Staging: StagingConfig{ScratchDir: "i", SyncCommand: `C:\bin\sync.exe {src} {dest}`},
		Metrics: MetricsConfig{TLS: MetricsTLSConfig{CertFile: "j", KeyFile: "k"}},
		Log:     LogConfig{File: "l"},
	}
	want := []NamedPath{
		{"nats.tls_cert_file", "a"},
		{"nats.tls_key_file", "b"},
		{"nats.tls_ca_file", "c"},
		{"nats.credential_file", "d"},
		{"nats.join_token_file", "e"},
		{"nats.server_tls_ca_file", "f"},
		{"worker.data_dir", "g"},
		{"worker.session_dir", "h"},
		{"staging.scratch_dir", "i"},
		{"metrics.tls.cert_file", "j"},
		{"metrics.tls.key_file", "k"},
		{"log.file", "l"},
		{"staging.sync_command", `C:\bin\sync.exe`},
		{"worker.power.sleep_command", `C:\bin\sleep.exe`},
		{"worker.power.shutdown_command", "tools/off"},
		{"worker.commands.houdini.path", "/opt/hfs/bin/hython"},
		{"worker.commands.nuke.path", `C:\Program Files\Nuke15.1v1\Nuke15.1.exe`},
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
