// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for ValidateWithOptions and the SubmitterOptions.EnforceLimits plumbing.
//
// ValidateWithOptions{EnforceLimits:true} must match Validate exactly. With
// EnforceLimits=false the quantitative limit checks are skipped while all
// structural-correctness checks still run.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── ValidateWithOptions ───────────────────────────────────────────────────────

func TestValidateWithOptions_EnforceLimitsTrue_SameAsValidate(t *testing.T) {
	// Validate is documented as a thin wrapper around
	// ValidateWithOptions{EnforceLimits:true}, so the two must produce the exact
	// same errors — even on a limit-violating template. Deep-equal, not just len.
	cases := []struct {
		name string
		yaml string
	}{
		{name: "valid template", yaml: minimalValidYAML()},
		{name: "missing name", yaml: `
specificationVersion: jobtemplate-2023-09
name: ""
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`},
		{name: "step name too long", yaml: `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: ` + strings.Repeat("a", 65) + `
    script:
      actions:
        onRun:
          command: echo
`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, tc.yaml)
			got := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
			want := openjd.Validate(tmpl)

			if !reflect.DeepEqual([]openjd.ValidationError(got), []openjd.ValidationError(want)) {
				t.Errorf("ValidateWithOptions(EnforceLimits:true) != Validate\n got:  %v\n want: %v", got, want)
			}
		})
	}
}

// TestValidateWithOptions_EnforceLimitsToggle proves the gate actually changes
// behavior: a limit-violating template fails the limit check only when
// EnforceLimits is true; the limit error must not appear when it is false.
func TestValidateWithOptions_EnforceLimitsToggle(t *testing.T) {
	const wantPtr = "/steps/0/name"
	yaml := `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: ` + strings.Repeat("a", 65) + `
    script:
      actions:
        onRun:
          command: echo
`
	tmpl := mustParse(t, yaml)

	withFalse := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
	if containsPointer(withFalse, wantPtr) {
		t.Errorf("EnforceLimits=false should not report limit error %q; got %v", wantPtr, withFalse)
	}

	withTrue := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
	if !containsPointer(withTrue, wantPtr) {
		t.Errorf("EnforceLimits=true should report limit error %q; got %v", wantPtr, withTrue)
	}
}

func TestValidateWithOptions_EnforceLimitsFalse_NoPanic(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	// Must not panic and must return no errors for a valid template.
	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
	if len(errs) != 0 {
		t.Errorf("ValidateWithOptions(EnforceLimits:false) on valid template: got errors %v", errs)
	}
}

func TestValidateWithOptions_EnforceLimitsFalse_StillCatchesBasicErrors(t *testing.T) {
	// Even with EnforceLimits:false, structural errors (missing name, etc.)
	// must still be reported — they are not gated by EnforceLimits.
	tmpl := mustParse(t, minimalValidYAML())
	tmpl.Name = "" // structural error: name required

	errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})
	if len(errs) == 0 {
		t.Error("ValidateWithOptions(EnforceLimits:false) should still report missing name")
	}
}

func TestValidateWithOptions_SameResultsRegardlessOfEnforceLimits(t *testing.T) {
	// For templates that violate no quantitative limits, both options must yield
	// identical results — only limit checks are gated by EnforceLimits.
	cases := []struct {
		name  string
		yaml  string
		valid bool
	}{
		{name: "valid template", yaml: minimalValidYAML(), valid: true},
		{name: "missing name", yaml: `
specificationVersion: jobtemplate-2023-09
name: ""
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`},
		{name: "no steps", yaml: `
specificationVersion: jobtemplate-2023-09
name: NoSteps
`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, tc.yaml)
			withTrue := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})
			withFalse := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: false})

			if len(withTrue) != len(withFalse) {
				t.Errorf("results differ: EnforceLimits:true gave %d errors, false gave %d",
					len(withTrue), len(withFalse))
			}
		})
	}
}

// ── Validate backwards compatibility ─────────────────────────────────────────

func TestValidate_UnchangedByRefactor(t *testing.T) {
	// Validate must behave exactly as before — it is the thin wrapper that calls
	// ValidateWithOptions(t, ValidateOptions{EnforceLimits: true}).
	cases := []struct {
		name     string
		yaml     string
		wantErrs int
	}{
		{
			name:     "valid template",
			yaml:     minimalValidYAML(),
			wantErrs: 0,
		},
		{
			name: "missing specificationVersion",
			yaml: `
name: TestJob
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`,
			wantErrs: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, tc.yaml)
			errs := openjd.Validate(tmpl)
			if len(errs) != tc.wantErrs {
				t.Errorf("Validate: got %d errors, want %d; errors: %v", len(errs), tc.wantErrs, errs)
			}
		})
	}
}

// ── Submitter: EnforceLimits plumbing ─────────────────────────────────────────

func TestNewSubmitterWithOptions_EnforceLimitsFalse_SubmitsValidTemplate(t *testing.T) {
	ctx := t.Context()
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)

	sub := openjd.NewSubmitterWithOptions(st, openjd.SubmitterOptions{EnforceLimits: false})
	_, err := sub.Submit(ctx, minimalJSON("optsTest"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Errorf("Submit with EnforceLimits:false on valid template: got error %v", err)
	}
}

func TestNewSubmitter_DefaultEnforceLimitsTrue_SubmitsValidTemplate(t *testing.T) {
	ctx := t.Context()
	st := fake.New()
	farmID, queueID := seedSubmitPrereqs(t, st)

	sub := openjd.NewSubmitter(st)
	_, err := sub.Submit(ctx, minimalJSON("defaultOptsTest"), store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID:  farmID,
		QueueID: queueID,
	})
	if err != nil {
		t.Errorf("Submit with default (EnforceLimits:true) on valid template: got error %v", err)
	}
}
