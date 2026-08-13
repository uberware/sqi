// SPDX-License-Identifier: AGPL-3.0-or-later

package product

// Tests for EXPR sub-project H1's two additions to product template
// validation: the operator's configured EXPR limits, and the wall-clock
// deadline that bounds POST/PUT /api/v1/products.
//
// They are INTERNAL tests because neither addition is observable from outside
// this package today. While the EXPR registry entry is StatusInProgress the
// expression walk never runs, so no limit and no deadline can change any
// outcome that ValidateTemplate's exported signature can return — and the
// registry is a package-level map in internal/openjd that only that package's
// own tests can flip. What is checkable here is the plumbing: the mapping onto
// openjd.ValidateOptions, and that the options this package produces really do
// stop a walk when one is forced on.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
)

// exprProductTemplate declares EXPR and enough expression work for the meter's
// periodic deadline check to be reached. A bare list literal would not do: it
// performs no meter.charge calls at all, so the clock is never sampled and the
// expression resolves successfully.
const exprProductTemplate = `specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: Demo
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args: ["{{ [x * 2 for x in range(100000)] }}"]`

// TestValidateOptions_MapOntoOpenJD pins the field-by-field mapping.
//
// It is worth a test for the reason internal/server's ExprLimitsFromConfig is:
// a struct literal copying one options type into another is the shape where an
// assignment can be dropped or pointed at the wrong member and still compile,
// start and serve. Every value below is non-default, so a mapping that
// substituted a zero value could not pass.
func TestValidateOptions_MapOntoOpenJD(t *testing.T) {
	deadline := time.Unix(1_700_000_000, 0)
	limits := openjd.ExprLimits{
		SubmissionOperations:  4321,
		SubmissionMemoryBytes: 654_321,
		TemplatePositions:     321,
		TemplateRetainedBytes: 21_000,
	}

	got := ValidateOptions{EnforceLimits: true, ExprLimits: limits, Deadline: deadline}.openjdOptions()

	if !got.EnforceLimits {
		t.Error("EnforceLimits = false, want the configured true")
	}
	if got.ExprLimits != limits {
		t.Errorf("ExprLimits = %+v, want the operator's %+v -- product template validation "+
			"would silently run on openjd.DefaultExprLimits() instead", got.ExprLimits, limits)
	}
	if !got.Deadline.Equal(deadline) {
		t.Errorf("Deadline = %v, want %v -- POST /api/v1/products would walk an arbitrary "+
			"client-supplied template with no bound on elapsed time", got.Deadline, deadline)
	}
	if got.CheckEXPRExpressionsWhileUnsupported {
		t.Error("CheckEXPRExpressionsWhileUnsupported = true; production must never set it -- " +
			"it forces the expression walk for a template the status gate rejects anyway")
	}
}

// TestValidateOptions_ZeroIsThePreH1Behavior pins that a caller offering no
// configuration — ParseDefinition, loading built-ins from package init where no
// config exists yet — is unchanged by H1.
func TestValidateOptions_ZeroIsThePreH1Behavior(t *testing.T) {
	got := ValidateOptions{EnforceLimits: true}.openjdOptions()
	if got.ExprLimits != (openjd.ExprLimits{}) {
		t.Errorf("ExprLimits = %+v, want the zero value (which openjd reads as its defaults)", got.ExprLimits)
	}
	if !got.Deadline.IsZero() {
		t.Errorf("Deadline = %v, want the zero time: no deadline, and no clock read", got.Deadline)
	}
}

// TestValidateParsed_DeadlineSurvivesAsTheSentinel is the composition test: the
// options this package builds really do stop the walk, and the resulting error
// is still matchable with errors.Is by the time it leaves this package.
//
// It forces the walk on (CheckEXPRExpressionsWhileUnsupported) because
// production cannot reach it: while EXPR is StatusInProgress, validateExtensions
// rejects every EXPR-declaring template before a single expression is
// evaluated. That is also why H1's deadline is INERT in production today, and
// why landing it before sub-project H2 flips the status is the whole point.
//
// The errors.Is assertion is the load-bearing half. internal/api tells a
// deadline from a bad template STRUCTURALLY, on this sentinel; a wrapper here
// that flattened the error to a string would turn every 503 into a 400 with
// nothing failing.
func TestValidateParsed_DeadlineSurvivesAsTheSentinel(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(exprProductTemplate), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	o := ValidateOptions{
		EnforceLimits: true,
		Deadline:      time.Now().Add(-time.Second), // already expired
	}.openjdOptions()
	o.CheckEXPRExpressionsWhileUnsupported = true

	verr := validateParsed(tmpl, o)
	if verr == nil {
		t.Fatal("validation succeeded with an already-expired deadline; the deadline is " +
			"probably not reaching the evaluator")
	}
	if !errors.Is(verr, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap expr.ErrDeadlineExceeded", verr)
	}
	if !strings.HasPrefix(verr.Error(), "product: ") {
		t.Errorf("error = %q, want this package's prefix so the source is identifiable", verr)
	}
}

// TestValidateParsed_InvalidTemplateIsNotADeadline is the other half: an
// ordinary bad template must NOT look like the backstop, or internal/api would
// answer 503 ("retry, the server gave up") to a submitter whose template is
// wrong and will never validate.
func TestValidateParsed_InvalidTemplateIsNotADeadline(t *testing.T) {
	tmpl, err := openjd.Parse([]byte(`specificationVersion: jobtemplate-2023-09
name: Demo
steps: []`), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	verr := validateParsed(tmpl, ValidateOptions{EnforceLimits: true}.openjdOptions())
	if verr == nil {
		t.Fatal("a template with no steps was accepted")
	}
	if errors.Is(verr, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want NOT the deadline sentinel", verr)
	}
}
