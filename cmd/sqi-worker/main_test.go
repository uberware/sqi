// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout to a pipe for the duration of fn, then
// returns everything written to it.  Required because the cobra commands in
// this binary write directly to os.Stdout via fmt.Printf / fmt.Fprint rather
// than through cmd.OutOrStdout().
//
// Must NOT be called from parallel sub-tests — the redirect is process-wide.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy from stdout pipe: %v", err)
	}
	r.Close()
	return buf.String()
}

// prepareRoot sets the args that rootCmd will parse on the next Execute() call
// and redirects cobra's own output writers (help, usage, error messages) to a
// discard buffer so test output stays clean.
func prepareRoot(args []string) {
	rootCmd.SetArgs(args)
	var sink bytes.Buffer
	rootCmd.SetOut(&sink)
	rootCmd.SetErr(&sink)
}

// TestExitOnErr_Nil verifies that exitOnErr is a no-op when passed nil, i.e.,
// it does not call os.Exit.
func TestExitOnErr_Nil(t *testing.T) {
	t.Run("nil error is no-op", func(_ *testing.T) {
		exitOnErr(nil) // must not call os.Exit
	})
}

// TestRootCmd_SubcommandTree exercises the cobra command tree without starting
// the worker, connecting to NATS, or binding any sockets.
func TestRootCmd_SubcommandTree(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "no args prints usage and exits cleanly",
			args:    []string{},
			wantErr: false,
		},
		{
			name:    "--help exits cleanly",
			args:    []string{"--help"},
			wantErr: false,
		},
		{
			name:    "unknown subcommand returns error",
			args:    []string{"bogus-subcommand"},
			wantErr: true,
		},
		{
			name:    "bad flag returns error",
			args:    []string{"--no-such-flag"},
			wantErr: true,
		},
		{
			name:    "bare config exits cleanly (prints usage)",
			args:    []string{"config"},
			wantErr: false,
		},
		{
			name:    "config --help exits cleanly",
			args:    []string{"config", "--help"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepareRoot(tt.args)
			err := Execute()
			if (err != nil) != tt.wantErr {
				t.Errorf("Execute(%v) error = %v; wantErr %v", tt.args, err, tt.wantErr)
			}
		})
	}
}

// TestVersionCmd verifies that the version subcommand prints the expected
// output label keys.  Values vary by build; only label presence is checked.
func TestVersionCmd(t *testing.T) {
	t.Run("prints version metadata labels to stdout", func(t *testing.T) {
		prepareRoot([]string{"version"})

		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(version) unexpected error: %v", err)
			}
		})

		for _, want := range []string{"sqi-worker", "commit:", "built:", "go:"} {
			if !strings.Contains(out, want) {
				t.Errorf("version output missing %q\ngot:\n%s", want, out)
			}
		}
	})
}

// TestConfigPrintCmd verifies that "config print" produces non-empty YAML
// containing a known top-level key from the worker configuration.
func TestConfigPrintCmd(t *testing.T) {
	t.Run("prints non-empty YAML with worker key", func(t *testing.T) {
		prepareRoot([]string{"config", "print"})

		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(config print) unexpected error: %v", err)
			}
		})

		if strings.TrimSpace(out) == "" {
			t.Fatal("config print produced empty output")
		}
		if !strings.Contains(out, "worker:") {
			t.Errorf("config print YAML missing 'worker:' key\ngot:\n%s", out)
		}
	})

	t.Run("log-level flag reaches flagOverrides via config print", func(t *testing.T) {
		// Run "config print" with an explicit --log-level flag so that
		// flagOverrides() sees pf.Changed("log-level") == true.  This covers
		// the Changed branch inside flagOverrides and verifies the override
		// propagates into the printed YAML.
		prepareRoot([]string{"--log-level", "debug", "config", "print"})
		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(--log-level debug config print) unexpected error: %v", err)
			}
		})
		// The YAML should reflect the overridden log level.
		if !strings.Contains(out, "debug") {
			t.Errorf("config print output should contain 'debug' level; got:\n%s", out)
		}
	})

	t.Run("log-format flag reaches flagOverrides via config print", func(t *testing.T) {
		// Exercise the pf.Changed("log-format") branch in flagOverrides().
		prepareRoot([]string{"--log-format", "text", "config", "print"})
		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(--log-format text config print) unexpected error: %v", err)
			}
		})
		if !strings.Contains(out, "text") {
			t.Errorf("config print output should contain 'text' format; got:\n%s", out)
		}
	})

	t.Run("non-existent config file returns load error", func(t *testing.T) {
		// Exercise the error-return branch in runConfigPrint when Load fails.
		// Restore the global config file path after this sub-test so it does not
		// bleed into later tests (pflag does not reset flag values between
		// Execute() calls).
		t.Cleanup(func() { persistentFlags.ConfigFile = "" })
		prepareRoot([]string{"--config", "/nonexistent/sqi-worker-test.yaml", "config", "print"})
		err := Execute()
		if err == nil {
			t.Fatal("expected error for non-existent config file, got nil")
		}
		if !strings.Contains(err.Error(), "load config") {
			t.Errorf("error should contain 'load config'; got: %v", err)
		}
	})
}

// TestStartCmd_DryRun exercises the "start --dry-run" code path, which resolves
// and prints effective configuration and detected capabilities without
// connecting to NATS or touching the filesystem.  This covers runStart (up to
// the dry-run branch), loadAndValidateConfig, and runDryRun.
//
// "start" without --dry-run is intentionally NOT tested because it blocks
// on a NATS connection and OS-signal wait.
func TestStartCmd_DryRun(t *testing.T) {
	t.Run("dry-run prints config and capabilities without connecting", func(t *testing.T) {
		prepareRoot([]string{"start", "--dry-run"})

		out := captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Errorf("Execute(start --dry-run) unexpected error: %v", err)
			}
		})

		if strings.TrimSpace(out) == "" {
			t.Fatal("start --dry-run produced empty output")
		}
		for _, want := range []string{
			"## Effective configuration",
			"## Detected capabilities",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("dry-run output missing %q\ngot:\n%s", want, out)
			}
		}
	})
}

// TestStartCmd_InvalidConfig verifies that start returns an error when given
// a config file path that does not exist, covering the load-error branch in
// loadAndValidateConfig.
func TestStartCmd_InvalidConfig(t *testing.T) {
	t.Run("non-existent config file returns load error", func(t *testing.T) {
		// Restore the global config file path after this sub-test so it does not
		// bleed into later tests (pflag does not reset flag values between
		// Execute() calls).
		t.Cleanup(func() { persistentFlags.ConfigFile = "" })
		prepareRoot([]string{"start", "--config", "/nonexistent/sqi-worker-test.yaml"})
		err := Execute()
		if err == nil {
			t.Fatal("expected error for non-existent config file, got nil")
		}
		if !strings.Contains(err.Error(), "load config") {
			t.Errorf("error should contain 'load config'; got: %v", err)
		}
	})
}

// TestCapabilitiesCmd_RunsAndPrints verifies that the "capabilities"
// diagnostic command runs without error.
func TestCapabilitiesCmd_RunsAndPrints(t *testing.T) {
	prepareRoot([]string{"capabilities"})
	if err := Execute(); err != nil {
		t.Fatalf("capabilities command failed: %v", err)
	}
}

// TestCapabilitiesCmd_ManualSuppression verifies that the "capabilities"
// command is authoritative: a manual tag suppression such as
// `capability_tags: ["testtag=false"]` must show up with its final value
// (false) rather than being silently reported as present, since a value the
// command doesn't show could still make a worker fail a preset's
// anyOf:["true"] gate at runtime.
func TestCapabilitiesCmd_ManualSuppression(t *testing.T) {
	t.Cleanup(func() { persistentFlags.ConfigFile = "" })

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sqi-worker.yaml")
	cfgYAML := `
worker:
  capability_tags:
    - "testtag=false"
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	prepareRoot([]string{"--config", cfgPath, "capabilities"})

	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Errorf("Execute(capabilities) unexpected error: %v", err)
		}
	})

	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !strings.Contains(lines[0], "TAG") ||
		!strings.Contains(lines[0], "VALUE") || !strings.Contains(lines[0], "SOURCE") {
		t.Errorf("capabilities output missing three-column header; got:\n%s", out)
	}

	found := false
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "testtag" {
			found = true
			if fields[1] != "false" {
				t.Errorf("testtag value: got %q, want %q\nfull output:\n%s", fields[1], "false", out)
			}
			if fields[2] != "manual" {
				t.Errorf("testtag source: got %q, want %q\nfull output:\n%s", fields[2], "manual", out)
			}
		}
	}
	if !found {
		t.Fatalf("capabilities output missing 'testtag' row; got:\n%s", out)
	}
}
