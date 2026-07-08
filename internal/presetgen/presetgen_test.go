// SPDX-License-Identifier: AGPL-3.0-or-later

package presetgen_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/internal/presetgen"
	"github.com/uberware/sqi/internal/presetlib"
)

const presetsDir = "../../presets/dcc"

func TestBuild(t *testing.T) {
	got, err := presetgen.Build(presetsDir, "dcc")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d presets, want 4", len(got))
	}
	// Sorted by name: blender, houdini, maya, nuke.
	want := []string{"blender-batch-render", "houdini-rop-render", "maya-batch-render", "nuke-write-render"}
	for i, w := range want {
		if got[i].Entry.Name != w {
			t.Fatalf("got[%d].Name = %q, want %q", i, got[i].Entry.Name, w)
		}
	}

	blender := got[0]
	if blender.Entry.Title != "Blender Batch Render" {
		t.Errorf("title = %q, want Blender Batch Render", blender.Entry.Title)
	}
	if blender.Entry.Category != "Rendering" {
		t.Errorf("category = %q, want Rendering", blender.Entry.Category)
	}
	if blender.Entry.Definition != "dcc/blender-batch-render.yaml" {
		t.Errorf("definition = %q, want dcc/blender-batch-render.yaml", blender.Entry.Definition)
	}
	if blender.Path != "dcc/blender-batch-render.yaml" {
		t.Errorf("path = %q, want dcc/blender-batch-render.yaml", blender.Path)
	}

	// SHA-256 must match the raw file bytes exactly (integrity guarantee).
	raw, err := os.ReadFile(filepath.Join(presetsDir, "blender-batch-render.yaml"))
	if err != nil {
		t.Fatalf("read preset: %v", err)
	}
	sum := sha256.Sum256(raw)
	if blender.Entry.Sha256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want %q", blender.Entry.Sha256, hex.EncodeToString(sum[:]))
	}
	if string(blender.Data) != string(raw) {
		t.Error("published Data does not equal the raw preset file bytes")
	}
}

func TestMergePreservesForeignAndReplacesOwned(t *testing.T) {
	existing := []byte(`{"presets":[
		{"name":"community-thing","definition":"community/thing.yaml","sha256":"aa"},
		{"name":"stale-dcc","definition":"dcc/stale.yaml","sha256":"bb"}
	]}`)
	generated := []presetgen.Generated{
		{Entry: presetlib.IndexEntry{Name: "blender-batch-render", Definition: "dcc/blender-batch-render.yaml", Sha256: "cc"}},
	}
	idx, err := presetgen.Merge(existing, generated, "dcc")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	names := make([]string, len(idx.Presets))
	for i, e := range idx.Presets {
		names[i] = e.Name
	}
	// Foreign entry survives; stale dcc entry replaced; sorted by name.
	want := []string{"blender-batch-render", "community-thing"}
	if len(names) != len(want) {
		t.Fatalf("merged names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("merged names = %v, want %v", names, want)
		}
	}
}

func TestMergeEmptyExisting(t *testing.T) {
	generated := []presetgen.Generated{
		{Entry: presetlib.IndexEntry{Name: "x", Definition: "dcc/x.yaml"}},
	}
	idx, err := presetgen.Merge(nil, generated, "dcc")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(idx.Presets) != 1 || idx.Presets[0].Name != "x" {
		t.Fatalf("merged = %+v, want single entry x", idx.Presets)
	}
}

func TestPublish(t *testing.T) {
	out := t.TempDir()
	mustMkdir(t, filepath.Join(out, "community"))
	mustMkdir(t, filepath.Join(out, "dcc"))
	mustWrite(t, filepath.Join(out, "community", "thing.yaml"), "x")
	mustWrite(t, filepath.Join(out, "dcc", "gone.yaml"), "old") // stale, no longer a source preset
	seed := `{"presets":[{"name":"community-thing","definition":"community/thing.yaml","sha256":"aa"},` +
		`{"name":"gone","definition":"dcc/gone.yaml","sha256":"bb"}]}`
	mustWrite(t, filepath.Join(out, "index.json"), seed)

	if err := presetgen.Publish(presetsDir, out, "dcc"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, err := os.Stat(filepath.Join(out, "dcc", "gone.yaml")); !os.IsNotExist(err) {
		t.Error("stale dcc/gone.yaml should have been removed")
	}
	if _, err := os.Stat(filepath.Join(out, "community", "thing.yaml")); err != nil {
		t.Error("foreign community/thing.yaml should be preserved")
	}
	if _, err := os.Stat(filepath.Join(out, "dcc", "blender-batch-render.yaml")); err != nil {
		t.Error("dcc/blender-batch-render.yaml should be published")
	}

	var idx presetgen.Index
	data, err := os.ReadFile(filepath.Join(out, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("parse published index: %v", err)
	}
	byName := map[string]bool{}
	for _, e := range idx.Presets {
		byName[e.Name] = true
	}
	if !byName["community-thing"] {
		t.Error("published index dropped the foreign community entry")
	}
	if byName["gone"] {
		t.Error("published index kept the stale dcc entry")
	}
	if !byName["blender-batch-render"] {
		t.Error("published index missing blender-batch-render")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
