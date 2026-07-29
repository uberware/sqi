// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestEvalListLit_InferenceWithoutTarget(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantType string
		wantStr  string
	}{
		{"all int", "[1, 2, 3]", "list[int]", "[1, 2, 3]"},
		{"all string", "['a', 'b']", "list[string]", "[a, b]"},
		{"all bool", "[true, false]", "list[bool]", "[true, false]"},
		{"mixed int and float", "[1, 2.0, 3]", "list[float]", "[1.0, 2.0, 3.0]"},
		{"all float", "[1.0, 2.5]", "list[float]", "[1.0, 2.5]"},
		{"empty", "[]", "list[nulltype]", "[]"},
		{"single", "[42]", "list[int]", "[42]"},
		{"trailing comma", "[1, 2, 3,]", "list[int]", "[1, 2, 3]"},
		{"nested", "[[1, 2], [3, 4]]", "list[list[int]]", "[[1, 2], [3, 4]]"},
		{"nested int and float lists", "[[1], [2.0]]", "list[list[float]]", "[[1.0], [2.0]]"},
		{"nested with an empty list", "[[], [1]]", "list[list[int]]", "[[], [1]]"},
		{"computed elements", "[1 + 1, 2 * 2]", "list[int]", "[2, 4]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, nil, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
			if got := v.String(); got != tc.wantStr {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.wantStr)
			}
		})
	}
}

func TestEvalListLit_InferenceWithTarget(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		target   string
		wantType string
		wantStr  string
	}{
		{"ints to floats", "[1, 2]", "list[float]", "list[float]", "[1.0, 2.0]"},
		{"ints to strings", "[1, 2]", "list[string]", "list[string]", "[1, 2]"},
		{"empty adopts the target", "[]", "list[int]", "list[int]", "[]"},
		{"nested applies recursively", "[[1], [2]]", "list[list[float]]", "list[list[float]]", "[[1.0], [2.0]]"},
		{"inside a conditional", "[1, 2] if true else []", "list[float]", "list[float]", "[1.0, 2.0]"},
		{"inside an or", "null or [1, 2]", "list[float]", "list[float]", "[1.0, 2.0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseType(tc.target)
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tc.target, err)
			}
			v, err := Eval(tc.src, nil, target)
			if err != nil {
				t.Fatalf("Eval(%q, %s): %v", tc.src, tc.target, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("type = %s, want %s", got, tc.wantType)
			}
			if got := v.String(); got != tc.wantStr {
				t.Errorf("value = %s, want %s", got, tc.wantStr)
			}
		})
	}
}

func TestEvalListLit_Errors(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"null element", "[1, null]", "null cannot be an element of a list"},
		{"null alone", "[null]", "null cannot be an element of a list"},
		{"incompatible elements", "[1, 'a']", "incompatible"},
		{"incompatible nested", "[[1], ['a']]", "incompatible"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, nil, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

func TestEvalListLit_UnresolvedElementsHoist(t *testing.T) {
	syms := MapSymbols{
		"Param.X": Unresolved(TInt),
		"Param.S": Unresolved(TString),
	}
	tests := []struct {
		src      string
		wantType string
	}{
		{"[Param.X, 1]", "unresolved[list[int]]"},
		{"[Param.X, 1.5]", "unresolved[list[float]]"},
		{"[Param.X]", "unresolved[list[int]]"},
		{"[Param.S, 'a']", "unresolved[list[string]]"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Fatalf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
			if !v.IsUnresolved() {
				t.Fatalf("Eval(%q) is not unresolved, but an element had no value", tc.src)
			}
		})
	}
}
