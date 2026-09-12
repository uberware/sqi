// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

// StubPackage is the import path of the recording stub binary.
const StubPackage = "github.com/uberware/sqi/test/stubproc"

var (
	stubOnce sync.Once
	stubPath string
	stubDir  string
	errStub  error
)

// BuildStub compiles the stub binary once per process and returns its path.
//
// The directory is deliberately NOT a testing.TempDir: the build is cached
// across every test in the run, so no single test may own it. Callers that want
// it removed should register [RemoveStub] in TestMain.
func BuildStub(ctx context.Context) (string, error) {
	stubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sqi-stubproc-*")
		if err != nil {
			errStub = fmt.Errorf("presettest: temp dir for stub: %w", err)
			return
		}
		stubDir = dir
		out := filepath.Join(dir, "stubproc"+exeSuffix())
		cmd := exec.CommandContext(ctx, "go", "build", "-o", out, StubPackage)
		if combined, err := cmd.CombinedOutput(); err != nil {
			errStub = fmt.Errorf("presettest: build stub: %w\n%s", err, combined)
			return
		}
		stubPath = out
	})
	return stubPath, errStub
}

// RemoveStub deletes the cached stub build directory.
func RemoveStub() {
	if stubDir != "" {
		_ = os.RemoveAll(stubDir) // best-effort temp cleanup
	}
}

// InstallStub copies the built stub into dir once per name in names, so a
// worker with dir on its PATH resolves each of those commands to the stub.
//
// Copies rather than symlinks: the stub identifies itself by argv[0]'s basename,
// and a symlink resolves differently across platforms and shells.
func InstallStub(dir string, names []string) error {
	src, err := BuildStub(context.Background())
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("presettest: read stub: %w", err)
	}
	for _, name := range names {
		dst := filepath.Join(dir, name+exeSuffix())
		if err := os.WriteFile(dst, data, 0o755); err != nil { //nolint:gosec // a test stub must be executable
			return fmt.Errorf("presettest: install stub as %s: %w", name, err)
		}
	}
	return nil
}

// CommandNames returns the distinct basenames of every task's command in s,
// sorted. These are the names [InstallStub] must shadow for a Tier-3 run.
//
// A command that is a materialized embedded-file path ("<WORKDIR>/main.sh")
// yields that file's basename, which cannot be shadowed on PATH — the caller
// checks for that and asserts on the script's INNER invocations instead.
func CommandNames(s Snapshot) []string {
	seen := map[string]bool{}
	for _, step := range s.Steps {
		for _, task := range step.Tasks {
			if task.Command == "" {
				continue
			}
			seen[filepath.Base(task.Command)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// exeSuffix is ".exe" on Windows and "" elsewhere.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
