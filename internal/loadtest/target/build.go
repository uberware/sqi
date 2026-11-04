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
)

const (
	modulePath = "github.com/uberware/sqi"
	stampFile  = ".sqi-load-build"
)

// buildPackages are the packages BuildBinaries builds, with the binary each
// becomes (go build names a binary after its package directory).
var buildPackages = []struct{ pkg, name string }{
	{"./cmd/sqi-server", "sqi-server"},
	{"./cmd/sqi-worker", "sqi-worker"},
	{"./internal/loadtest/target/noop", "noop"},
}

// BuildBinaries builds sqi-server, sqi-worker and the no-op task command into
// outDir with one go build, and returns their paths. A build of the same git
// revision with a clean working tree is reused; a dirty tree always
// rebuilds. It needs the sqi source: the module containing the working
// directory, or the one this package was compiled from.
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
	head, dirty := gitState(ctx, root)
	if !upToDate(outDir, head, dirty, names) {
		if err := os.Remove(filepath.Join(outDir, stampFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", "", fmt.Errorf("target: clear build stamp: %w", err)
		}
		if err := goBuild(ctx, root, outDir, pkgs); err != nil {
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
	status, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
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
