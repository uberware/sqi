// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// Template Schemas §7.5: an <intstring> or <floatstring> may have white space
// before and after the number, and that white space is ignored — in a value
// written in a template (a default, a constraint, a range element, a list
// parameter's items and item constraints) and in a value given when a Job is
// created. White space inside the number is not allowed, and white space is the
// Unicode White_Space property, so U+200B and U+001C are not.

func numericTemplate(paramDefs, rangeDef string) string {
	return fmt.Sprintf(`
specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: NumericStrings
parameterDefinitions:
%s
steps:
  - name: Step1
    parameterSpace:
      taskParameterDefinitions:
        - name: X
          type: INT
          range: %s
    script:
      actions:
        onRun:
          command: echo
`, paramDefs, rangeDef)
}

func TestValidate_NumericStringsAllowSurroundingWhiteSpace(t *testing.T) {
	tests := []struct {
		name      string
		paramDefs string
		rangeDef  string
	}{
		{"INT default and bounds", `  - {name: P, type: INT, default: " 7 ", minValue: " 1", maxValue: "10 ", allowedValues: [" 7 ", "8"]}`, `[1]`},
		{"FLOAT default and bounds", `  - {name: P, type: FLOAT, default: "2.5\n", minValue: " 1.0", maxValue: "3 "}`, `[1]`},
		{"INT range list element", `  - {name: P, type: INT, default: 1}`, `[" 3 ", 4]`},
		{"LIST[INT] default", `  - {name: P, type: "LIST[INT]", default: [" 1", "3 "]}`, `[1]`},
		{"LIST[FLOAT] default", `  - {name: P, type: "LIST[FLOAT]", default: [" 1.50 ", 2]}`, `[1]`},
		{"LIST[LIST[INT]] default", `  - {name: P, type: "LIST[LIST[INT]]", default: [[" 1"], ["3 ", 4]]}`, `[1]`},
		{"LIST[INT] item constraints", `  - {name: P, type: "LIST[INT]", default: [1, " 2 "], item: {minValue: " 1", maxValue: "5 ", allowedValues: [" 1 ", "2"]}}`, `[1]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, numericTemplate(tc.paramDefs, tc.rangeDef))
			if errs := openjd.Validate(tmpl); len(errs) != 0 {
				t.Fatalf("Validate() = %v, want no errors", errs)
			}
		})
	}
}

func TestValidate_NumericStringsRejectInnerOrNonWhiteSpace(t *testing.T) {
	tests := []struct {
		name      string
		paramDefs string
		rangeDef  string
		want      string
	}{
		{"INT default with an inner space", `  - {name: P, type: INT, default: "1 000"}`, `[1]`, "/parameterDefinitions/0"},
		{"INT default with a zero width space", "  - {name: P, type: INT, default: \"\u200b7\"}", `[1]`, "/parameterDefinitions/0"},
		{"INT default of only white space", `  - {name: P, type: INT, default: "   "}`, `[1]`, "/parameterDefinitions/0"},
		{"INT range element with an inner space", `  - {name: P, type: INT, default: 1}`, `["1 2"]`, "/steps/0/parameterSpace/taskParameterDefinitions/0/range/0"},
		{"LIST[INT] item with an inner space", `  - {name: P, type: "LIST[INT]", default: ["1 2"]}`, `[1]`, "/parameterDefinitions/0/default/0"},
		{"LIST[INT] item above a padded item.maxValue", `  - {name: P, type: "LIST[INT]", default: [7], item: {maxValue: " 5"}}`, `[1]`, "/parameterDefinitions/0/default/0"},
		{"LIST[INT] item not in padded item.allowedValues", `  - {name: P, type: "LIST[INT]", default: [3], item: {allowedValues: [" 1 ", "2"]}}`, `[1]`, "/parameterDefinitions/0/default/0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, numericTemplate(tc.paramDefs, tc.rangeDef))
			assertValidationContains(t, tmpl, tc.want)
		})
	}
}

func TestBindJobParameters_NumericStringsAllowSurroundingWhiteSpace(t *testing.T) {
	str := func(s string) *string { return &s }
	params := []openjd.JobParameter{
		{Name: "I", Type: openjd.JobParamTypeInt, MinValue: str(" 1"), MaxValue: str("10 ")},
		{Name: "F", Type: openjd.JobParamTypeFloat, AllowedValues: []string{" 3.5 "}},
		{Name: "L", Type: openjd.JobParamTypeListInt},
	}
	got, errs := openjd.BindJobParameters(params, map[string]string{"I": " 7 ", "F": "\t3.500\n", "L": `[1, " 2 "]`})
	if len(errs) != 0 {
		t.Fatalf("BindJobParameters() errors = %v", errs)
	}
	// The white space is not part of the value, so it is not what a format
	// string later substitutes: a FLOAT keeps its written text without it.
	if got["I"] != "7" || got["F"] != "3.500" {
		t.Errorf("BindJobParameters() = I %q, F %q; want I \"7\", F \"3.500\"", got["I"], got["F"])
	}

	for value, want := range map[string]string{"1 000": "not a valid INT", "\u200b7": "not a valid INT", "\u001c7": "not a valid INT", " 11 ": "above maximum"} {
		_, errs := openjd.BindJobParameters(params[:1], map[string]string{"I": value})
		if !strings.Contains(errs.Error(), want) {
			t.Errorf("BindJobParameters(I=%q) errors = %v, want %q", value, errs, want)
		}
	}
	if _, errs := openjd.BindJobParameters(params[2:], map[string]string{"L": `["1 2"]`}); len(errs) == 0 {
		t.Error(`BindJobParameters(L=["1 2"]) = no errors, want an error`)
	}
}
