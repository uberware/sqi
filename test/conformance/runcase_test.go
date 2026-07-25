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

// TestRunCase_NotApplicableNeverPasses is the false-green guard. An ".invalid"
// fixture for an unregistered extension is rejected by sqi because the
// extension is unknown, NOT because sqi detected the fixture's actual defect.
// Reporting that as a pass would make all 209 EXPR fixtures green before any
// EXPR code exists, and keep them green through a broken implementation.
//
// DO NOT DELETE THIS TEST.
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
