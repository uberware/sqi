// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// Test for sub-project E2's Task 10: wiring checkTemplateExpressions
// (sub-project E2's own phase-1/phase-2 checker, see exprcheck.go) into
// Submit as phase 2 of the specification's Progressive Expression
// Evaluation model, via the checkExpressionsAtSubmit helper in submit.go.
//
// This is a package-openjd (white-box) test, not an addition to
// submit_test.go's package-openjd_test suite, because the thing being
// proved -- phase 1 (unresolved Param.N) accepts an expression that phase 2
// (concrete Param.N) rejects -- cannot currently be observed by driving
// Submitter.Submit end to end: Submit's own phase 1 call
// (ValidateWithOptions, the step before parameter binding) rejects every
// EXPR-declaring template outright, because the EXPR extension is
// registered but not yet StatusSupported (extension.go) and
// validateExtensions enforces that unconditionally. That is a separate,
// deliberate gate (sub-project H's, not this task's) with no bearing on
// whether checkExpressionsAtSubmit itself is wired correctly -- but it does
// mean Submit never reaches parameter binding for such a template today, so
// checkExpressionsAtSubmit is unreachable via the public API until H ships.
// Calling it directly here proves the phase 2 wiring on its own merits,
// independent of that unrelated, temporary gate. See this task's report for
// the full account, including a from-scratch confirmation that Submit
// itself short-circuits at the extension check for any EXPR template.

import (
	"errors"
	"strings"
	"testing"
)

// TestCheckExpressionsAtSubmit_PhaseDistinction is the test the task brief
// asks for: an expression valid with Param.N unresolved but invalid once
// Param.N is concrete. "{{ 10 / Param.N }}" type-checks fine against an
// unresolved INT placeholder (phase 1 has no value to divide by, so nothing
// to fail on) but divides by zero once N is submitted as "0" (phase 2).
func TestCheckExpressionsAtSubmit_PhaseDistinction(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		Extensions:           []string{"EXPR"},
		ParameterDefinitions: []JobParameter{{Name: "N", Type: "INT"}},
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{
				Actions: StepActions{
					OnRun: Action{Command: "echo", Args: []string{"{{ 10 / Param.N }}"}},
				},
			},
		}},
	}

	// Phase 1: Param.N unresolved -- accepted. (Mirrors what
	// ValidateWithOptions does inside Submit before parameter binding, via
	// the same checkTemplateExpressions(tmpl, nil) call this function
	// wraps.)
	if err := checkExpressionsAtSubmit(tmpl, nil); err != nil {
		t.Fatalf("phase 1 (Param.N unresolved) must accept the expression; got: %v", err)
	}

	// Phase 2: Param.N concrete and zero -- rejected.
	err := checkExpressionsAtSubmit(tmpl, map[string]string{"N": "0"})
	if err == nil {
		t.Fatal("phase 2 (Param.N == \"0\") must reject the division by zero")
	}

	var subErr *SubmitValidationError
	if !errors.As(err, &subErr) {
		t.Fatalf("error must be a *SubmitValidationError so the API surfaces it like other submit validation "+
			"failures; got %T: %v", err, err)
	}
	if !strings.Contains(subErr.Error(), "division by zero") {
		t.Fatalf("error should mention the division by zero; got: %v", subErr)
	}

	// Sanity check: a NON-zero concrete value is accepted at phase 2 too --
	// confirms the rejection above is really about the value being zero,
	// not some other break introduced by supplying params at all.
	if err := checkExpressionsAtSubmit(tmpl, map[string]string{"N": "2"}); err != nil {
		t.Fatalf("phase 2 with a nonzero Param.N must accept the expression; got: %v", err)
	}
}

// TestCheckExpressionsAtSubmit_NoOpWithoutEXPR pins that checkExpressionsAtSubmit
// is inert for a template that does not declare the EXPR extension --
// checkTemplateExpressions itself no-ops in that case (see its own doc
// comment), so a parameter that would divide by zero under EXPR is not
// evaluated at all for a base-spec template, and Submit's behavior for the
// common EXPR-off case is unchanged.
func TestCheckExpressionsAtSubmit_NoOpWithoutEXPR(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "N", Type: "INT"}},
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{
				Actions: StepActions{
					OnRun: Action{Command: "echo", Args: []string{"{{ 10 / Param.N }}"}},
				},
			},
		}},
	}

	if err := checkExpressionsAtSubmit(tmpl, map[string]string{"N": "0"}); err != nil {
		t.Fatalf("checkExpressionsAtSubmit must no-op for a template that does not declare EXPR; got: %v", err)
	}
}
