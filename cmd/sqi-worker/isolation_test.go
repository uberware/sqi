// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
)

func TestWorkerExitsWhenIsolationRequiredButNotCapable(t *testing.T) {
	cfg := workerconfig.IsolationConfig{Required: true}
	provider := isolation.NewFakeIncapable(nil) // Capable() returns ErrNotCapable

	err := verifyIsolationCapability(cfg, provider)

	if !errors.Is(err, isolation.ErrNotCapable) {
		t.Errorf("err = %v, want ErrNotCapable so the worker refuses to start", err)
	}
}

func TestWorkerStartsWhenIsolationNotRequired(t *testing.T) {
	cfg := workerconfig.IsolationConfig{Required: false}
	provider := isolation.NewFakeIncapable(nil)

	if err := verifyIsolationCapability(cfg, provider); err != nil {
		t.Errorf("err = %v, want nil — an unconfigured worker must still start", err)
	}
}

func TestWorkerStartsWhenIsolationRequiredAndCapable(t *testing.T) {
	cfg := workerconfig.IsolationConfig{Required: true}
	provider := isolation.NewFake(nil) // Capable() returns nil

	if err := verifyIsolationCapability(cfg, provider); err != nil {
		t.Errorf("err = %v, want nil when the provider reports capable", err)
	}
}

// ── effectiveSessionRoot ────────────────────────────────────────────────────

func TestEffectiveSessionRoot_ExplicitOverrideAlwaysWins(t *testing.T) {
	cfg := workerconfig.WorkerSettings{DataDir: "/data", SessionDir: "/custom/sessions"}

	for _, isRoot := range []bool{true, false} {
		got, mode := effectiveSessionRootFor(cfg, func() bool { return isRoot })
		if got != "/custom/sessions" {
			t.Errorf("isRoot=%v: got %q, want the explicit override unchanged", isRoot, got)
		}
		if mode != 0o711 {
			t.Errorf("isRoot=%v: mode = %o, want 0711 (an explicit override opts into traversable-from-birth)", isRoot, mode)
		}
	}
}

// TestEffectiveSessionRoot_RootDefaultsToPlatformRoot compares against
// defaultSessionRoot() itself, not a hardcoded path: that function is
// per-GOOS (sessionroot_unix.go / sessionroot_windows.go), and isRoot()=true
// forces this branch on every platform, so the assertion holds regardless of
// which one the test binary was built for.
func TestEffectiveSessionRoot_RootDefaultsToPlatformRoot(t *testing.T) {
	cfg := workerconfig.WorkerSettings{DataDir: "/data"}

	got, mode := effectiveSessionRootFor(cfg, func() bool { return true })

	wantRoot, wantMode := defaultSessionRoot()
	if got != wantRoot {
		t.Errorf("got %q, want %q (data_dir must never be reused as the session root)", got, wantRoot)
	}
	if mode != wantMode {
		t.Errorf("mode = %o, want %o (traversable from birth — the only worker kind that can actually isolate)", mode, wantMode)
	}
}

// TestEffectiveSessionRoot_NonRootFallsBackUnderDataDir is POSIX-only: on
// Windows, effectiveSessionRootFor always takes the defaultSessionRoot()
// branch regardless of isRoot(), because executor.IsRunningAsRoot() is
// hardcoded false there and Windows isolation capability is a privilege
// check, not a uid check — see effectiveSessionRoot's own doc.
func TestEffectiveSessionRoot_NonRootFallsBackUnderDataDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the DataDir fallback this test exercises is unreachable on Windows: " +
			"effectiveSessionRootFor takes the defaultSessionRoot() branch unconditionally there")
	}

	cfg := workerconfig.WorkerSettings{DataDir: "/data"}

	got, mode := effectiveSessionRootFor(cfg, func() bool { return false })

	want := filepath.Join("/data", "sessions")
	if got != want {
		t.Errorf("got %q, want %q — the pre-split location, so non-root deployments are byte-for-byte unaffected", got, want)
	}
	if mode != 0o750 {
		t.Errorf("mode = %o, want 0750 — the pre-split mode; a non-root worker can never actually isolate, "+
			"so it gains nothing from the wider 0711 and should not silently get `other +x`", mode)
	}
}

// TestDefaultSessionRootNeverNestsUnderDefaultDataDir guards against the
// self-inflicted boot refusal a prior round introduced: defaultSessionRoot()
// (/var/lib/sqi-worker-sessions on POSIX; ProgramData\sqi\worker\sessions on
// Windows) must never be a descendant of workerconfig.Default().Worker.DataDir,
// in EITHER the HOME-set or HOME-unset case. workerconfig.LoadOrCreateWorkerID
// creates DataDir at 0700 by design; if the session root nested under it,
// this package's own boot-time isolation.ValidateTraversable would walk up
// from the session root, hit DataDir at 0700, and refuse to start over a
// directory sqi itself had just created — naming the exact self-inflicted bug
// an earlier revision of the POSIX default (/var/lib/sqi-worker/sessions,
// nesting under the HOME-unset DataDir fallback /var/lib/sqi-worker)
// introduced.
func TestDefaultSessionRootNeverNestsUnderDefaultDataDir(t *testing.T) {
	cases := []struct {
		name string
		home string
	}{
		{"HOME set", t.TempDir()},
		// os.UserHomeDir treats an empty HOME identically to an unset one
		// (both fail its `Getenv(env) != ""` check), so t.Setenv("HOME", "")
		// exercises the HOME-unset branch of workerconfig.defaultDataDir
		// without needing an unsettable-env workaround.
		{"HOME unset", ""},
	}
	sessionRoot, _ := defaultSessionRoot()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", tc.home)

			dataDir := workerconfig.Default().Worker.DataDir
			if isDescendantPath(dataDir, sessionRoot) {
				t.Errorf("defaultSessionRoot() %q is a descendant of the default data_dir %q — "+
					"LoadOrCreateWorkerID creates data_dir 0700, and a session root nested under it "+
					"would refuse to boot when isolation.required is set (see ValidateTraversable)",
					sessionRoot, dataDir)
			}
		})
	}
}

// isDescendantPath reports whether path is ancestor itself or nested under it.
func isDescendantPath(ancestor, path string) bool {
	ancestor = filepath.Clean(ancestor)
	path = filepath.Clean(path)
	return path == ancestor || strings.HasPrefix(path, ancestor+string(filepath.Separator))
}

// ── validateIsolationAncestors ──────────────────────────────────────────────

func TestValidateIsolationAncestors_SkippedWhenNotRequired(t *testing.T) {
	// isolation.required=false must never cause a boot refusal over directory
	// permissions, even on a worker that COULD isolate (root): root and
	// will-actually-isolate are not the same predicate, and there is nothing
	// an operator could act on via chmod for a queue that isn't isolated in
	// the first place. The per-assignment check (session.Manager.Create /
	// staging.Stager.StageIn) covers this worker if an isolated assignment
	// ever actually arrives.
	if err := validateIsolationAncestors(false, "/some/traversal-guarded/path", ""); err != nil {
		t.Errorf("err = %v, want nil when isolation.required is false", err)
	}
}

func TestValidateIsolationAncestors_FailsOnNonTraversableAncestorWhenRequired(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("isolation.ValidateTraversable is a deliberate no-op on Windows (see its own " +
			"doc): NTFS access control is ACL-based, not POSIX permission-bit-based, so there " +
			"is no ancestor \"traversable bit\" for this test's narrow, 0700 ancestor to defeat — " +
			"validateIsolationAncestors proceeds past the check this test exists to prove fails")
	}

	dir := t.TempDir()
	narrow := filepath.Join(dir, "narrow")
	sessionRoot := filepath.Join(narrow, "sessions")
	if err := os.Mkdir(narrow, 0o700); err != nil {
		t.Fatalf("set up narrow ancestor: %v", err)
	}

	err := validateIsolationAncestors(true, sessionRoot, "")

	if err == nil {
		t.Fatal("expected an error naming the non-traversable ancestor; got nil")
	}
}
