// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

// TestCheckParamValueAgainstType covers the shared core directly. Each case is
// a declared type plus a candidate value; the same function serves the
// template's default (sub-project F1) and a submitted value (F2), which is the
// whole reason it exists.
func TestCheckParamValueAgainstType(t *testing.T) {
	tests := []struct {
		name    string
		p       JobParameter
		value   string
		wantErr bool
	}{
		{"bool true", JobParameter{Type: JobParamTypeBool}, "true", false},
		{"bool yes", JobParameter{Type: JobParamTypeBool}, "yes", false},
		{"bool maybe", JobParameter{Type: JobParamTypeBool}, "maybe", true},

		{"range expr", JobParameter{Type: JobParamTypeRangeExpr}, "1-10:2", false},
		{"range expr descending", JobParameter{Type: JobParamTypeRangeExpr}, "10-1:-1", false},
		{"range expr invalid", JobParameter{Type: JobParamTypeRangeExpr}, "abc", true},

		{"list of strings", JobParameter{Type: JobParamTypeListString}, `["a","b"]`, false},
		{"list of strings, number element", JobParameter{Type: JobParamTypeListString}, `[1]`, true},
		{"list of ints", JobParameter{Type: JobParamTypeListInt}, `[1,2]`, false},
		{"list of ints, string element", JobParameter{Type: JobParamTypeListInt}, `["a"]`, true},
		{"nested list", JobParameter{Type: JobParamTypeListListInt}, `[[1],[2]]`, false},
		{"nested list, flat", JobParameter{Type: JobParamTypeListListInt}, `[1]`, true},
		{"list, not JSON at all", JobParameter{Type: JobParamTypeListString}, "notalist", true},

		{
			"list below minLength",
			JobParameter{Type: JobParamTypeListString, MinLength: new(2)},
			`["a"]`, true,
		},
		{
			"list item below item.minValue",
			JobParameter{
				Type: JobParamTypeListInt,
				Item: &ItemConstraint{MinValue: new("5")},
			},
			`[1]`, true,
		},

		// The four base-spec types keep working through the same entry point.
		{"int", JobParameter{Type: JobParamTypeInt}, "42", false},
		{"int invalid", JobParameter{Type: JobParamTypeInt}, "x", true},
		{"string", JobParameter{Type: JobParamTypeString}, "anything", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := checkParamValueAgainstType(tt.p, tt.value, "/p")
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("value %q accepted for %s", tt.value, tt.p.Type)
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("value %q rejected for %s: %v", tt.value, tt.p.Type, errs)
			}
			for _, e := range errs {
				if len(e.Pointer) < 2 || e.Pointer[:2] != "/p" {
					t.Errorf("error pointer %q does not start with the given prefix", e.Pointer)
				}
			}
		})
	}
}

// TestCheckParamValueAgainstType_SkipsFormatStrings mirrors the scalar rule: a
// value containing "{{" is not known until it is resolved, so checking it
// against its declared type here would reject a valid template.
func TestCheckParamValueAgainstType_SkipsFormatStrings(t *testing.T) {
	p := JobParameter{Type: JobParamTypeListInt, MinLength: new(9)}
	if errs := checkParamValueAgainstType(p, "{{Param.Other}}", "/p"); len(errs) != 0 {
		t.Errorf("a format-string value was validated as a literal: %v", errs)
	}
}
