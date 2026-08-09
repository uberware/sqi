// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

func TestCheckLetBindings_SequentialTypePropagation(t *testing.T) {
	syms := expr.MapSymbols{"Param.Count": expr.Unresolved(expr.TInt)}
	errs := checkLetBindings(
		[]string{"x = Param.Count", "y = x + 1", "z = string(y)"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 0 {
		t.Fatalf("checkLetBindings = %v, want no errors", errs)
	}
	for name, want := range map[string]expr.Type{"x": expr.TInt, "y": expr.TInt, "z": expr.TString} {
		v, ok := syms[name]
		if !ok {
			t.Errorf("%q not bound", name)
			continue
		}
		// Param.Count is itself an unresolved placeholder (this test's syms
		// carries no concrete value for it), so every binding derived from it
		// stays unresolved too -- correctly: Eval propagates "value not known
		// yet" through arithmetic and calls exactly as it propagates a known
		// value, per E2's one-code-path model. What this test pins is the
		// NATURAL type each binding carries underneath that placeholder
		// status (3.6.1's "the type of the binding is the natural result type
		// of the expression"), so the comparison unwraps one layer of
		// "unresolved[T]" to T before comparing -- the same unwrap
		// unwrapUnresolved performs inside the expr package, done here by
		// hand since that helper is unexported.
		if !v.IsUnresolved() {
			t.Errorf("%q bound as %v, want an unresolved placeholder (Param.Count carries no concrete value in this test)", name, v.Type)
		}
		got := v.Type
		if got.Code == expr.CodeUnresolved && len(got.Params) == 1 {
			got = got.Params[0]
		}
		if !got.Equal(want) {
			t.Errorf("%q bound as %v, want %v", name, v.Type, want)
		}
	}
}

func TestCheckLetBindings_TypeErrorReportsAtItsOwnPointer(t *testing.T) {
	syms := expr.MapSymbols{"Param.Count": expr.Unresolved(expr.TInt)}
	errs := checkLetBindings(
		[]string{"x = Param.Count", "bad = x + 'hello'", "after = 1"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	if errs[0].Pointer != "/steps/0/let/1" {
		t.Errorf("pointer = %q, want %q", errs[0].Pointer, "/steps/0/let/1")
	}
	if _, ok := syms["bad"]; ok {
		t.Error("a failed binding was inserted into the table; it must not be")
	}
	if _, ok := syms["after"]; !ok {
		t.Error("a later binding was skipped; one bad binding must not stop the block")
	}
}

func TestCheckLetBindings_SelfReferenceIsAnUnknownName(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings([]string{"x = x + 1"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	// The REASON matters: self-reference must fail because the name does not
	// exist yet, which is what makes it fall out of ordering with no special
	// case. A test asserting only "it failed" would pass even if some unrelated
	// rule were doing the work.
	if !strings.Contains(errs[0].Message, "x") {
		t.Errorf("message %q does not name the unresolved symbol", errs[0].Message)
	}
	if _, ok := syms["x"]; ok {
		t.Error("x was bound despite its own binding failing")
	}
}

func TestCheckLetBindings_MalformedBindingReportsAtItsIndex(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings([]string{"a = 1", "Foo = 2"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/let/1" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/let/1", errs)
	}
}

func TestCheckLetBindings_HostOnlyFunctionRejectedInNonHostScope(t *testing.T) {
	syms := expr.MapSymbols{"Param.P": expr.Unresolved(expr.TPath)}
	errs := checkLetBindings(
		[]string{"p = apply_path_mapping(Param.P)"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	if !strings.Contains(errs[0].Message, "host-context") {
		t.Errorf("message %q is not the host-context rejection", errs[0].Message)
	}
}

// TestCheckLetBindings_OverBudgetEvalIsRejectedAtSubmissionLimit pins that
// checkLetBindings' Eval call is metered by submissionLimits(), not by
// expr.Eval's own much looser execution-time defaults (10,000,000
// operations). Without that, an unmetered evaluation on the synchronous
// POST /api/v1/jobs path is the same class of Critical E2's whole-branch
// review already found once (~9 minutes of server CPU per request) --
// nothing else in the repo can catch a dropped submissionLimits() here:
// checkLetBindings has no caller yet, so neither the conformance suite nor
// the oracle differential test ever reaches this code.
//
// The binding below costs ~10,953 operations (a 200-iteration comprehension,
// each iteration building and upper-casing a 900,000-byte string) -- comfortably
// over submissionOperationLimit (10,000) but a small fraction of expr.Eval's
// 10,000,000-operation default, so this asserts specifically on the
// SUBMISSION limit tripping, not merely on some error occurring: a generic
// "contains an error" assertion would still pass at the default budget and
// pin nothing about which limit fired.
func TestCheckLetBindings_OverBudgetEvalIsRejectedAtSubmissionLimit(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings(
		[]string{"a = max([len(('y' * 900000).upper()) for i in range(200)])"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	if !strings.Contains(errs[0].Message, "limit of 10000") {
		t.Errorf("message %q does not name the submission operation limit (10000)", errs[0].Message)
	}
	if _, ok := syms["a"]; ok {
		t.Error("an over-budget binding was inserted into the table; it must not be")
	}
}

func TestCheckLetBindings_RejectsDuplicateInSameBlock(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings([]string{"x = 1", "x = 2"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/let/1" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/let/1", errs)
	}
	if !strings.Contains(errs[0].Message, "shadow") {
		t.Errorf("message %q does not say the binding shadows", errs[0].Message)
	}
	v, ok := syms["x"]
	if !ok {
		t.Fatal("x is not bound at all")
	}
	if got := v.AsInt(); got != 1 {
		t.Errorf("x = %v, want the FIRST binding's value 1", got)
	}
}

func TestCheckLetBindings_RejectsShadowingAnEnclosingBlock(t *testing.T) {
	// The enclosing block's names are already in syms -- that is what makes
	// "same block" and "any enclosing scope" one check rather than two.
	syms := expr.MapSymbols{}
	if errs := checkLetBindings([]string{"x = 1"}, "/steps/0/let", ScopeStepTemplate, syms); len(errs) != 0 {
		t.Fatalf("outer block: %v", errs)
	}
	errs := checkLetBindings([]string{"x = 2"}, "/steps/0/script/let", ScopeStepScript, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/script/let/0" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/script/let/0", errs)
	}
}

// TestCheckLetBindings_RejectsShadowingAPreexistingTableEntry replaces the
// task brief's original third test, which seeded syms with the name
// "lower.Thing" to prove the check is keyed off the syms TABLE rather than
// some parallel list of names checkLetBindings itself has bound. That input
// is unreachable through the real grammar: "." is not a legal
// <UserIdentifier> character (letbinding.go's isLetBindingNameCont), so
// parseLetBinding rejects "lower.Thing = 1" for its name before the
// shadowing check this task adds ever runs -- the test would still see a
// non-empty errs slice, but for the wrong reason, and would keep "passing"
// even if the shadowing check were deleted entirely.
//
// This restructures the same intent with a name the grammar actually admits:
// syms is seeded directly (bypassing checkLetBindings, standing in for
// whatever future mechanism -- a wider identifier rule, a scope model with
// its own pre-bound names -- might one day place a non-let-derived entry in
// the table) with a plain lowercase identifier, "thing", that a let binding
// COULD legally produce. Binding over it still proves the check consults
// syms itself rather than a self-maintained set of "names seen so far in
// this call", since "thing" was never seen by this call until the lookup
// that rejects it.
func TestCheckLetBindings_RejectsShadowingAPreexistingTableEntry(t *testing.T) {
	syms := expr.MapSymbols{"thing": expr.Unresolved(expr.TString)}
	errs := checkLetBindings([]string{"thing = 1"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/let/0" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/let/0", errs)
	}
	if !strings.Contains(errs[0].Message, "shadow") {
		t.Errorf("message %q does not say the binding shadows", errs[0].Message)
	}
}
