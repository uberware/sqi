// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance_test

import (
	"testing"

	"github.com/uberware/sqi/test/conformance"
)

const validTemplate = `
specificationVersion: jobtemplate-2023-09
name: MinimalJob
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
          args: ["hello"]
`

// missingName omits the required top-level "name".
const invalidTemplate = `
specificationVersion: jobtemplate-2023-09
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
`

const malformedYAML = `
specificationVersion: [unclosed
`

func TestRunCase(t *testing.T) {
	tests := []struct {
		name       string
		fixture    string
		data       string
		state      conformance.State
		wantPassed bool
	}{
		{
			name:       "valid template accepted passes",
			fixture:    "base/job_templates/1.1--minimal.yaml",
			data:       validTemplate,
			state:      conformance.StateLive,
			wantPassed: true,
		},
		{
			name:       "valid template rejected fails",
			fixture:    "base/job_templates/1.1--minimal.yaml",
			data:       invalidTemplate,
			state:      conformance.StateLive,
			wantPassed: false,
		},
		{
			name:       "invalid template rejected passes",
			fixture:    "base/job_templates/2.1--missing-name.invalid.yaml",
			data:       invalidTemplate,
			state:      conformance.StateLive,
			wantPassed: true,
		},
		{
			name:       "invalid template accepted fails",
			fixture:    "base/job_templates/2.1--missing-name.invalid.yaml",
			data:       validTemplate,
			state:      conformance.StateLive,
			wantPassed: false,
		},
		{
			name:       "malformed yaml counts as rejection",
			fixture:    "base/job_templates/1.1--broken.invalid.yaml",
			data:       malformedYAML,
			state:      conformance.StateLive,
			wantPassed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := conformance.ParseTestCase(tt.fixture)
			got := conformance.RunCase(tc, tt.state, []byte(tt.data))
			if got.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v (reason: %s)", got.Passed, tt.wantPassed, got.Reason)
			}
		})
	}
}

// TestRunCase_BlanksReasonOnAPass pins the property several
// TestConformance_*Fixtures doc comments in suite_test.go depend on:
// Result.Reason is empty once Passed is true, so a correctly rejected
// ".invalid" fixture carries no record of which rule rejected it. Those tests
// therefore assert only res.Passed, and TestConformance_LetBindingFixtures
// re-runs the validation pipeline (letFixtureReason) to read the error.
func TestRunCase_BlanksReasonOnAPass(t *testing.T) {
	tc := conformance.ParseTestCase("base/job_templates/2.1--missing-name.invalid.yaml")
	got := conformance.RunCase(tc, conformance.StateLive, []byte(invalidTemplate))

	if !got.Passed {
		t.Fatalf("want a passing .invalid fixture to set up the assertion; got Passed=false (%s)", got.Reason)
	}
	if got.Reason != "" {
		t.Errorf("Reason = %q; want it empty on a pass", got.Reason)
	}
}

// TestRunCase_NotApplicableNeverPasses guards against false passes. An
// ".invalid" fixture for an unregistered extension is rejected by sqi because
// the extension is unknown, not because sqi detected the fixture's defect.
// Scoring that as a pass would report every such fixture as passing with no
// implementation behind it.
func TestRunCase_NotApplicableNeverPasses(t *testing.T) {
	tc := conformance.ParseTestCase("EXPR/job_templates/2.10--bad-default.invalid.yaml")
	got := conformance.RunCase(tc, conformance.StateNotApplicable, []byte(invalidTemplate))

	if got.Passed {
		t.Fatal("a not-applicable fixture reported Passed=true; " +
			"this is the false-green failure the three-state classification exists to prevent")
	}
	if got.State != conformance.StateNotApplicable {
		t.Errorf("State = %v, want StateNotApplicable", got.State)
	}
}

// TestRunCase_EnvTemplatesNeverPass guards against the false pass
// TestRunCase_NotApplicableNeverPasses covers, keyed on template kind rather
// than extension. sqi does not implement standalone environment-2023-09
// templates: every env_templates fixture, including under "base", which is
// otherwise always live, is rejected on "/specificationVersion: unsupported
// version", never on the fixture's own defect. A classifier that looked only
// at the extension directory would score each of the 24
// "base/env_templates/*.invalid.yaml" fixtures as a pass.
//
// The test drives the path the suite takes (ExtensionFor + KindFor feeding
// Classify) rather than passing StateNotApplicable directly, so it also
// covers the classification wiring.
func TestRunCase_EnvTemplatesNeverPass(t *testing.T) {
	path := "base/env_templates/2.1--bad-default.invalid.yaml"
	tc := conformance.ParseTestCase(path)
	state := conformance.Classify(conformance.ExtensionFor(path), conformance.KindFor(path))

	got := conformance.RunCase(tc, state, []byte(invalidTemplate))

	if state != conformance.StateNotApplicable {
		t.Fatalf("Classify(%q) = %v, want StateNotApplicable", path, state)
	}
	if got.Passed {
		t.Fatal("a base/env_templates fixture reported Passed=true; " +
			"this is the false-green failure kind-aware classification exists to prevent")
	}
}
