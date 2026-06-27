// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

func TestParseUserInterface(t *testing.T) {
	src := `
specificationVersion: jobtemplate-2023-09
name: ui-test
parameterDefinitions:
  - name: Quality
    type: STRING
    allowedValues: [low, high]
    userInterface:
      control: DROPDOWN_LIST
      label: Render quality
      groupLabel: Output
  - name: Frames
    type: STRING
parameterSpace: {}
steps:
  - name: A
    script:
      actions:
        onRun:
          command: echo
`
	tmpl, err := Parse([]byte(src), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	q := tmpl.ParameterDefinitions[0]
	if q.UserInterface == nil {
		t.Fatal("Quality.UserInterface = nil, want non-nil")
	}
	if q.UserInterface.Control != ControlDropdownList {
		t.Errorf("Control = %q, want %q", q.UserInterface.Control, ControlDropdownList)
	}
	if q.UserInterface.Label != "Render quality" {
		t.Errorf("Label = %q, want %q", q.UserInterface.Label, "Render quality")
	}
	if q.UserInterface.GroupLabel != "Output" {
		t.Errorf("GroupLabel = %q, want %q", q.UserInterface.GroupLabel, "Output")
	}

	if tmpl.ParameterDefinitions[1].UserInterface != nil {
		t.Error("Frames.UserInterface = non-nil, want nil (no userInterface key)")
	}
}

func TestParseUserInterfaceControlExtras(t *testing.T) {
	src := `
specificationVersion: jobtemplate-2023-09
name: ui-extras
parameterDefinitions:
  - name: Scale
    type: FLOAT
    userInterface:
      control: SPIN_BOX
      decimals: 3
  - name: Tags
    type: STRING
    userInterface:
      control: CHIP_INPUT
      singleStepRemoval: true
steps:
  - name: A
    script:
      actions:
        onRun:
          command: echo
`
	tmpl, err := Parse([]byte(src), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	scale := tmpl.ParameterDefinitions[0].UserInterface
	if scale == nil || scale.Decimals == nil || *scale.Decimals != 3 {
		t.Fatalf("Scale.Decimals = %v, want 3", scale)
	}
	tags := tmpl.ParameterDefinitions[1].UserInterface
	if tags == nil || tags.SingleStepRemoval == nil || !*tags.SingleStepRemoval {
		t.Fatalf("Tags.SingleStepRemoval = %v, want true", tags)
	}
}
