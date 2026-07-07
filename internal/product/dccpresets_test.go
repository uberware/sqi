// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four DCC reference presets (presets/dcc/) are authored in this repo and
// published to the preset library; this test pins them to the real schema.
func TestDCCReferencePresets(t *testing.T) {
	want := map[string][]string{
		"maya-batch-render":    {"SceneFile", "Frames", "OutputDir", "Renderer", "RenderLayer"},
		"houdini-rop-render":   {"SceneFile", "Frames", "RopPath"},
		"nuke-write-render":    {"SceneFile", "Frames", "WriteNode"},
		"blender-batch-render": {"SceneFile", "Frames", "OutputPath"},
	}
	paths, err := filepath.Glob(filepath.Join("..", "..", "presets", "dcc", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) != len(want) {
		t.Fatalf("expected %d presets, found %d: %v", len(want), len(paths), paths)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			p, err := ParseDefinition(data)
			if err != nil {
				t.Fatalf("ParseDefinition: %v", err)
			}
			if p.Name != name {
				t.Errorf("name %q does not match file %q", p.Name, name)
			}
			if p.Category != "Rendering" {
				t.Errorf("category = %q, want Rendering", p.Category)
			}
			if err := ValidateTemplate(p.Template, p.Format, true); err != nil {
				t.Fatalf("ValidateTemplate: %v", err)
			}
			for _, param := range want[name] {
				if !strings.Contains(p.Template, "name: "+param) {
					t.Errorf("template missing convention parameter %q", param)
				}
			}
			if !strings.Contains(p.Template, "TASK_CHUNKING") {
				t.Errorf("template does not declare TASK_CHUNKING")
			}
		})
	}
}
