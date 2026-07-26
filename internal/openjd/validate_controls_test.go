// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// pathParamTemplate builds a template with one PATH job parameter carrying the
// given userInterface control.
//
// objectType tracks the control: a CHOOSE_*_FILE control requires FILE and
// CHOOSE_DIRECTORY requires DIRECTORY (see validateChooseControl). This test is
// about which controls are legal on a PATH parameter, so the objectType must
// agree rather than accidentally testing the coherence rule.
func pathParamTemplate(control string) string {
	objectType := "DIRECTORY"
	if control == "CHOOSE_INPUT_FILE" || control == "CHOOSE_OUTPUT_FILE" {
		objectType = "FILE"
	}
	return `
specificationVersion: jobtemplate-2023-09
name: PathControlJob
parameterDefinitions:
  - name: ScenePath
    type: PATH
    objectType: ` + objectType + `
    dataFlow: IN
    userInterface: { control: ` + control + `, label: Scene }
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
}

func TestValidate_ChooserControlsAccepted(t *testing.T) {
	for _, control := range []string{"CHOOSE_INPUT_FILE", "CHOOSE_OUTPUT_FILE", "CHOOSE_DIRECTORY"} {
		t.Run(control, func(t *testing.T) {
			tmpl := mustParse(t, pathParamTemplate(control))
			if errs := openjd.Validate(tmpl); len(errs) > 0 {
				t.Fatalf("expected %s to be valid on a PATH parameter; got: %v", control, errs)
			}
		})
	}
}

// The full control vocabulary checked against every parameter type. Every pair
// marked invalid here is accepted by sqi before this task.
func TestValidate_ControlsScopedToParameterType(t *testing.T) {
	for _, tc := range []struct {
		paramType string
		control   string
		valid     bool
	}{
		{"STRING", "LINE_EDIT", true},
		{"STRING", "MULTILINE_EDIT", true},
		{"STRING", "HIDDEN", true},
		{"STRING", "SPIN_BOX", false},
		{"STRING", "CHOOSE_INPUT_FILE", false},
		{"PATH", "CHOOSE_INPUT_FILE", true},
		{"PATH", "CHOOSE_OUTPUT_FILE", true},
		{"PATH", "CHOOSE_DIRECTORY", true},
		{"PATH", "HIDDEN", true},
		{"PATH", "LINE_EDIT", false},
		{"PATH", "MULTILINE_EDIT", false},
		{"PATH", "CHECK_BOX", false},
		{"INT", "SPIN_BOX", true},
		{"INT", "HIDDEN", true},
		{"INT", "LINE_EDIT", false},
		{"INT", "CHECK_BOX", false},
		{"FLOAT", "SPIN_BOX", true},
		{"FLOAT", "MULTILINE_EDIT", false},
		// CHIP_INPUT is an sqi invention appearing nowhere in the spec, at any
		// type. These rows go red here, where the vocabulary actually changes,
		// and stay as the permanent guard against anyone re-adding it. Task 9
		// then deletes the dead constant with no new test of its own.
		{"STRING", "CHIP_INPUT", false},
		{"PATH", "CHIP_INPUT", false},
		{"INT", "CHIP_INPUT", false},
		{"FLOAT", "CHIP_INPUT", false},
	} {
		t.Run(tc.paramType+"/"+tc.control, func(t *testing.T) {
			// A PATH parameter's objectType must agree with a CHOOSE_* control
			// (validateChooseControl); it defaults to DIRECTORY. Declare FILE
			// for the file choosers so this test measures control-to-parameter
			// -type scoping rather than the coherence rule.
			objectType := ""
			if tc.paramType == "PATH" &&
				(tc.control == "CHOOSE_INPUT_FILE" || tc.control == "CHOOSE_OUTPUT_FILE") {
				objectType = "\n    objectType: FILE"
			}
			yaml := `
specificationVersion: jobtemplate-2023-09
name: ScopedControlJob
parameterDefinitions:
  - name: P
    type: ` + tc.paramType + objectType + `
    userInterface: { control: ` + tc.control + `, label: P }
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
			tmpl := mustParse(t, yaml)
			errs := openjd.Validate(tmpl)
			got := len(errs) == 0
			if got != tc.valid {
				t.Fatalf("%s on %s: valid = %v, want %v (errs: %v)",
					tc.control, tc.paramType, got, tc.valid, errs)
			}
		})
	}
}

func TestValidate_LabelLengthBounds(t *testing.T) {
	// No minimum-length case: getString (parse.go:693-697) returns "" for both
	// an absent key and an explicitly empty one, and Label is a plain string, so
	// `label: ""` and no label at all are the same state after parsing -- and the
	// absent case is legal (label is @optional; the spec prescribes falling back
	// to the parameter name). There is no representable "present but empty" to
	// reject. Ruled 2026-07-24: enforce the maximum only.
	for _, tc := range []struct {
		name  string
		label string
		valid bool
	}{
		{"empty is indistinguishable from absent, so valid", "", true},
		{"one char", "S", true},
		{"exactly 64", strings.Repeat("a", 64), true},
		{"65 is too long", strings.Repeat("a", 65), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
specificationVersion: jobtemplate-2023-09
name: LabelJob
parameterDefinitions:
  - name: P
    type: STRING
    userInterface: { control: LINE_EDIT, label: "` + tc.label + `" }
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`
			tmpl := mustParse(t, yaml)
			errs := openjd.Validate(tmpl)
			got := len(errs) == 0
			if got != tc.valid {
				t.Fatalf("label len %d: valid = %v, want %v (errs: %v)",
					len(tc.label), got, tc.valid, errs)
			}
		})
	}
}
