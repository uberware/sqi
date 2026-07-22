// SPDX-License-Identifier: AGPL-3.0-or-later

package staging

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuiltinCopy(t *testing.T) {
	ctx := context.Background()
	t.Run("single file", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "in.txt")
		if err := os.WriteFile(src, []byte("hello"), 0o640); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, "sub", "out.txt") // parent must be created
		if err := builtinCopy(ctx, src, dest); err != nil {
			t.Fatalf("copy: %v", err)
		}
		b, err := os.ReadFile(dest)
		if err != nil || string(b) != "hello" {
			t.Fatalf("content: %q err=%v", b, err)
		}
		fi, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Fatalf("mode: %v", fi.Mode().Perm())
		}
	})
	t.Run("directory tree", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "srcdir")
		if err := os.MkdirAll(filepath.Join(src, "nested"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "nested", "b.txt"), []byte("B"), 0o644); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, "destdir")
		if err := builtinCopy(ctx, src, dest); err != nil {
			t.Fatalf("copy: %v", err)
		}
		for rel, want := range map[string]string{"a.txt": "A", "nested/b.txt": "B"} {
			b, err := os.ReadFile(filepath.Join(dest, rel))
			if err != nil || string(b) != want {
				t.Fatalf("%s: %q err=%v", rel, b, err)
			}
		}
	})
	t.Run("missing src errors", func(t *testing.T) {
		if err := builtinCopy(ctx, filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "x")); err == nil {
			t.Fatal("want error for missing src")
		}
	})
	t.Run("refuses symlink src", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(target, []byte("secret"), 0o640); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, "out.txt")
		err := builtinCopy(ctx, link, dest)
		if err == nil {
			t.Fatal("want error refusing a symlinked source")
		}
		if _, statErr := os.Stat(dest); statErr == nil {
			t.Error("dest must not have been created from a symlinked source")
		}
	})
	t.Run("overwrites an existing dest", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "in.txt")
		if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, "out.txt")
		if err := os.WriteFile(dest, []byte("stale-longer-content"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := builtinCopy(ctx, src, dest); err != nil {
			t.Fatalf("copy: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || string(got) != "new" {
			t.Fatalf("dest = %q, err=%v; want %q (full overwrite, not a truncate-in-place leaving stale bytes)", got, err, "new")
		}
	})
}

// TestCopyFile_RefusesSymlinkDest proves copyFile never writes THROUGH a
// symlink planted at dest — the write half of the stage-out boundary that
// sqi's own code path can close (an operator-configured sync_command remains
// the operator's responsibility; see docs/worker-configuration.md). Per
// copyFile's doc (mirroring isolation.WriteFileFchown), the planted symlink
// entry is removed and a fresh regular file created in its place — root
// writes the new bytes to a brand-new inode at dest, never through the
// symlink to whatever it pointed at, so "elsewhere" must be completely
// untouched and dest must end up an ordinary file, not a dangling symlink.
func TestCopyFile_RefusesSymlinkDest(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(src, []byte("task-controlled-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(dir, "elsewhere.txt")
	if err := os.WriteFile(elsewhere, []byte("must-not-be-overwritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.txt")
	if err := os.Symlink(elsewhere, dest); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dest, 0o644); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(elsewhere)
	if err != nil || string(got) != "must-not-be-overwritten" {
		t.Fatalf("elsewhere = %q, err=%v; must be untouched, not written through", got, err)
	}
	if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("dest should be a fresh regular file (symlink entry replaced), got mode %v err=%v", fi.Mode(), err)
	}
	destContent, err := os.ReadFile(dest)
	if err != nil || string(destContent) != "task-controlled-bytes" {
		t.Fatalf("dest = %q, err=%v; want the copied content in the new inode", destContent, err)
	}
}

// TestCopyFile_RefusesSourceWithExtraHardlink proves copyFile's own fd-based
// re-check refuses a source with an extra hardlink ENTIRELY ON ITS OWN, with
// no upstream path-based guard (validateStageOutSource) involved at all.
// That is the point of the fix: O_NOFOLLOW closes the symlink half of the
// stage-out TOCTOU (a symlink swapped in after an upstream Lstat cannot be
// traversed), but a hardlink IS a regular file — O_NOFOLLOW opens it
// successfully — so nothing before this test's fd-based in.Stat() call would
// have refused it. A background child left running past its task's
// successful exit (nothing kills the process group on success — see
// executor.runTask/killAndWait) can link a second name onto a scratch entry's
// inode between validateStageOutSource's Lstat and copyFile's open; calling
// copyFile directly here, without going through StageOut/validateStageOutSource
// first, simulates exactly that: the source path already shares its inode
// with another entry by the time copyFile ever sees it.
func TestCopyFile_RefusesSourceWithExtraHardlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink-count check is unimplemented on Windows (see staging_windows.go); " +
			"task isolation itself is not yet supported there")
	}

	dir := t.TempDir()
	outside := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(outside, []byte("root-only-contents"), 0o644); err != nil {
		t.Fatal(err)
	}

	// src shares outside's inode — a hardlink is a regular file, so
	// O_NOFOLLOW alone would open it without complaint.
	src := filepath.Join(dir, "render.exr")
	if err := os.Link(outside, src); err != nil {
		t.Fatalf("create hardlink: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "render.exr")
	err := copyFile(src, dest, 0o644)
	if err == nil {
		t.Fatal("want error refusing a source with an extra hardlink opened by copyFile")
	}
	if !strings.Contains(err.Error(), "hardlink") {
		t.Errorf("err = %v, want it to mention the hardlink refusal", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("dest must not exist: the linked partner's contents must never reach it")
	}
}
