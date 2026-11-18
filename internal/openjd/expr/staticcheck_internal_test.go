// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

// TestNamesThatResolveNowhere_AreReportedFromSkippablePart pins RFC 0005's
// "Names that Resolve Nowhere". Static type checking evaluates parts of an
// expression a run may skip — the branches of a conditional with an unknown
// condition, the operand of and/or after one whose value is not known, the
// links of a chained comparison after an unknown one — and suppresses a failure
// there, because a run that skips the part succeeds. A name that resolves
// nowhere is the exception: whether it exists does not depend on any value, so
// every run that reaches the part fails on it.
func TestNamesThatResolveNowhere_AreReportedFromSkippablePart(t *testing.T) {
	syms := MapSymbols{"Param.Zero": Unresolved(TInt), "Param.Flag": Unresolved(TBool)}
	tests := []struct {
		src  string
		want string
	}{
		{"1 if Param.Zero == 0 else strr(1)", `unknown function "strr"`},
		{"strr(1) if Param.Zero == 0 else 1", `unknown function "strr"`},
		{"1 if Param.Zero == 0 else Param.Typo", `unknown symbol "Param.Typo"`},
		{"1 if Param.Zero == 0 else 'a'.uper()", `unknown function "uper"`},
		{"1 if Param.Zero == 0 else 'a'.nam", `unknown property "nam"`},
		{"Param.Flag and strr(1)", `unknown function "strr"`},
		{"Param.Flag or Param.Typo", `unknown symbol "Param.Typo"`},
		{"1 < Param.Zero < strr(1)", `unknown function "strr"`},
		{"1 < Param.Zero < 2 < Param.Typo", `unknown symbol "Param.Typo"`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Eval(%q) = %v, %v; want an error containing %q", tc.src, v, err, tc.want)
			}
		})
	}
}

// TestValueDependentFailures_AreStillSuppressedInSkippablePart is the other
// half: a failure that depends on the values' types, such as x.upper() with
// x: int, is still suppressed, so these all type-check.
func TestValueDependentFailures_AreStillSuppressedInSkippablePart(t *testing.T) {
	syms := MapSymbols{"Param.Zero": Unresolved(TInt), "Param.Flag": Unresolved(TBool)}
	for _, src := range []string{
		"1 if Param.Zero == 0 else (1).upper()",
		"1 if Param.Zero == 0 else 1 + 'a'",
		"Param.Flag and 1 + 'a'",
		"1 < Param.Zero < 'a'",
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := Eval(src, syms, TAny); err != nil {
				t.Errorf("Eval(%q) failed: %v", src, err)
			}
		})
	}
}

// TestListValues_CannotNestThreeLevels pins RFC 0005 §1.2.1's constraint that
// the T of list[T] may be list[S] but S may not be a list, for list values
// built by a literal or by a comprehension over known elements (§1.3.7: "A
// result that would nest lists three levels deep is an error").
func TestListValues_CannotNestThreeLevels(t *testing.T) {
	for _, src := range []string{
		"[[[1]]]",
		"[[[x]] for x in [1, 2]]",
		"[[x] for x in [[1], [2]]]",
		"flatten([[[1]], [[2]]])",
		"repr_pwsh([[['a']], [['b']]])",
		"[[[1]]] < [[[]]]",
	} {
		t.Run(src, func(t *testing.T) {
			v, err := Eval(src, MapSymbols{}, TAny)
			if err == nil || !strings.Contains(err.Error(), "three levels") {
				t.Fatalf("Eval(%q) = %v, %v; want a three-level nesting error", src, v, err)
			}
		})
	}
	for _, src := range []string{"[[1], [2]]", "[[x] for x in [1, 2]]", "[x for x in [[1], [2]]]"} {
		t.Run(src, func(t *testing.T) {
			if _, err := Eval(src, MapSymbols{}, TAny); err != nil {
				t.Errorf("Eval(%q) failed: %v", src, err)
			}
		})
	}
}
