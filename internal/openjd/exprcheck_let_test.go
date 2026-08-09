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
