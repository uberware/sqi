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
			name: "decimals without spinbox",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeFloat,
				UserInterface: &ParameterUserInterface{Control: ControlLineEdit, Decimals: intPtr(2)},
			},
			wantPointer: "/parameterDefinitions/0/userInterface/decimals",
		},
		{
			name: "singleStepRemoval without chip input",
			param: JobParameter{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ControlLineEdit, SingleStepRemoval: boolPtr(true)},
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

func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

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
