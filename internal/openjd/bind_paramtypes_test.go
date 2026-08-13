// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

// TestBindJobParameters_BoolAndRangeExpr pins that a SUBMITTED value is checked
// against its declared type. Before sub-project F2, validateParamValue had arms
// for INT/FLOAT/STRING/PATH only and every other type fell through accepted, so
// a BOOL parameter would have taken "maybe" at submission.
func TestBindJobParameters_BoolAndRangeExpr(t *testing.T) {
	params := []JobParameter{
		{Name: "Flag", Type: JobParamTypeBool},
		{Name: "Frames", Type: JobParamTypeRangeExpr},
	}

	tests := []struct {
		name     string
		provided map[string]string
		wantErr  bool
	}{
		{"both valid", map[string]string{"Flag": "yes", "Frames": "1-10"}, false},
		{"bool literal", map[string]string{"Flag": "true", "Frames": "1-10"}, false},
		{"bool numeric", map[string]string{"Flag": "1", "Frames": "1-10"}, false},
		{"bool nonsense", map[string]string{"Flag": "maybe", "Frames": "1-10"}, true},
		{"range nonsense", map[string]string{"Flag": "true", "Frames": "abc"}, true},
		{"range descending is legal", map[string]string{"Flag": "true", "Frames": "10-1:-1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := BindJobParameters(params, tt.provided)
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("BindJobParameters accepted %v", tt.provided)
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("BindJobParameters rejected %v: %v", tt.provided, errs)
			}
		})
	}
}

// TestBindJobParameters_BoolDefaultApplies checks the other source of a value:
// a declared default flows through the same validation as a submitted one.
func TestBindJobParameters_BoolDefaultApplies(t *testing.T) {
	params := []JobParameter{{Name: "Flag", Type: JobParamTypeBool, Default: new("on")}}
	bound, errs := BindJobParameters(params, nil)
	if len(errs) != 0 {
		t.Fatalf("BindJobParameters: %v", errs)
	}
	if bound["Flag"] != "on" {
		t.Errorf("bound Flag = %q, want the default %q carried verbatim", bound["Flag"], "on")
	}
}

// TestBindJobParameters_ListValues pins the submission encoding: a list value
// arrives as the canonical JSON internal/openjd/paramjson.go produces, because
// neither submission surface can carry a native array — job parameters come in
// as ?param.<Name>=<value> query strings and the product body declares
// map[string]string.
func TestBindJobParameters_ListValues(t *testing.T) {
	params := []JobParameter{
		{Name: "Cameras", Type: JobParamTypeListString},
		{Name: "Frames", Type: JobParamTypeListInt},
	}

	tests := []struct {
		name     string
		provided map[string]string
		wantErr  bool
	}{
		{
			"both valid",
			map[string]string{"Cameras": `["main","closeup"]`, "Frames": "[1,2,3]"},
			false,
		},
		{
			"empty lists are valid",
			map[string]string{"Cameras": "[]", "Frames": "[]"},
			false,
		},
		{
			"a bare scalar is not a list",
			map[string]string{"Cameras": "main", "Frames": "[1]"},
			true,
		},
		{
			"wrong element type",
			map[string]string{"Cameras": `["a"]`, "Frames": `["notanint"]`},
			true,
		},
		{
			"malformed JSON",
			map[string]string{"Cameras": `["a"`, "Frames": "[1]"},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := BindJobParameters(params, tt.provided)
			if tt.wantErr && len(errs) == 0 {
				t.Fatalf("BindJobParameters accepted %v", tt.provided)
			}
			if !tt.wantErr && len(errs) != 0 {
				t.Fatalf("BindJobParameters rejected %v: %v", tt.provided, errs)
			}
		})
	}
}

// TestBindJobParameters_ListConstraints pins that the list's own bounds and its
// item: constraints apply to a submitted value, not only to a declared default.
func TestBindJobParameters_ListConstraints(t *testing.T) {
	params := []JobParameter{{
		Name: "Cameras", Type: JobParamTypeListString,
		MinLength: new(2),
		Item:      &ItemConstraint{AllowedValues: []string{"main", "closeup"}, AllowedValuesSet: true},
	}}

	if _, errs := BindJobParameters(params, map[string]string{
		"Cameras": `["main","closeup"]`,
	}); len(errs) != 0 {
		t.Fatalf("a valid list was rejected: %v", errs)
	}
	if _, errs := BindJobParameters(params, map[string]string{
		"Cameras": `["main"]`,
	}); len(errs) == 0 {
		t.Error("a list below minLength was accepted")
	}
	if _, errs := BindJobParameters(params, map[string]string{
		"Cameras": `["main","wide"]`,
	}); len(errs) == 0 {
		t.Error("an element outside item.allowedValues was accepted")
	}
}

// TestBindJobParameters_ListDefaultRoundTrips pins that a declared list default
// survives binding as the same canonical JSON text, since that text is exactly
// what the store, the wire and expr.ValueFromText all carry downstream.
func TestBindJobParameters_ListDefaultRoundTrips(t *testing.T) {
	params := []JobParameter{{
		Name: "Cameras", Type: JobParamTypeListString,
		Default: new(`["main","closeup"]`),
	}}
	bound, errs := BindJobParameters(params, nil)
	if len(errs) != 0 {
		t.Fatalf("BindJobParameters: %v", errs)
	}
	if bound["Cameras"] != `["main","closeup"]` {
		t.Errorf("bound Cameras = %q, want the canonical JSON carried verbatim", bound["Cameras"])
	}
}
