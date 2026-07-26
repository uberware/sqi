// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/openjd"
)

// The test-job presets (presets/testing/) are authored in this repo and
// published to the preset library under the "testing" namespace. This test
// pins them to the real schema. It is intentionally separate from
// TestDCCReferencePresets, which enforces render-only rules (Rendering
// category, TASK_CHUNKING) that do not apply to test jobs.
func TestTestingPresets(t *testing.T) {
	sharedParams := []string{
		"FrameRange", "SleepSeconds", "EmitProgress",
		"FailFrames", "HangFrames", "HangSeconds", "OutputDir",
	}
	want := map[string][]string{
		"test-render-bash":       sharedParams,
		"test-render-powershell": sharedParams,
		"test-steps-bash":        sharedParams,
		"test-steps-powershell":  sharedParams,
	}
	paths, err := fsutil.Glob(filepath.Join("..", "..", "presets", "testing", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) != len(want) {
		t.Fatalf("expected %d testing presets, found %d: %v", len(want), len(paths), paths)
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
			if p.Category != "Testing" {
				t.Errorf("category = %q, want Testing", p.Category)
			}
			if err := ValidateTemplate(p.Template, p.Format, true); err != nil {
				t.Fatalf("ValidateTemplate: %v", err)
			}
			params, ok := want[name]
			if !ok {
				t.Fatalf("unexpected preset %q (add it to want)", name)
			}
			for _, param := range params {
				if !strings.Contains(p.Template, "name: "+param) {
					t.Errorf("template missing parameter %q", param)
				}
			}

			// Every task's onRun action must carry the per-task timeout
			// guardrail (the preset descriptions promise a 120 s cap that
			// Hang Frames exercise). A wrong or misspelled OpenJD key
			// (e.g. timeoutSeconds instead of timeout) parses to 0 and
			// silently drops the guardrail, so assert the parsed value.
			pf := openjd.FormatYAML
			tmpl, err := openjd.Parse([]byte(p.Template), pf)
			if err != nil {
				t.Fatalf("openjd.Parse: %v", err)
			}
			for _, step := range tmpl.Steps {
				if step.Script == nil {
					continue
				}
				if got := step.Script.Actions.OnRun.TimeoutSeconds; got != 120 {
					t.Errorf("step %q onRun timeout = %d, want 120", step.Name, got)
				}
			}
		})
	}
}
