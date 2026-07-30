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
		"Param.Maybe":  Unresolved(TBool),
		"x":            Int(99),
		// Mystery, LetFlag and NotBoolFlag are bare (not Param.*) because they
		// stand in for a "let" binding, matching how the conformance harness's
		// DeclaredSymbols binds one (test/conformance/exprcase.go): untyped,
		// as expr.TAny.
		"Mystery":     Unresolved(TInt),
		"LetFlag":     Unresolved(TAny),
		"NotBoolFlag": Unresolved(TString),
		// Mixed is declared list[float] but its items are actually Int —
		// something evalListLit itself would never build (coerce makes every
		// item's type match a list literal's own declared element type), but
		// nothing stops a caller from injecting one through Symbols. It
		// exists to tell apart "the iterable's OWN declared element type"
		// from "the first collected item's own type", which agree for every
		// list this package builds itself and so are otherwise
		// indistinguishable from any test built only out of well-formed
		// lists.
		"Param.Mixed": List(TFloat, []Value{Int(1), Int(2), Int(3)}),
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

// evalRaw parses and evaluates src against target WITHOUT Expression.Eval's own
// final boundary coerce.
//
// That final coerce narrows the WHOLE expression's result to target no matter
// what an inner node computed, which masks exactly the class of bug
// TestEvalListComp_TargetFlowsInward exists to catch: when the comprehension
// IS the entire expression, Eval's own boundary call quietly repairs a wrong
// inner derivation (an unresolved list[int] narrows straight to list[float]
// when the target asks for it, whether or not the inner code that produced
// list[int] ever consulted the target at all), so a test built on the public
// Eval alone cannot tell a fixed unresolvedComp from the unfixed one. Calling
// evalNode directly, the same way Eval does internally before that final
// step, observes the comprehension's own derivation honestly.
func evalRaw(t *testing.T, src string, syms Symbols, target Type) (Value, error) {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return evalNode(e.root, e.src, syms, target, 0)
}

// TestEvalListComp_TargetFlowsInward pins that the element expression is an
// identity position for the target type, like a list literal's elements — on
// BOTH the resolved iterable path (runComp's coerce call) and the unresolved
// one (unresolvedComp). The two are pinned separately because they used to
// disagree: the unresolved path used to derive its result type solely from
// the element expression and silently drop the target, so the same source
// text reported two different static types depending on whether its iterable
// happened to be resolved at the time. The unresolved-path subtests use
// evalRaw, not Eval, because Eval's own final boundary coerce independently
// narrows the WHOLE result to target and would pass even against the
// unfixed code — see evalRaw's doc.
func TestEvalListComp_TargetFlowsInward(t *testing.T) {
	syms := compSyms(t)
	target, err := ParseType("list[float]")
	if err != nil {
		t.Fatalf("ParseType: %v", err)
	}
	t.Run("resolved iterable", func(t *testing.T) {
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
	})
	t.Run("unresolved iterable", func(t *testing.T) {
		v, err := evalRaw(t, "[i for i in Param.Unk]", syms, target)
		if err != nil {
			t.Fatalf("evalRaw: %v", err)
		}
		if got, want := v.Type.String(), "unresolved[list[float]]"; got != want {
			t.Errorf("type = %s, want %s", got, want)
		}
		if !v.IsUnresolved() {
			t.Fatal("value is not unresolved, but its iterable had no value")
		}
	})
	t.Run("target rejects an incompatible element", func(t *testing.T) {
		intTarget, err := ParseType("list[int]")
		if err != nil {
			t.Fatalf("ParseType: %v", err)
		}
		// true cannot coerce to int (coerce.go's scalarCoercible has no
		// bool -> int rule), so this must be reported exactly as the
		// resolved path's coerce() call would report it — as "bool cannot be
		// coerced to int", blaming the "true" literal itself — rather than
		// silently accepted (the unfixed unresolvedComp never checks the
		// element against the target at all, so it returns
		// unresolved[list[bool]] with no error here).
		_, err = evalRaw(t, "[true for i in Param.Unk]", syms, intTarget)
		if err == nil {
			t.Fatal("evalRaw = nil error, want the element rejected against the target")
		}
		if !strings.Contains(err.Error(), "bool cannot be coerced to int") {
			t.Fatalf("evalRaw error = %q, want it to mention %q", err.Error(), "bool cannot be coerced to int")
		}
	})
}

// TestEvalListComp_MidIterationUnresolved exercises runComp's fallback to
// unresolvedComp when a CONCRETE iterable's filter or element turns out
// unresolved partway through iteration — the subtlest path in the file.
// TestEvalListComp_Unresolved does not reach it: it only varies the
// ITERABLE, which short-circuits to unresolvedComp before runComp's loop
// ever starts.
func TestEvalListComp_MidIterationUnresolved(t *testing.T) {
	syms := compSyms(t)
	tests := []struct {
		name     string
		src      string
		wantType string
	}{
		{
			"unresolved filter over a concrete iterable",
			"[i for i in Param.Items if Param.Maybe]",
			"unresolved[list[int]]",
		},
		{
			"unresolved element over a concrete iterable",
			"[Mystery for i in Param.Items]",
			"unresolved[list[int]]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Fatalf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
			if !v.IsUnresolved() {
				t.Fatalf("Eval(%q) is not unresolved, but its filter/element had no value", tc.src)
			}
		})
	}
}

// TestEvalListComp_ElemTypeIsThreadedNotRederived pins Minor finding 2 (review
// round 1): runComp's fallback to unresolvedComp must use the SAME elemType
// evalListComp computed from the iterable's own declared type, not re-derive
// one from whatever partial items runComp happened to collect before falling
// back.
//
// The two agree for any list this package's own evaluator builds — coerce
// always makes every item's type match the list's declared element type — so
// Param.Mixed (compSyms) exists specifically to break that agreement: it is
// declared list[float] but its items are actually Int, which only a value
// injected directly through Symbols can produce.
func TestEvalListComp_ElemTypeIsThreadedNotRederived(t *testing.T) {
	syms := compSyms(t)
	v, err := Eval("[i for i in Param.Mixed if Param.Maybe]", syms, TAny)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	// The iterable's OWN declared element type is float; the first collected
	// item's raw payload type (int) must not leak into the result.
	if got, want := v.Type.String(), "unresolved[list[float]]"; got != want {
		t.Errorf("type = %s, want %s", got, want)
	}
}

// TestEvalListComp_FilterAcceptsAnyTypedPlaceholder pins the fix for a filter
// bound to an untyped ("any") placeholder — exactly what a "let" binding is
// bound as by the conformance harness's DeclaredSymbols
// (test/conformance/exprcase.go's letSymbols) — being wrongly rejected as
// "not a bool" when it COULD, at runtime, turn out to be one. It must defer,
// like evalCond does for the identical question about its own condition, not
// reject outright. A placeholder that could never be a bool must still be
// rejected, which the third case pins so the fix does not overcorrect into
// accepting everything.
func TestEvalListComp_FilterAcceptsAnyTypedPlaceholder(t *testing.T) {
	syms := compSyms(t)
	t.Run("any-typed placeholder defers rather than rejects, reached via the mid-iteration fallback", func(t *testing.T) {
		v, err := Eval("[i for i in Param.Items if LetFlag]", syms, TAny)
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if got, want := v.Type.String(), "unresolved[list[int]]"; got != want {
			t.Errorf("type = %s, want %s", got, want)
		}
	})
	t.Run("an unresolved iterable reaches the same check directly", func(t *testing.T) {
		v, err := Eval("[i for i in Param.Unk if LetFlag]", syms, TAny)
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if got, want := v.Type.String(), "unresolved[list[int]]"; got != want {
			t.Errorf("type = %s, want %s", got, want)
		}
	})
	t.Run("a placeholder that could never be bool is still rejected", func(t *testing.T) {
		_, err := Eval("[i for i in Param.Unk if NotBoolFlag]", syms, TAny)
		if err == nil {
			t.Fatal("Eval = nil error, want a bool-filter rejection")
		}
		if !strings.Contains(err.Error(), "must be a bool") {
			t.Fatalf("Eval error = %q, want it to mention %q", err.Error(), "must be a bool")
		}
	})
	t.Run("a placeholder that could never be bool is rejected directly over a concrete iterable too", func(t *testing.T) {
		// Unlike the previous case, Param.Items is CONCRETE, so evaluation
		// reaches evalCompFilter's own includes check first (runComp's
		// per-item loop), which now rejects on the spot rather than only
		// deferring to checkCompFilter's identical check by way of the
		// unresolvedComp fallback. The two checks are intentionally
		// redundant here — for a filter that does not depend on the loop
		// variable, checkCompFilter's fallback would report the identical
		// error either way, so this pins the end-to-end behavior rather
		// than discriminating evalCompFilter's branch in isolation.
		_, err := Eval("[i for i in Param.Items if NotBoolFlag]", syms, TAny)
		if err == nil {
			t.Fatal("Eval = nil error, want a bool-filter rejection")
		}
		if !strings.Contains(err.Error(), "must be a bool") {
			t.Fatalf("Eval error = %q, want it to mention %q", err.Error(), "must be a bool")
		}
	})
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
// another's element expression reaches a depth guard rather than the stack.
//
// This exercises the PARSER's depth guard only (Parse, not Eval): at
// maxParseDepth = 500 and roughly 3 parser frames per nested "[... for ... in
// ...]", parsing already rejects source this deeply nested, so evalNode's own
// maxEvalDepth (10,000) can never be reached by nesting comprehensions this
// way at all — there is no source Parse would accept that is deep enough to
// need it.
func TestEvalListComp_DepthIsBounded(t *testing.T) {
	src := strings.Repeat("[", 600) + "1" + strings.Repeat(" for q in [1]]", 600)
	if _, err := Parse(src); err == nil {
		t.Fatal("Parse of a 600-deep comprehension = nil error, want the depth guard")
	}
}
