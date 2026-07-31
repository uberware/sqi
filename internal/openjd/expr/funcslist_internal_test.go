// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestRangeAndFlatten(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"range with a stop", "range(5)", "[0, 1, 2, 3, 4]", "list[int]"},
		{"range with a zero stop is empty", "range(0)", "[]", "list[nulltype]"},
		{"range with a negative stop is empty", "range(-1)", "[]", "list[nulltype]"},
		{"range with start and stop", "range(1, 5)", "[1, 2, 3, 4]", "list[int]"},
		{"range with an empty span", "range(5, 5)", "[]", "list[nulltype]"},
		{"range with a step", "range(0, 10, 2)", "[0, 2, 4, 6, 8]", "list[int]"},
		{"range counting down", "range(5, 0, -1)", "[5, 4, 3, 2, 1]", "list[int]"},
		{"range down with an empty span", "range(0, 5, -1)", "[]", "list[nulltype]"},
		{"flatten one level", "flatten([[1, 2], [3]])", "[1, 2, 3]", "list[int]"},
		{"flatten with an empty inner list", "flatten([[1], [], [2]])", "[1, 2]", "list[int]"},
		{"flatten of a flat list is the identity", "flatten([1, 2])", "[1, 2]", "list[int]"},
		{"flatten of an empty list", "flatten([])", "[]", "list[nulltype]"},
		{"flatten of strings", `flatten([["a"], ["b"]])`, "[a, b]", "list[string]"},
		{"flatten only removes one level", "flatten([[[1]], [[2]]])", "[[1], [2]]", "list[list[int]]"},
		{"the comprehension idiom from the spec", `flatten([["-e", e] for e in ["A=1", "B=2"]])`, "[-e, A=1, -e, B=2]", "list[string]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestRangeAndFlatten_Bounds pins that both new allocation paths report the
// element bound rather than allocating. maxElements is a per-operation floor
// that always applies; the specification's own configurable limits are
// sub-project E's.
func TestRangeAndFlatten_Bounds(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"range with an enormous stop", "range(100000000)"},
		{"range with an enormous span", "range(-100000000, 100000000)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want a size error", tc.src)
			}
			if !strings.Contains(err.Error(), "too large") {
				t.Errorf("Eval(%q) error = %v, want it to report the size bound", tc.src, err)
			}
		})
	}
}

func TestRange_RejectsAZeroStep(t *testing.T) {
	_, err := Eval("range(0, 10, 0)", MapSymbols{}, TAny)
	if err == nil {
		t.Fatal("range with a zero step succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "step") {
		t.Errorf("error = %v, want it to name the step", err)
	}
}
