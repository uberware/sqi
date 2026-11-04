// SPDX-License-Identifier: AGPL-3.0-or-later

package target

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/fslock"
)

const (
	modulePath = "github.com/uberware/sqi"
	stampFile  = ".sqi-load-build"
	// lockFile serializes builds into one directory, between processes.
	lockFile = ".sqi-load-build.lock"
	// oldSuffix marks a binary moved aside by a rebuild; it is removed once
	// nothing runs it any more.
	oldSuffix = ".old-"
)

// lockPoll is how often a build waits to retry a held build lock. A variable
// so a test can shorten it.
var lockPoll = 200 * time.Millisecond

// buildPackages are the packages BuildBinaries builds, with the binary each
// becomes (go build names a binary after its package directory).
var buildPackages = []struct{ pkg, name string }{
	{"./cmd/sqi-server", "sqi-server"},
	{"./cmd/sqi-worker", "sqi-worker"},
	{"./internal/loadtest/target/noop", "noop"},
}

// BuildBinaries builds sqi-server, sqi-worker and the no-op task command into
// outDir with one go build, and returns their paths. A build of the same git
// revision with a clean working tree (untracked files do not count) is
// reused; a dirty tree always rebuilds. It needs the sqi source: the module
// containing the working directory, or the one this package was compiled
// from.
//
// Concurrent runs may share outDir: a lock file serializes their builds, and
// a rebuild goes to a scratch directory first and then moves each binary into
// place, moving the old one aside rather than overwriting it, since Windows
// refuses to overwrite an executable another run is still running.
func BuildBinaries(ctx context.Context, outDir string) (server, worker, noop string, err error) {
	root, err := moduleRoot(ctx)
	if err != nil {
		return "", "", "", err
	}
	outDir, err = filepath.Abs(outDir)
	if err != nil {
		return "", "", "", fmt.Errorf("target: build directory: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return "", "", "", fmt.Errorf("target: build directory: %w", err)
	}
	names := make([]string, len(buildPackages))
	pkgs := make([]string, len(buildPackages))
	for i, b := range buildPackages {
		names[i] = b.name + exeSuffix()
		pkgs[i] = b.pkg
	}
	lock, err := acquireBuildLock(ctx, outDir)
	if err != nil {
		return "", "", "", err
	}
	defer lock.Release() //nolint:errcheck // releasing a lock file cannot usefully fail the build
	removeOld(outDir)
	head, dirty := gitState(ctx, root)
	if !upToDate(outDir, head, dirty, names) {
		if err := rebuild(ctx, root, outDir, pkgs, names); err != nil {
			return "", "", "", err
		}
		if !dirty && head != "" {
			if err := writeStamp(outDir, head); err != nil {
				return "", "", "", err
			}
		}
	}
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(outDir, n)
		if _, err := os.Stat(paths[i]); err != nil {
			// Antivirus software has been seen quarantining freshly built
			// small Go binaries on Windows.
			return "", "", "", fmt.Errorf("target: %s is missing after the build (removed by antivirus?): %w", paths[i], err)
		}
	}
	return paths[0], paths[1], paths[2], nil
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// acquireBuildLock takes outDir's build lock, waiting while another process
// holds it, until ctx ends.
func acquireBuildLock(ctx context.Context, outDir string) (*fslock.Lock, error) {
	path := filepath.Join(outDir, lockFile)
	for {
		l, err := fslock.Acquire(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, fslock.ErrHeld) {
			return nil, fmt.Errorf("target: build lock: %w", err)
		}
		t := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("target: waiting for another run's build into %s: %w", outDir, ctx.Err())
		case <-t.C:
		}
	}
}

// rebuild builds pkgs into a scratch directory beside outDir's binaries and
// moves each into place.
func rebuild(ctx context.Context, root, outDir string, pkgs, names []string) error {
	if err := os.Remove(filepath.Join(outDir, stampFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("target: clear build stamp: %w", err)
	}
	tmp, err := os.MkdirTemp(outDir, ".build-")
	if err != nil {
		return fmt.Errorf("target: build directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := goBuild(ctx, root, tmp, pkgs); err != nil {
		return err
	}
	return install(tmp, outDir, names)
}

// install moves each named binary from dir into outDir. A binary already
// there is first renamed aside (Windows allows renaming, but not overwriting,
// a running executable) and removed when nothing runs it.
func install(dir, outDir string, names []string) error {
	for _, n := range names {
		dst := filepath.Join(outDir, n)
		if _, err := os.Stat(dst); err == nil {
			aside := fmt.Sprintf("%s%s%d", dst, oldSuffix, time.Now().UnixNano())
			if err := os.Rename(dst, aside); err != nil {
				return fmt.Errorf("target: move the old %s aside: %w", n, err)
			}
			// Fails while another run still runs it; removeOld retries later.
			os.Remove(aside)
		}
		if err := os.Rename(filepath.Join(dir, n), dst); err != nil {
			return fmt.Errorf("target: install %s: %w", n, err)
		}
	}
	return nil
}

// removeOld removes the binaries earlier rebuilds moved aside, as far as
// nothing still runs them.
func removeOld(outDir string) {
	old, err := filepath.Glob(filepath.Join(outDir, "*"+oldSuffix+"*"))
	if err != nil {
		return
	}
	for _, f := range old {
		os.Remove(f) // still running somewhere when it fails; a later build retries
	}
}

func goBuild(ctx context.Context, root, outDir string, pkgs []string) error {
	args := append([]string{"build", "-o", outDir + string(filepath.Separator)}, pkgs...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("target: go build: %w\n%s", err, out)
	}
	return nil
}

// upToDate reports whether outDir holds every binary in names, built from
// revision head of a clean tree.
func upToDate(outDir, head string, dirty bool, names []string) bool {
	if dirty || head == "" {
		return false
	}
	stamp, err := os.ReadFile(filepath.Join(outDir, stampFile))
	if err != nil || strings.TrimSpace(string(stamp)) != head {
		return false
	}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(outDir, n)); err != nil {
			return false
		}
	}
	return true
}

func writeStamp(outDir, head string) error {
	if err := os.WriteFile(filepath.Join(outDir, stampFile), []byte(head+"\n"), 0o600); err != nil {
		return fmt.Errorf("target: write build stamp: %w", err)
	}
	return nil
}

// gitState returns the HEAD revision of the repository at root and whether
// its working tree has changes. Without git, the revision is "" and the tree
// counts as dirty, so the binaries are always rebuilt.
func gitState(ctx context.Context, root string) (head string, dirty bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", true
	}
	head = strings.TrimSpace(string(out))
	// Untracked files do not count: they are not in the build unless a
	// package embeds them, and an editor's or a tool's directory (.claude/,
	// say) would otherwise force a rebuild on every run.
	status, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return head, true
	}
	return head, len(bytes.TrimSpace(status)) > 0
}

// moduleRoot finds the sqi module's root directory: the module containing
// the working directory, else the one this file was compiled from.
func moduleRoot(ctx context.Context) (string, error) {
	if out, err := exec.CommandContext(ctx, "go", "env", "GOMOD").Output(); err == nil {
		gomod := strings.TrimSpace(string(out))
		if isSqiModule(gomod) {
			return filepath.Dir(gomod), nil
		}
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
			if isSqiModule(filepath.Join(dir, "go.mod")) {
				return dir, nil
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return "", errors.New("target: cannot find the sqi source to build from; run from inside the repository, or pass the server and worker binaries")
}

// isSqiModule reports whether gomod is the go.mod of the sqi module.
func isSqiModule(gomod string) bool {
	f, err := os.Open(gomod)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if mod, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.TrimSpace(mod) == modulePath
		}
	}
	return false
}
