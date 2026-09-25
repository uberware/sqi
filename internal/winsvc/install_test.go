// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validSpec(t *testing.T) InstallSpec {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Program Files", "sqi")
	return InstallSpec{
		Name:             "sqi-worker",
		DisplayName:      "sqi Worker Agent",
		Description:      "sqi distributed task worker",
		ExePath:          filepath.Join(dir, "sqi-worker.exe"),
		RunVerb:          "start",
		ConfigPath:       filepath.Join(dir, "conf dir", "sqi-worker.yaml"),
		DelayedAutoStart: true,
		DrainTimeout:     30 * time.Second,
	}
}

func TestBuildServiceConfig_Args(t *testing.T) {
	spec := validSpec(t)
	got, err := BuildServiceConfig(spec)
	if err != nil {
		t.Fatal(err)
	}
	// mgr.CreateService quotes ExePath and escapes each arg, so paths with
	// spaces are passed as separate, unquoted args here.
	want := []string{"start", "--config", spec.ConfigPath}
	if !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("Args = %q, want %q", got.Args, want)
	}
	if got.ExePath != spec.ExePath || got.Name != "sqi-worker" || !got.DelayedAutoStart {
		t.Fatalf("config = %+v", got)
	}
}

func TestBuildServiceConfig_RecoveryAndPreShutdown(t *testing.T) {
	got, err := BuildServiceConfig(validSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{5 * time.Second, 30 * time.Second, 60 * time.Second}; !reflect.DeepEqual(got.Recovery, want) {
		t.Errorf("Recovery = %v, want %v", got.Recovery, want)
	}
	if got.RecoveryReset != 24*time.Hour {
		t.Errorf("RecoveryReset = %v", got.RecoveryReset)
	}
	if got.PreShutdownTimeout != 45*time.Second {
		t.Errorf("PreShutdownTimeout = %v, want drain 30s + 15s margin", got.PreShutdownTimeout)
	}
}

func TestBuildServiceConfig_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*InstallSpec)
		want   string
	}{
		{"empty name", func(s *InstallSpec) { s.Name = "" }, "name"},
		{"slash in name", func(s *InstallSpec) { s.Name = `a\b` }, "name"},
		{"relative exe", func(s *InstallSpec) { s.ExePath = "sqi-worker.exe" }, "absolute"},
		{"relative config", func(s *InstallSpec) { s.ConfigPath = "sqi-worker.yaml" }, "absolute"},
		{"no verb", func(s *InstallSpec) { s.RunVerb = "" }, "verb"},
		{"account without password", func(s *InstallSpec) { s.Account = `.\render` }, "password"},
		{"zero drain", func(s *InstallSpec) { s.DrainTimeout = 0 }, "drain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec(t)
			tc.mutate(&spec)
			_, err := BuildServiceConfig(spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeAccount(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"LocalSystem":       "",
		"localsystem":       "",
		"render":            `.\render`,
		`.\render`:          `.\render`,
		`STUDIO\render`:     `STUDIO\render`,
		"render@studio.lan": "render@studio.lan",
	} {
		if got := NormalizeAccount(in); got != want {
			t.Errorf("NormalizeAccount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveConfigPath(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)

	if _, err := ResolveConfigPath("", "sqi-worker"); err == nil ||
		!strings.Contains(err.Error(), filepath.Join(pd, "sqi", "sqi-worker.yaml")) ||
		!strings.Contains(err.Error(), "sqi-worker config print") {
		t.Fatalf("missing default config err = %v; must name the path and the config print remedy", err)
	}

	cfg := filepath.Join(t.TempDir(), "w.yaml")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveConfigPath(cfg, "sqi-worker")
	if err != nil || got != cfg || !filepath.IsAbs(got) {
		t.Fatalf("ResolveConfigPath(existing) = %q, %v", got, err)
	}

	t.Chdir(filepath.Dir(cfg))
	got, err = ResolveConfigPath("w.yaml", "sqi-worker")
	if err != nil || got != cfg {
		t.Fatalf("relative flag resolved to %q, %v; want absolute %q", got, err, cfg)
	}
}

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TailLines(path, 2); !reflect.DeepEqual(got, []string{"c", "d"}) {
		t.Errorf("TailLines = %q", got)
	}
	if got := TailLines(filepath.Join(t.TempDir(), "none.log"), 5); got != nil {
		t.Errorf("TailLines(missing) = %q, want nil", got)
	}
}
