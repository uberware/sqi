// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"reflect"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/config/pathseedtest"
	"github.com/uberware/sqi/internal/worker/commands"
)

// detectedApps is the reason the capability detectors' paths are not checked.
const detectedApps = "detected application install locations are the operator's; on a worker with " +
	"no run_as_user queue these run as the service account (documented gap)"

// pathFieldExclusions are the path-valued WorkerConfig fields NamedPaths
// leaves out, by field path, each with the reason.
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
	pathseedtest.CheckCoverage(t, reflect.ValueOf(&cfg).Elem(), "WorkerConfig", pathFieldExclusions, func() []string {
		var paths []string
		for _, p := range cfg.NamedPaths() {
			paths = append(paths, p.Path)
		}
		return paths
	})
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
