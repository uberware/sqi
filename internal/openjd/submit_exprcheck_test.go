// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

// Tests for wiring checkTemplateExpressions (the phase-1/phase-2 checker, see
// exprcheck.go) into Submit as phase 2 of the specification's Progressive
// Expression Evaluation model, via the checkExpressionsAtSubmit helper in
// submit.go.
//
// This is a package-openjd (white-box) file, not an addition to
// submit_test.go's package-openjd_test suite, because
// TestCheckExpressionsAtSubmit_PhaseDistinction and
// TestCheckExpressionsAtSubmit_NoOpWithoutEXPR call the unexported
// checkExpressionsAtSubmit directly.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// TestCheckExpressionsAtSubmit_PhaseDistinction pins an expression that is
// valid with Param.N unresolved but invalid once
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

	// Phase 1: Param.N unresolved, accepted. This mirrors what
	// ValidateWithOptions does inside Submit before parameter binding, via
	// the same checkTemplateExpressions(tmpl, nil) call this function wraps.
	if err := checkExpressionsAtSubmit(tmpl, nil); err != nil {
		t.Fatalf("phase 1 (Param.N unresolved) must accept the expression; got: %v", err)
	}

	// Phase 2: Param.N concrete and zero, rejected.
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

	// A nonzero concrete value is accepted at phase 2, so the rejection above
	// is due to the zero value and not to supplying parameters at all.
	if err := checkExpressionsAtSubmit(tmpl, map[string]string{"N": "2"}); err != nil {
		t.Fatalf("phase 2 with a nonzero Param.N must accept the expression; got: %v", err)
	}
}

// TestCheckExpressionsAtSubmit_NoOpWithoutEXPR pins that checkExpressionsAtSubmit
// is inert for a template that does not declare the EXPR extension:
// checkTemplateExpressions no-ops in that case (see its doc comment), so a
// parameter that would divide by zero under EXPR is not evaluated at all for a
// base-spec template.
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

// TestSubmit_PhaseDistinction_ThroughRealSubmit drives the phase-2 check
// through the public Submitter.Submit API
// (TestCheckExpressionsAtSubmit_PhaseDistinction covers
// checkExpressionsAtSubmit in isolation). The division-by-zero template clears
// ValidateWithOptions's phase 1 call and reaches parameter binding.
//
// Submitting with N="0" must fail at the phase 2 call with a
// division-by-zero *SubmitValidationError; submitting the same template with
// N="2" must succeed. Removing the checkExpressionsAtSubmit call from
// prepareTemplate (submit.go) makes the N=0 case return a nil error instead,
// failing this test.
func TestSubmit_PhaseDistinction_ThroughRealSubmit(t *testing.T) {
	ctx := context.Background()
	st := fake.New()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "exprcheck-farm"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "exprcheck-queue"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	sub := NewSubmitter(st)

	const tmpl = `
specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: ExprPhaseDistinctionJob
parameterDefinitions:
- name: N
  type: INT
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: echo
        args:
        - "{{ 10 / Param.N }}"
`

	// N == "0": phase 2 must reject the division by zero.
	_, err = sub.Submit(ctx, tmpl, store.TemplateFormatYAML, SubmitOptions{
		FarmID: farm.ID, QueueID: queue.ID, Parameters: map[string]string{"N": "0"},
	})
	if err == nil {
		t.Fatal("N=\"0\" must be rejected by the phase 2 re-check; Submit returned nil error")
	}
	var subErr *SubmitValidationError
	if !errors.As(err, &subErr) {
		t.Fatalf("N=\"0\": expected *SubmitValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(subErr.Error(), "division by zero") {
		t.Fatalf("N=\"0\": expected a division-by-zero message; got: %v", subErr)
	}

	// N == "2": phase 2 accepts and the job is created.
	res, err := sub.Submit(ctx, tmpl, store.TemplateFormatYAML, SubmitOptions{
		FarmID: farm.ID, QueueID: queue.ID, Parameters: map[string]string{"N": "2"},
	})
	if err != nil {
		t.Fatalf("N=\"2\" must be accepted; got: %v", err)
	}
	if res == nil || res.Job.ID == "" {
		t.Fatal("N=\"2\": expected a persisted job, got none")
	}
}
