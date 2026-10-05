// SPDX-License-Identifier: AGPL-3.0-or-later

package product

// Tests for two parts of product template validation: the operator's
// configured EXPR limits, and the wall-clock deadline that bounds POST/PUT
// /api/v1/products.
//
// The deadline tests drive the exported [ValidateTemplate] end to end. The
// file is internal for the two things only visible from inside: the
// openjdOptions mapping and the validateParsed tail.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/openjd/expr"
	"github.com/uberware/sqi/internal/store"
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
}

// TestValidateOptions_ZeroGivesDefaultLimitsAndNoDeadline pins that a caller offering no
// configuration — ParseDefinition, loading built-ins from package init where no
// config exists yet — gets default EXPR limits and no deadline.
func TestValidateOptions_ZeroGivesDefaultLimitsAndNoDeadline(t *testing.T) {
	got := ValidateOptions{EnforceLimits: true}.openjdOptions()
	if got.ExprLimits != (openjd.ExprLimits{}) {
		t.Errorf("ExprLimits = %+v, want the zero value (which openjd reads as its defaults)", got.ExprLimits)
	}
	if !got.Deadline.IsZero() {
		t.Errorf("Deadline = %v, want the zero time: no deadline, and no clock read", got.Deadline)
	}
}

// TestValidateTemplate_DeadlineSurvivesAsTheSentinel is the composition test:
// the options this package builds really do stop the walk, and the resulting
// error is still matchable with errors.Is by the time it leaves this package.
//
// It drives the EXPORTED [ValidateTemplate] with nothing forced, so what it
// pins is the whole path POST /api/v1/products takes: the raw template, the
// parse, the ValidateOptions -> openjd.ValidateOptions mapping, and the walk
// that mapping is supposed to bound.
//
// The errors.Is assertion is the half that matters. internal/api tells a
// deadline from a bad template STRUCTURALLY, on this sentinel; a wrapper here
// that flattened the error to a string would turn every 503 into a 400 with
// nothing failing.
func TestValidateTemplate_DeadlineSurvivesAsTheSentinel(t *testing.T) {
	verr := ValidateTemplate(exprProductTemplate, store.TemplateFormatYAML, ValidateOptions{
		EnforceLimits: true,
		Deadline:      time.Now().Add(-time.Second), // already expired
	})
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

// TestValidateTemplate_SameFixtureWithoutADeadlineIsNotTheSentinel is the
// non-vacuity guard for the test above, on the SAME fixture.
// TestValidateParsed_InvalidTemplateIsNotADeadline makes the same point with a
// base-spec template that never reaches an expression at all; this one keeps
// everything constant except the deadline, so the sentinel above can only have
// come from the deadline.
//
// The fixture is rejected either way -- 100,000 comprehension iterations trip
// the deterministic operation limit -- which is exactly the discrimination
// internal/api depends on: that rejection is a 4xx the client can act on, and
// only the wall-clock sentinel is the 503 that says "retry, this server gave
// up".
func TestValidateTemplate_SameFixtureWithoutADeadlineIsNotTheSentinel(t *testing.T) {
	err := ValidateTemplate(exprProductTemplate, store.TemplateFormatYAML, ValidateOptions{
		EnforceLimits: true,
	})
	if err == nil {
		t.Fatal("the fixture was accepted with no deadline; it is meant to be over-budget, " +
			"so the deadline test above may be passing on the wrong rejection")
	}
	if errors.Is(err, expr.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want NOT the deadline sentinel: no deadline was set", err)
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

// TestParseDefinition_PassesOptionsThrough pins that [ParseDefinition]'s opts
// argument actually reaches the validator.
//
// The preset install path (internal/presetlib, reached from
// POST /api/v1/presets/{name}/install) reaches the validator through this
// argument. An opts parameter that compiled but was dropped on the way to
// ValidateTemplate would leave that path validating on
// openjd.DefaultExprLimits() with no deadline while the sibling product route
// honors the operator's configuration.
//
// EnforceLimits is the field used to detect the pass-through because it has an
// observable effect on a plain base-spec template. A template whose job name exceeds
// the 128-character limit is rejected only when limits are enforced, so the two
// calls below must disagree.
func TestParseDefinition_PassesOptionsThrough(t *testing.T) {
	def := "name: studio/over-long\ntitle: Over Long\ntemplate:\n" +
		"  specificationVersion: jobtemplate-2023-09\n" +
		"  name: " + strings.Repeat("x", 200) + "\n" +
		"  steps:\n" +
		"    - name: Run\n" +
		"      script:\n" +
		"        actions:\n" +
		"          onRun:\n" +
		"            command: echo\n"

	if _, err := ParseDefinition([]byte(def), ValidateOptions{EnforceLimits: false}); err != nil {
		t.Fatalf("ParseDefinition with limits off: %v (the fixture is meant to breach "+
			"only a GATED limit, so this call must succeed)", err)
	}
	_, err := ParseDefinition([]byte(def), ValidateOptions{EnforceLimits: true})
	if err == nil {
		t.Fatal("ParseDefinition accepted a 200-character job name with EnforceLimits set; " +
			"the opts argument is not reaching ValidateTemplate")
	}
	if !strings.Contains(err.Error(), "128 characters") {
		t.Errorf("error = %q, want the job-name length limit", err)
	}
}
