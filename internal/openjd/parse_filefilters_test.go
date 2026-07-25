// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// fileFiltersYAML builds n distinct, individually-valid fileFilters entries
// for embedding under a PATH parameter's fileFilters key.
func fileFiltersYAML(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "      - label: F%d\n        patterns: [\"*.png\"]\n", i)
	}
	return b.String()
}

func TestParse_PathFileFilters(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: FilterJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    objectType: FILE
    dataFlow: IN
    userInterface: { control: CHOOSE_INPUT_FILE, label: Scene }
    fileFilters:
      - label: Image Files
        patterns: ["*.png", "*.exr"]
      - label: All Files
        patterns: ["*"]
    fileFilterDefault:
      label: Image Files
      patterns: ["*.png", "*.exr"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	if errs := openjd.Validate(tmpl); len(errs) > 0 {
		t.Fatalf("expected file filters to validate; got: %v", errs)
	}
	p := tmpl.ParameterDefinitions[0]
	if len(p.FileFilters) != 2 {
		t.Fatalf("FileFilters = %d, want 2", len(p.FileFilters))
	}
	if p.FileFilters[0].Label != "Image Files" {
		t.Errorf("filter label = %q, want %q", p.FileFilters[0].Label, "Image Files")
	}
	if len(p.FileFilters[0].Patterns) != 2 || p.FileFilters[0].Patterns[1] != "*.exr" {
		t.Errorf("patterns = %v, want [*.png *.exr]", p.FileFilters[0].Patterns)
	}
	if p.FileFilterDefault == nil || p.FileFilterDefault.Label != "Image Files" {
		t.Errorf("FileFilterDefault = %+v, want the Image Files filter", p.FileFilterDefault)
	}
}

func TestValidate_FileFiltersRejectedOnNonPath(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: BadFilterJob
parameterDefinitions:
  - name: Frames
    type: STRING
    fileFilters:
      - label: Image Files
        patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "fileFilters")
}

func TestValidate_FileFilterDefaultOnlyRejectedOnNonPath(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: BadFilterDefaultJob
parameterDefinitions:
  - name: Frames
    type: STRING
    fileFilterDefault:
      label: Image Files
      patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/parameterDefinitions/0/fileFilterDefault:")
}

func TestValidate_FileFilterRequiresPatterns(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: EmptyFilterJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    fileFilters:
      - label: Broken
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "patterns")
}

// ── Rule 1: label is required (structural) ─────────────────────────────────

func TestValidate_FileFilterLabelRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: NoLabelJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    userInterface: { control: CHOOSE_INPUT_FILE, label: Scene }
    fileFilters:
      - patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/parameterDefinitions/0/fileFilters/0/label")
}

func TestValidate_FileFilterDefaultLabelRequired(t *testing.T) {
	yaml := `
specificationVersion: jobtemplate-2023-09
name: NoDefaultLabelJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    userInterface: { control: CHOOSE_INPUT_FILE, label: Scene }
    fileFilterDefault:
      patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "/parameterDefinitions/0/fileFilterDefault/label")
}

// ── Rule 2: label capped at 64 characters (gated) ──────────────────────────

func TestValidate_FileFilterLabelLength(t *testing.T) {
	cases := []struct {
		name    string
		label   string
		wantErr bool
	}{
		{"exactly 64 accepted", strings.Repeat("a", 64), false},
		{"65 rejected", strings.Repeat("a", 65), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
name: LabelLenJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    userInterface: { control: CHOOSE_INPUT_FILE, label: Scene }
    fileFilters:
      - label: %s
        patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`, tc.label)
			tmpl := mustParse(t, yaml)
			errs := openjd.Validate(tmpl)
			got := containsPointer(errs, "/parameterDefinitions/0/fileFilters/0/label")
			if got != tc.wantErr {
				t.Errorf("label length %d: got error=%v, want error=%v; errs=%v", len(tc.label), got, tc.wantErr, errs)
			}
		})
	}
}

// ── Rule 3: maximum of 20 filters (gated) ──────────────────────────────────

func TestValidate_FileFilterMaxCount(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{"20 accepted", 20, false},
		{"21 rejected", 21, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
specificationVersion: jobtemplate-2023-09
name: CountJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    userInterface: { control: CHOOSE_INPUT_FILE, label: Scene }
    fileFilters:
` + fileFiltersYAML(tc.n) + `
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
			tmpl := mustParse(t, yaml)
			errs := openjd.Validate(tmpl)
			got := containsMessage(errs, "at most 20 file filters")
			if got != tc.wantErr {
				t.Errorf("%d filters: got error=%v, want error=%v; errs=%v", tc.n, got, tc.wantErr, errs)
			}
		})
	}
}

// ── Rule 4: filters valid only with CHOOSE_INPUT_FILE / CHOOSE_OUTPUT_FILE (structural) ──

func TestValidate_FileFilterControlPairing(t *testing.T) {
	cases := []struct {
		name    string
		control string
		wantErr bool
	}{
		{"CHOOSE_INPUT_FILE accepted", "CHOOSE_INPUT_FILE", false},
		{"CHOOSE_OUTPUT_FILE accepted", "CHOOSE_OUTPUT_FILE", false},
		{"CHOOSE_DIRECTORY rejected", "CHOOSE_DIRECTORY", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
name: ControlPairJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    userInterface: { control: %s, label: Scene }
    fileFilters:
      - label: Image Files
        patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`, tc.control)
			tmpl := mustParse(t, yaml)
			errs := openjd.Validate(tmpl)
			got := containsMessage(errs, "CHOOSE_INPUT_FILE or CHOOSE_OUTPUT_FILE")
			if got != tc.wantErr {
				t.Errorf("control %s: got error=%v, want error=%v; errs=%v", tc.control, got, tc.wantErr, errs)
			}
		})
	}
}

func TestValidate_FileFilterControlPairing_AbsentControlRejected(t *testing.T) {
	// A PATH parameter with fileFilters but no userInterface at all: absent is
	// not one of the two file-choosing controls, so it is invalid.
	yaml := `
specificationVersion: jobtemplate-2023-09
name: NoControlJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    fileFilters:
      - label: Image Files
        patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)
	assertValidationContains(t, tmpl, "CHOOSE_INPUT_FILE or CHOOSE_OUTPUT_FILE")
}

// ── Gated (2, 3) vs structural (1, 4): EnforceLimits must only skip the gated ones ──

func TestValidateWithOptions_FileFilters_GatedVsStructural(t *testing.T) {
	t.Run("label length gated, label-required structural", func(t *testing.T) {
		yaml := `
specificationVersion: jobtemplate-2023-09
name: GateLabelJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    userInterface: { control: CHOOSE_INPUT_FILE, label: Scene }
    fileFilters:
      - label: ""
        patterns: ["*.png"]
      - label: ` + strings.Repeat("a", 65) + `
        patterns: ["*.png"]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
		tmpl := mustParse(t, yaml)

		withFalse := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
		if containsPointer(withFalse, "/parameterDefinitions/0/fileFilters/1/label") {
			t.Errorf("EnforceLimits=false should not report the 64-char label cap; got %v", withFalse)
		}
		if !containsPointer(withFalse, "/parameterDefinitions/0/fileFilters/0/label") {
			t.Errorf("EnforceLimits=false should still report the required-label structural error; got %v", withFalse)
		}

		withTrue := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
		if !containsPointer(withTrue, "/parameterDefinitions/0/fileFilters/1/label") {
			t.Errorf("EnforceLimits=true should report the 64-char label cap; got %v", withTrue)
		}
		if !containsPointer(withTrue, "/parameterDefinitions/0/fileFilters/0/label") {
			t.Errorf("EnforceLimits=true should still report the required-label structural error; got %v", withTrue)
		}
	})

	t.Run("max filter count gated, control pairing structural", func(t *testing.T) {
		yaml := `
specificationVersion: jobtemplate-2023-09
name: GateCountJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    userInterface: { control: CHOOSE_DIRECTORY, label: Scene }
    fileFilters:
` + fileFiltersYAML(21) + `
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
		tmpl := mustParse(t, yaml)

		withFalse := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
		if containsMessage(withFalse, "at most 20 file filters") {
			t.Errorf("EnforceLimits=false should not report the 20-filter cap; got %v", withFalse)
		}
		if !containsMessage(withFalse, "CHOOSE_INPUT_FILE or CHOOSE_OUTPUT_FILE") {
			t.Errorf("EnforceLimits=false should still report the control-pairing structural error; got %v", withFalse)
		}

		withTrue := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
		if !containsMessage(withTrue, "at most 20 file filters") {
			t.Errorf("EnforceLimits=true should report the 20-filter cap; got %v", withTrue)
		}
		if !containsMessage(withTrue, "CHOOSE_INPUT_FILE or CHOOSE_OUTPUT_FILE") {
			t.Errorf("EnforceLimits=true should still report the control-pairing structural error; got %v", withTrue)
		}
	})
}
