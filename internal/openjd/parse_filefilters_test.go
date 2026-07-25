// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

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
