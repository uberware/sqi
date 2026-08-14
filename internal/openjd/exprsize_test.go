// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// oversizedExprTemplate builds a template whose single expression is far past
// expr's maxSourceBytes, in the shape that defeats that package's parse-DEPTH
// guard: a flat left-associative chain, parsed in a loop rather than by
// recursion.
//
// 400,000 bytes rather than the 4 MB of the original report, only so the
// fixture stays cheap to build; the bound it trips is the same one, and with
// the bound removed this body is still a ~40 MB parse.
func oversizedExprTemplate() string {
	return `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: T
steps:
- name: S
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ 1` + strings.Repeat("+1", 200_000) + ` }}"]
`
}

// TestValidateWithBudget_OversizedExpressionIsAValidationError is the
// production-path half of expr's maxSourceBytes: the bound must fire where a
// submission actually reaches it, not only in a direct expr.Parse call.
//
// THE CLASSIFICATION IS THE ASSERTION. "This expression is too long" is a
// deterministic property of the body — the same template gets the same verdict
// on every machine, at every load — so it is a ValidationError and becomes a
// 422. It must NOT arrive as the wall-clock error, which
// ValidateWithBudget returns separately and internal/api maps to 503: that
// channel means "the server gave up", and a template that is invariably
// invalid must never be reported through it. The deadline here is set
// deliberately generously so that a breach would be a real finding rather than
// an artifact of a slow machine.
func TestValidateWithBudget_OversizedExpressionIsAValidationError(t *testing.T) {
	tmpl, err := Parse([]byte(oversizedExprTemplate()), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	start := time.Now()
	errs, ferr := ValidateWithBudget(tmpl, ValidateOptions{
		EnforceLimits: true,
		Deadline:      time.Now().Add(time.Minute),
	})
	elapsed := time.Since(start)

	if ferr != nil {
		t.Fatalf("ValidateWithBudget returned a non-validation error %v; "+
			"an oversized expression must be a 422, not the 503 deadline path", ferr)
	}
	if errors.Is(ferr, expr.ErrDeadlineExceeded) {
		t.Fatal("an oversized expression was reported as a deadline breach")
	}

	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "bytes of source") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no size ValidationError among %d errors: %v", len(errs), errs)
	}

	// The message assertion above is what proves the bound FIRED — nothing
	// else in either package words an error that way, and with the bound
	// disabled this same template comes back rejected anyway, by maxEvalDepth,
	// after a full 400 KB parse ("this expression is nested too deeply to
	// evaluate", measured). This second assertion is the coarse backstop for a
	// walk that runs away: it should take milliseconds, so the margin is three
	// orders of magnitude and no realistic machine load reaches it.
	if elapsed > 5*time.Second {
		t.Errorf("rejecting an oversized expression took %v; the bound is probably "+
			"no longer applying before the source is tokenized", elapsed)
	}
}
