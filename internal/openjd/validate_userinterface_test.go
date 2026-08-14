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
		// NOTE: there is deliberately no "empty control" case here. This table
		// once asserted that an absent control was an error, which codified a
		// bug: the schema marks userInterface.control "@optional" on every
		// parameter type. The spec-correct behavior is covered by
		// TestValidateUserInterface_ControlIsOptional below.
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

// TestValidateUIGroupLabelExactBoundary proves the groupLabel cap is enforced
// at exactly maxUIGroupLabelLen characters, not merely somewhere below a
// clearly-over-limit value. TestValidateUIGroupLabelLengthLimit above only
// exercises a 257-character groupLabel, which would still fail an off-by-one
// in the comparison (e.g. >= instead of >); this test pins the boundary
// itself: maxUIGroupLabelLen characters must be accepted, maxUIGroupLabelLen+1
// must be rejected.
func TestValidateUIGroupLabelExactBoundary(t *testing.T) {
	base := func(groupLabel string) *JobTemplate {
		return &JobTemplate{
			SpecificationVersion: SpecVersion,
			Name:                 "x",
			ParameterDefinitions: []JobParameter{{
				Name: "Q", Type: JobParamTypeString,
				UserInterface: &ParameterUserInterface{Control: ControlLineEdit, GroupLabel: groupLabel},
			}},
			Steps: []StepTemplate{{Name: "A"}},
		}
	}

	// Exactly maxUIGroupLabelLen characters — must be accepted.
	groupLabelAtLimit := strings.Repeat("x", maxUIGroupLabelLen)
	if errs := ValidateWithOptions(base(groupLabelAtLimit), ValidateOptions{EnforceLimits: true}); strings.Contains(errs.Error(), "groupLabel") {
		t.Errorf("%d-character groupLabel was incorrectly rejected; got %v", maxUIGroupLabelLen, errs)
	}

	// maxUIGroupLabelLen+1 characters — must be rejected with a pointer on /groupLabel.
	groupLabelOverLimit := strings.Repeat("x", maxUIGroupLabelLen+1)
	errs := ValidateWithOptions(base(groupLabelOverLimit), ValidateOptions{EnforceLimits: true})
	found := false
	for _, e := range errs {
		if e.Pointer == "/parameterDefinitions/0/userInterface/groupLabel" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("%d-character groupLabel was not flagged at expected pointer; got %v", maxUIGroupLabelLen+1, errs)
	}
}

// TestValidateUILabelRuneCounting proves the label limit is counted in Unicode
// runes (characters), not bytes.  "é" (U+00E9) is 2 bytes but 1 rune.
//
// With byte counting (the old len() approach) a maxUILabelLen-rune label of
// "é" would be 2*maxUILabelLen bytes and would be wrongly rejected.  With
// correct rune counting it must be accepted; maxUILabelLen+1 runes must be
// rejected.
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

	// Exactly maxUILabelLen multibyte runes — must be accepted.
	labelAtLimit := strings.Repeat("é", maxUILabelLen) // 2*maxUILabelLen bytes, maxUILabelLen runes
	if errs := ValidateWithOptions(base(labelAtLimit), ValidateOptions{EnforceLimits: true}); strings.Contains(errs.Error(), "label") {
		t.Errorf("%d-rune multibyte label was incorrectly rejected; got %v", maxUILabelLen, errs)
	}

	// maxUILabelLen+1 multibyte runes — must be rejected with a pointer on /label.
	labelOverLimit := strings.Repeat("é", maxUILabelLen+1)
	errs := ValidateWithOptions(base(labelOverLimit), ValidateOptions{EnforceLimits: true})
	found := false
	for _, e := range errs {
		if e.Pointer == "/parameterDefinitions/0/userInterface/label" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("%d-rune multibyte label was not flagged at expected pointer; got %v", maxUILabelLen+1, errs)
	}
}

// userInterface.control is OPTIONAL. The 2023-09 Template Schemas mark it
// "# @optional" on every parameter type's userInterface block, so a template
// may supply only a label or groupLabel and leave the control to the
// submitting application's judgement.
//
// sqi required it, rejecting conforming templates outright. Surfaced by the
// official conformance suite fixtures 2.1--string-ui-label-without-control,
// 2.1--string-ui-grouplabel-without-control, 2.2--path-ui-label-without-control,
// 2.3--int-ui-label-without-control, and 2.4--float-ui-label-without-control.
func TestValidateUserInterface_ControlIsOptional(t *testing.T) {
	uiTemplate := func(typ JobParamType, ui *ParameterUserInterface) *JobTemplate {
		return &JobTemplate{
			SpecificationVersion: SpecVersion,
			Name:                 "x",
			ParameterDefinitions: []JobParameter{{Name: "Q", Type: typ, UserInterface: ui}},
			Steps: []StepTemplate{{
				Name: "A",
				Script: &StepScript{
					Actions: StepActions{OnRun: Action{Command: "echo", Args: []string{"hi"}}},
				},
			}},
		}
	}
	decimals := 2

	t.Run("label without control is accepted on every parameter type", func(t *testing.T) {
		for _, typ := range []JobParamType{
			JobParamTypeString, JobParamTypePath, JobParamTypeInt, JobParamTypeFloat,
		} {
			errs := Validate(uiTemplate(typ, &ParameterUserInterface{Label: "My Label"}))
			if len(errs) != 0 {
				t.Errorf("%s: expected no errors, got %v", typ, errs)
			}
		}
	})

	t.Run("groupLabel without control is accepted", func(t *testing.T) {
		errs := Validate(uiTemplate(JobParamTypeString, &ParameterUserInterface{GroupLabel: "My Group"}))
		if len(errs) != 0 {
			t.Errorf("expected no errors, got %v", errs)
		}
	})

	// Making control optional must not disable the checks that depend on it.
	t.Run("decimals without a control is still rejected", func(t *testing.T) {
		errs := Validate(uiTemplate(JobParamTypeFloat, &ParameterUserInterface{Decimals: &decimals}))
		if !strings.Contains(errs.Error(), "decimals") {
			t.Errorf("expected a decimals error, got %v", errs)
		}
	})

	t.Run("an unknown control is still rejected", func(t *testing.T) {
		errs := Validate(uiTemplate(JobParamTypeString, &ParameterUserInterface{Control: "WAT"}))
		if len(errs) == 0 {
			t.Error("expected an error for an unknown control, got none")
		}
	})
}
