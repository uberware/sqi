// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

func TestBuiltinDetectors_LoadValidate(t *testing.T) {
	ds, err := BuiltinDetectors()
	if err != nil {
		t.Fatalf("BuiltinDetectors: %v", err)
	}
	got := map[string]bool{}
	for _, d := range ds {
		if err := d.Validate(); err != nil {
			t.Errorf("builtin %q invalid: %v", d.Tag, err)
		}
		got[d.Tag] = true
	}
	for _, want := range []string{"maya", "nuke", "houdini", "blender"} {
		if !got[want] {
			t.Errorf("missing built-in detector %q", want)
		}
	}
}

// TestBuiltinDetectors_CoverPresets enforces the paired-change convention: every
// software tag a shipped reference preset requires must be emitted by a built-in
// detector, so a new default preset cannot ship without its detector.
func TestBuiltinDetectors_CoverPresets(t *testing.T) {
	ds, err := BuiltinDetectors()
	if err != nil {
		t.Fatal(err)
	}
	detectorTags := map[string]bool{}
	for _, d := range ds {
		detectorTags[d.Tag] = true
	}
	// Extract base software tags required by presets/sqi/*.yaml, dropping any
	// "-<version>" suffix. Path is relative to this test file's package dir.
	root := filepath.Join("..", "..", "..", "presets", "sqi")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read presets dir: %v", err)
	}
	tagRe := regexp.MustCompile(`attr\.worker\.tag\.([A-Za-z0-9_.-]+)`)
	required := map[string]bool{}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tagRe.FindAllStringSubmatch(string(data), -1) {
			base := m[1]
			if i := regexp.MustCompile(`-[0-9].*$`).FindStringIndex(base); i != nil {
				base = base[:i[0]]
			}
			required[base] = true
		}
	}
	var missing []string
	for tag := range required {
		if !detectorTags[tag] {
			missing = append(missing, tag)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("reference presets require tags with no built-in detector: %v", missing)
	}
}

// TestBuiltinDetectors_FFmpeg pins the ffmpeg detector's shape. It is a PATH
// tool on every platform, so unlike the DCC detectors it carries no
// install-location glob and no registry probe -- if a later change adds one,
// this test should be the thing that asks why.
func TestBuiltinDetectors_FFmpeg(t *testing.T) {
	ds, err := BuiltinDetectors()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range ds {
		if d.Tag != "ffmpeg" {
			continue
		}
		found = true
		if len(d.Checks) != 1 {
			t.Fatalf("ffmpeg detector has %d checks, want 1 (exe only)", len(d.Checks))
		}
		if d.Checks[0].Exe != "ffmpeg" {
			t.Errorf("ffmpeg detector check = %+v, want exe: ffmpeg", d.Checks[0])
		}
	}
	if !found {
		t.Fatal("no built-in detector emits the ffmpeg tag")
	}
}
