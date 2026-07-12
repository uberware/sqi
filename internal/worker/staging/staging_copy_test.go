// SPDX-License-Identifier: AGPL-3.0-or-later

package staging

import (
	"context"
	"os"
	"path/filepath"
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
		if err := builtinCopy(ctx, src, dest, "FILE"); err != nil {
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
		if err := builtinCopy(ctx, src, dest, "DIRECTORY"); err != nil {
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
		if err := builtinCopy(ctx, filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "x"), "FILE"); err == nil {
			t.Fatal("want error for missing src")
		}
	})
}
