// SPDX-License-Identifier: AGPL-3.0-or-later

package presetgen_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/internal/presetgen"
)

// TestBuild_IgnoresAppleDoubleSidecars pins that a macOS AppleDouble sidecar
// in the presets directory is not read as a preset.
//
// macOS writes "._name" alongside a file whenever extended attributes cannot
// be stored natively, which is the case on non-APFS volumes. A plain `git
// checkout` that rewrites a preset is enough to create one: the rewritten file
// gets a com.apple.provenance xattr, and the xattr lands in the sidecar.
//
// The sidecar matches *.yaml, so without this filter Build reads it, fails to
// parse it ("control characters are not allowed"), and returns a hard error --
// meaning a release-time publish breaks on a developer machine for a reason
// that has nothing to do with the presets. This is not hypothetical: it
// happened in this repo and broke `make ci` for every package that globs the
// presets directory.
func TestBuild_IgnoresAppleDoubleSidecars(t *testing.T) {
	dir := t.TempDir()

	// One real preset, copied from the authored set.
	authored, err := os.ReadFile(filepath.Join(presetsDir, "blender-batch-render.yaml"))
	if err != nil {
		t.Fatalf("read source preset: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blender-batch-render.yaml"), authored, 0o600); err != nil {
		t.Fatalf("write preset: %v", err)
	}

	// The sidecar. Real ones begin with the AppleDouble magic number and are
	// full of NUL bytes, which is exactly what makes the YAML parser reject
	// them -- so use bytes of that shape rather than something that might
	// accidentally parse.
	sidecar := append([]byte{0x00, 0x05, 0x16, 0x07}, make([]byte, 128)...)
	if err := os.WriteFile(filepath.Join(dir, "._blender-batch-render.yaml"), sidecar, 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	got, err := presetgen.Build(dir, "sqi")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d presets, want 1: the AppleDouble sidecar was read as a preset", len(got))
	}
	if got[0].Entry.Name != "blender-batch-render" {
		t.Errorf("name = %q, want blender-batch-render", got[0].Entry.Name)
	}
}
