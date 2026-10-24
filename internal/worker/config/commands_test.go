// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/worker/commands"
)

// hostAbsSlash returns an absolute path valid on this host, spelled with
// forward slashes so it can sit in a YAML scalar without escaping.
func hostAbsSlash(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(abs)
}

func TestLoad_WorkerCommands(t *testing.T) {
	nuke := hostAbsSlash(t, "Nuke17.1")
	f := writeTempYAML(t, `
nats:
  url: "nats://localhost:4222"
worker:
  commands:
    nuke:
      path: "`+nuke+`"
      args: ["--nc"]
    ffmpeg:
      path: ffmpeg-7
`)

	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.Worker.Commands["nuke"]
	if got.Path != nuke || !slices.Equal(got.Args, []string{"--nc"}) {
		t.Errorf("commands.nuke = %+v, want path %q and args [--nc]", got, nuke)
	}
	if got := cfg.Worker.Commands["ffmpeg"]; got.Path != "ffmpeg-7" || len(got.Args) != 0 {
		t.Errorf("commands.ffmpeg = %+v, want a bare path and no args", got)
	}
}

func TestLoad_NoCommandsByDefault(t *testing.T) {
	f := writeTempYAML(t, "nats:\n  url: \"nats://localhost:4222\"\n")

	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Worker.Commands) != 0 {
		t.Errorf("Commands = %v, want none by default", cfg.Worker.Commands)
	}
}

func TestValidate_WorkerCommandsReportsTheFieldOfABadEntry(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.Commands = commands.Table{
		"nuke": {Path: ""},
		"ok":   {Path: hostAbsSlash(t, "tool")},
	}

	errs := Validate(cfg)

	if !containsField(errs, "worker.commands.nuke") {
		t.Errorf("want an error on worker.commands.nuke, got %v", errs)
	}
	if containsField(errs, "worker.commands.ok") {
		t.Errorf("a valid entry was reported: %v", errs)
	}
}

func TestValidate_WorkerCommandsAcceptsAValidTable(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.Commands = commands.Table{
		"nuke": {Path: hostAbsSlash(t, "Nuke17.1"), Args: []string{"--nc"}},
	}

	if errs := Validate(cfg); len(errs) != 0 {
		t.Errorf("Validate = %v, want none", errs)
	}
}
