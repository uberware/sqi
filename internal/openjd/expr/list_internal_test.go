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

// TestEvalIndexAndSlice_UnionReceiver pins the union arm of indexResultType and
// sliceResultType.
//
// Every case here was a FALSE REJECTION before that arm existed — a type error
// reported for an expression that cannot fail at runtime, which for a template
// author means a rejected job. The first is self-inflicted: sliceResultType
// deliberately types a range_expr slice as "range_expr | list[int]", and the
// subscript function could not then consume the union its own package
// manufactures. The rest come from condResult (eval.go) typing a conditional
// with an unknown condition as the union of both branches, per section 1.3.1.
//
// The expected types are asserted, not merely the absence of an error: the
// point of the arm is that the result is USABLE downstream, which it is only if
// it carries the type the runtime value will really have.
func TestEvalIndexAndSlice_UnionReceiver(t *testing.T) {
	syms := MapSymbols{
		"Param.Range": Unresolved(TRangeExpr),
		"Param.Flag":  Unresolved(TBool),
		"Param.Name":  Unresolved(TString),
	}
	tests := []struct {
		src      string
		wantType string
	}{
		// range_expr | list[int], sliced then subscripted: an int either way.
		{"Param.Range[:][0]", "unresolved[int]"},
		{"Param.Range[:][-1]", "unresolved[int]"},
		// Slicing that same union again keeps both possibilities rather than
		// collapsing to list[int] — see unifyResultPair.
		{"Param.Range[:][0:1]", "unresolved[list[int] | range_expr]"},
		// A conditional with an unknown condition, its branches two list types
		// with different elements. Section 1.2.6 rule 3 unifies int and float.
		{"([1, 2] if Param.Flag else [3.0])[0]", "unresolved[float]"},
		{"([1, 2] if Param.Flag else [3.0])[0:1]", "unresolved[list[float]]"},
		// Branches of the same type need no unification at all.
		{"([1, 2] if Param.Flag else [3])[0]", "unresolved[int]"},
		// Genuinely unrelated member results stay a union, which downstream
		// operators consume (shape.go's unionArgValueCost).
		{"('ab' if Param.Flag else [1])[0]", "unresolved[int | string]"},
		// The union arm must still reject a member that really cannot be
		// subscripted, rather than quietly dropping it.
		{"Param.Name[0]", "unresolved[string]"},
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

// TestEvalIndexAndSlice_UnionReceiverRejectsAnUnindexableMember checks the other
// half of the union rule: legal on EVERY member, not on any one of them. A
// union holding a path is not subscriptable, because the runtime value might be
// the path.
func TestEvalIndexAndSlice_UnionReceiverRejectsAnUnindexableMember(t *testing.T) {
	syms := MapSymbols{
		"Param.Flag": Unresolved(TBool),
		"Param.Dir":  Value{Type: TPath, s: "/tmp"},
	}
	tests := []struct {
		src      string
		wantSubs string
	}{
		{"([1] if Param.Flag else Param.Dir)[0]", "path cannot be subscripted"},
		{"([1] if Param.Flag else Param.Dir)[0:1]", "path cannot be sliced"},
		{"([1] if Param.Flag else 2)[0]", "cannot be subscripted"},
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
