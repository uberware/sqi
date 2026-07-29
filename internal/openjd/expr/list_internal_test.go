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
		// Section 1.2.6 rule 4 (a mix of path and string is list[string]) and
		// rule 5 (rules 3 and 4 one level down), which no row above reached:
		// every other case here is int/float/bool/string, so unifyElemPair's
		// isPathStringPair branch had no direct coverage at all. The coercion
		// tests exercise path -> string through a different code path
		// (coercible), not through unification.
		{"mixed string and path", "['a', Param.Dir]", "list[string]", "[a, /tmp]"},
		{"all path", "[Param.Dir, Param.Dir]", "list[path]", "[/tmp, /tmp]"},
		{"nested string and path lists", "[['a'], [Param.Dir]]", "list[list[string]]", "[[a], [/tmp]]"},
	}
	syms := MapSymbols{"Param.Dir": Value{Type: TPath, s: "/tmp"}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
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

func TestEvalIndex(t *testing.T) {
	syms := MapSymbols{
		"Param.Items":   List(TInt, []Value{Int(10), Int(20), Int(30)}),
		"Param.Name":    String("hello"),
		"Param.Unicode": String("héllo"),
	}
	rng, err := RangeExpr("10-30:10")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	syms["Param.Range"] = rng

	tests := []struct {
		src  string
		want string
	}{
		{"Param.Items[0]", "10"},
		{"Param.Items[2]", "30"},
		{"Param.Items[-1]", "30"},
		{"Param.Items[-3]", "10"},
		{"[1, 2, 3][1]", "2"},
		{"Param.Name[0]", "h"},
		{"Param.Name[-1]", "o"},
		{"Param.Unicode[1]", "é"},
		{"Param.Range[0]", "10"},
		{"Param.Range[-1]", "30"},
		{"[[1, 2], [3, 4]][1][0]", "3"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Fatalf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
		})
	}
}

func TestEvalIndex_Errors(t *testing.T) {
	syms := MapSymbols{
		"Param.Items": List(TInt, []Value{Int(10)}),
		"Param.Name":  String("hi"),
		"Param.Dir":   Value{Type: TPath, s: "/tmp"},
	}
	tests := []struct {
		src      string
		wantSubs string
	}{
		{"Param.Items[5]", "out of bounds"},
		{"Param.Items[-2]", "out of bounds"},
		{"Param.Name[9]", "out of bounds"},
		{"Param.Dir[0]", "parts"},
		{"Param.Items['a']", "index"},
		{"5[0]", "cannot be subscripted"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := Eval(tc.src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

func TestEvalIndex_Unresolved(t *testing.T) {
	syms := MapSymbols{
		"Param.Items": Unresolved(ListOf(TInt)),
		"Param.Name":  Unresolved(TString),
		"Param.Range": Unresolved(TRangeExpr),
		"Param.I":     Unresolved(TInt),
		"Param.Known": List(TInt, []Value{Int(1)}),
	}
	tests := []struct {
		src      string
		wantType string
	}{
		{"Param.Items[0]", "unresolved[int]"},
		{"Param.Items[99]", "unresolved[int]"}, // no bounds check against a placeholder
		{"Param.Name[0]", "unresolved[string]"},
		{"Param.Range[0]", "unresolved[int]"},
		{"Param.Known[Param.I]", "unresolved[int]"},
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
		})
	}
}
