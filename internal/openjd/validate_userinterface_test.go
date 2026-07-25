// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"
)

func TestValidateUserInterface(t *testing.T) {
	tests := []struct {
		name        string
		param       JobParameter
		wantPointer string // "" means expect no error
	}{
		{
			name: "valid dropdown",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString, AllowedValues: []string{"a", "b"},
				UserInterface: &ParameterUserInterface{Control: ControlDropdownList},
			},
		},
		{
			name: "valid hidden no constraints",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ControlHidden},
			},
		},
		{
			name: "unknown control",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: "WAT"},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/control",
		},
		{
			name: "empty control",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ""},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/control",
		},
		{
			name: "dropdown without allowedValues",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ControlDropdownList},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/control",
		},
		{
			name: "checkbox not exactly two values",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString, AllowedValues: []string{"a"},
				UserInterface: &ParameterUserInterface{Control: ControlCheckBox},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/control",
		},
		{
			name: "spinbox on string",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ControlSpinBox},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/control",
		},
		{
			name: "valid spinbox with decimals",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeFloat,
				UserInterface: &ParameterUserInterface{Control: ControlSpinBox, Decimals: new(2)},
			},
		},
		{
			name: "decimals without spinbox",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeFloat,
				UserInterface: &ParameterUserInterface{Control: ControlHidden, Decimals: new(2)},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/decimals",
		},
		{
			name: "singleStepRemoval without chip input",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ControlLineEdit, SingleStepRemoval: new(true)},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/singleStepRemoval",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateUserInterface(tc.param, "/parameterDefinitions/0")
			if tc.wantPointer == "" {
				if len(errs) != 0 {
					t.Fatalf("got errors %v, want none", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Pointer == tc.wantPointer {
					return
				}
			}
			t.Fatalf("no error at %q; got %v", tc.wantPointer, errs)
		})
	}
}

// Confirms the check is wired into the full validate path.
func TestValidateRejectsBadUserInterface(t *testing.T) {
	tmpl := &JobTemplate{
		SpecificationVersion: SpecVersion,
		Name:                 "x",
		ParameterDefinitions: []JobParameter{{
			Name: "Q", Type: JobParamTypeString,
			UserInterface: &ParameterUserInterface{Control: "WAT"},
		}},
		Steps: []StepTemplate{{Name: "A"}},
	}
	errs := Validate(tmpl)
	if !strings.Contains(errs.Error(), "userInterface") {
		t.Fatalf("Validate did not flag userInterface; got %v", errs)
	}
}

func TestValidateUILabelLengthLimit(t *testing.T) {
	long := strings.Repeat("x", 257)
	tmpl := &JobTemplate{
		SpecificationVersion: SpecVersion,
		Name:                 "x",
		ParameterDefinitions: []JobParameter{{
			Name: "Q", Type: JobParamTypeString,
			UserInterface: &ParameterUserInterface{Control: ControlLineEdit, Label: long},
		}},
		Steps: []StepTemplate{{Name: "A"}},
	}

	// Enforced: too-long label is rejected.
	if errs := ValidateWithOptions(tmpl, ValidateOptions{EnforceLimits: true}); !strings.Contains(errs.Error(), "label") {
		t.Errorf("EnforceLimits=true did not flag long label; got %v", errs)
	}
	// Not enforced: limit check skipped.
	if errs := ValidateWithOptions(tmpl, ValidateOptions{EnforceLimits: false}); strings.Contains(errs.Error(), "label") {
		t.Errorf("EnforceLimits=false flagged long label; got %v", errs)
	}
}

// TestValidateUIGroupLabelLengthLimit checks that a GroupLabel exceeding the
// cap is rejected when EnforceLimits is true.
func TestValidateUIGroupLabelLengthLimit(t *testing.T) {
	longGroup := strings.Repeat("x", 257)
	tmpl := &JobTemplate{
		SpecificationVersion: SpecVersion,
		Name:                 "x",
		ParameterDefinitions: []JobParameter{{
			Name: "Q", Type: JobParamTypeString,
			UserInterface: &ParameterUserInterface{Control: ControlLineEdit, GroupLabel: longGroup},
		}},
		Steps: []StepTemplate{{Name: "A"}},
	}

	// Enforced: too-long groupLabel is rejected.
	errs := ValidateWithOptions(tmpl, ValidateOptions{EnforceLimits: true})
	found := false
	for _, e := range errs {
		if e.Pointer == "/parameterDefinitions/0/userInterface/groupLabel" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("EnforceLimits=true did not flag long groupLabel at expected pointer; got %v", errs)
	}

	// Not enforced: limit check skipped.
	if errs := ValidateWithOptions(tmpl, ValidateOptions{EnforceLimits: false}); strings.Contains(errs.Error(), "groupLabel") {
		t.Errorf("EnforceLimits=false flagged long groupLabel; got %v", errs)
	}
}

// TestValidateUILabelRuneCounting proves the label limit is counted in Unicode
// runes (characters), not bytes.  "é" (U+00E9) is 2 bytes but 1 rune.
//
// With byte counting (the old len() approach) a 256-rune label of "é" would be
// 512 bytes and would be wrongly rejected.  With correct rune counting it must
// be accepted; 257 runes must be rejected.
func TestValidateUILabelRuneCounting(t *testing.T) {
	base := func(label string) *JobTemplate {
		return &JobTemplate{
			SpecificationVersion: SpecVersion,
			Name:                 "x",
			ParameterDefinitions: []JobParameter{{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ControlLineEdit, Label: label},
			}},
			Steps: []StepTemplate{{Name: "A"}},
		}
	}

	// Exactly 256 multibyte runes — must be accepted.
	label256 := strings.Repeat("é", 256) // 512 bytes, 256 runes
	if errs := ValidateWithOptions(base(label256), ValidateOptions{EnforceLimits: true}); strings.Contains(errs.Error(), "label") {
		t.Errorf("256-rune multibyte label was incorrectly rejected; got %v", errs)
	}

	// 257 multibyte runes — must be rejected with a pointer on /label.
	label257 := strings.Repeat("é", 257)
	errs := ValidateWithOptions(base(label257), ValidateOptions{EnforceLimits: true})
	found := false
	for _, e := range errs {
		if e.Pointer == "/parameterDefinitions/0/userInterface/label" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("257-rune multibyte label was not flagged at expected pointer; got %v", errs)
	}
}
