// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/presettest"
)

func TestLoadCases_ReadsParamsPatchAndExpectations(t *testing.T) {
	cases, err := presettest.LoadCases(filepath.Join("testdata", "cases-valid.yaml"))
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("cases = %d, want 2", len(cases))
	}
	if cases[0].Name != "default" || cases[0].Params["Frames"] != "1-3" {
		t.Errorf("case 0 = %+v", cases[0])
	}
	if cases[0].ExpectTasks != 3 || cases[0].ExpectInvocations["renderer"] != 3 {
		t.Errorf("case 0 expectations = %+v", cases[0])
	}
	if cases[0].Patch != nil {
		t.Errorf("case 0 should carry no patch, got %+v", cases[0].Patch)
	}
	if cases[1].Patch == nil || cases[1].Patch.Step != "Render" || cases[1].Patch.ChunksDefaultTaskCount != 5 {
		t.Errorf("case 1 patch = %+v", cases[1].Patch)
	}
}

func TestLoadCases_RejectsDuplicateAndEmptyNames(t *testing.T) {
	dir := t.TempDir()
	dup := filepath.Join(dir, "dup.yaml")
	writeFile(t, dup, "cases:\n  - name: a\n  - name: a\n")
	if _, err := presettest.LoadCases(dup); err == nil {
		t.Error("LoadCases: want error for duplicate case names, got nil")
	}
	blank := filepath.Join(dir, "blank.yaml")
	writeFile(t, blank, "cases:\n  - params: {}\n")
	if _, err := presettest.LoadCases(blank); err == nil {
		t.Error("LoadCases: want error for an unnamed case, got nil")
	}
	empty := filepath.Join(dir, "empty.yaml")
	writeFile(t, empty, "cases: []\n")
	if _, err := presettest.LoadCases(empty); err == nil {
		t.Error("LoadCases: want error for a file with no cases, got nil")
	}
}

// TestPresetTemplate_LoadsBothSources proves the harness can reach a preset
// file and a compiled-in built-in product through one call, because the tier
// registry covers both and a survey of presets/ alone gives a wrong answer
// about what sqi ships.
func TestPresetTemplate_LoadsBothSources(t *testing.T) {
	tmpl, err := presettest.PresetTemplate(presettest.SourcePresets, "maya-layer-render")
	if err != nil {
		t.Fatalf("PresetTemplate(presets): %v", err)
	}
	if !strings.Contains(tmpl, "command: Render") {
		t.Errorf("maya-layer-render template missing its command:\n%s", tmpl)
	}
	builtin, err := presettest.PresetTemplate(presettest.SourceBuiltins, "script")
	if err != nil {
		t.Fatalf("PresetTemplate(builtins): %v", err)
	}
	if !strings.Contains(builtin, "/bin/sh") {
		t.Errorf("script built-in template missing /bin/sh:\n%s", builtin)
	}
	if _, err := presettest.PresetTemplate(presettest.SourcePresets, "no-such-preset"); err == nil {
		t.Error("PresetTemplate: want error for an unknown preset, got nil")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
