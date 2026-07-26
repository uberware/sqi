// SPDX-License-Identifier: AGPL-3.0-or-later

package fsutil_test

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/uberware/sqi/internal/fsutil"
)

func TestIsAppleDouble(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"companion, bare name", "._render.yaml", true},
		{"companion, full path", "presets/sqi/._render.yaml", true},
		{"companion, windows separators", `presets\sqi\._render.yaml`, true},
		{"companion with no extension", "._DS_Store", true},
		{"ordinary preset", "render.yaml", false},
		{"ordinary preset in a directory", "presets/sqi/render.yaml", false},
		{"underscore prefix is not a companion", "_render.yaml", false},
		{"dot prefix alone is not a companion", ".render.yaml", false},
		{"period inside the name", "render._backup.yaml", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fsutil.IsAppleDouble(tt.path); got != tt.want {
				t.Errorf("IsAppleDouble(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestFilterPaths(t *testing.T) {
	got := fsutil.FilterPaths([]string{
		"presets/a.yaml",
		"presets/._a.yaml",
		"presets/b.yaml",
		"presets/._b.yaml",
	})
	want := []string{"presets/a.yaml", "presets/b.yaml"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFilterPaths_Empty(t *testing.T) {
	if got := fsutil.FilterPaths(nil); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// HideAppleDouble must hide companions from BOTH listing and opening: a caller
// that already holds a name (from a manifest, say) must not be able to read one
// either, or the guard only half-works.
func TestHideAppleDouble(t *testing.T) {
	inner := fstest.MapFS{
		"dir/real.yaml":    {Data: []byte("name: real\n")},
		"dir/._real.yaml":  {Data: []byte("\x00\x05binary junk")},
		"dir/other.yaml":   {Data: []byte("name: other\n")},
		"dir/._other.yaml": {Data: []byte("\x00\x05binary junk")},
	}
	hidden := fsutil.HideAppleDouble(inner)

	entries, err := fs.ReadDir(hidden, "dir")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("ReadDir returned %v, want only the two real files", names)
	}

	if _, err := hidden.Open("dir/real.yaml"); err != nil {
		t.Errorf("Open on a real file failed: %v", err)
	}
	if _, err := hidden.Open("dir/._real.yaml"); err == nil {
		t.Error("Open on an AppleDouble companion succeeded; it must report ErrNotExist")
	}
}
