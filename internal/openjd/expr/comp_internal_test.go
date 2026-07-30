// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func compSyms(t *testing.T) MapSymbols {
	t.Helper()
	rng, err := RangeExpr("1-3")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	return MapSymbols{
		"Param.Items":  List(TInt, []Value{Int(1), Int(2), Int(3)}),
		"Param.Words":  List(TString, []Value{String("a"), String("b")}),
		"Param.Range":  rng,
		"Param.Empty":  List(TNull, nil),
		"Param.Unk":    Unresolved(ListOf(TInt)),
		"Param.Scalar": Int(7),
		"Param.Text":   String("abc"),
		"x":            Int(99),
	}
}

func TestEvalListComp_Values(t *testing.T) {
	syms := compSyms(t)
	tests := []struct {
		src      string
		want     string
		wantType string
	}{
		{"[i for i in Param.Items]", "[1, 2, 3]", "list[int]"},
		{"[i * 2 for i in Param.Items]", "[2, 4, 6]", "list[int]"},
		{"[i for i in Param.Items if i > 1]", "[2, 3]", "list[int]"},
		{"[i for i in Param.Items if i > 99]", "[]", "list[nulltype]"},
		{"[i for i in Param.Empty]", "[]", "list[nulltype]"},
		{"[i for i in Param.Range]", "[1, 2, 3]", "list[int]"},
		{"[i * 1.5 for i in Param.Items]", "[1.5, 3.0, 4.5]", "list[float]"},
		{"[w for w in Param.Words]", "[a, b]", "list[string]"},
		{"[[i] for i in Param.Items]", "[[1], [2], [3]]", "list[list[int]]"},
		{"[[j for j in Param.Items] for i in Param.Words]", "[[1, 2, 3], [1, 2, 3]]", "list[list[int]]"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestEvalListComp_TargetFlowsInward pins that the element expression is an
// identity position for the target type, like a list literal's elements.
func TestEvalListComp_TargetFlowsInward(t *testing.T) {
	syms := compSyms(t)
	target, err := ParseType("list[float]")
	if err != nil {
		t.Fatalf("ParseType: %v", err)
	}
	v, err := Eval("[i for i in Param.Items]", syms, target)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got, want := v.Type.String(), "list[float]"; got != want {
		t.Errorf("type = %s, want %s", got, want)
	}
	if got, want := v.String(), "[1.0, 2.0, 3.0]"; got != want {
		t.Errorf("value = %s, want %s", got, want)
	}
}

func TestEvalListComp_Errors(t *testing.T) {
	syms := compSyms(t)
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"shadows a bound symbol", "[x for x in Param.Items]", "shadows an existing binding"},
		{"shadows an enclosing loop variable", "[[i for i in Param.Items] for i in Param.Words]", "shadows an existing binding"},
		{"scalar iterable", "[i for i in Param.Scalar]", "cannot be iterated"},
		{"string iterable", "[c for c in Param.Text]", "cannot be iterated"},
		{"non-bool filter", "[i for i in Param.Items if i]", "must be a bool"},
		{"unknown iterable symbol", "[i for i in Param.Nope]", "unknown symbol"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

func TestEvalListComp_Unresolved(t *testing.T) {
	syms := compSyms(t)
	tests := []struct {
		src      string
		wantType string
	}{
		{"[i for i in Param.Unk]", "unresolved[list[int]]"},
		{"[i * 2 for i in Param.Unk]", "unresolved[list[int]]"},
		{"[i * 1.5 for i in Param.Unk]", "unresolved[list[float]]"},
		{"[i for i in Param.Unk if i > 1]", "unresolved[list[int]]"},
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
				t.Fatalf("Eval(%q) is not unresolved, but its iterable had no value", tc.src)
			}
		})
	}
}

// TestEvalListComp_DepthIsBounded pins that nesting a comprehension inside
// another's element expression reaches the depth guards rather than the stack.
func TestEvalListComp_DepthIsBounded(t *testing.T) {
	src := strings.Repeat("[", 600) + "1" + strings.Repeat(" for q in [1]]", 600)
	if _, err := Parse(src); err == nil {
		t.Fatal("Parse of a 600-deep comprehension = nil error, want the depth guard")
	}
}
