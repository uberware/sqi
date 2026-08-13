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
