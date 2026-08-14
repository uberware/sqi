// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for the EXPR expression walk's registry-status gate.
//
// HISTORY, because this file is now a remnant. While the EXPR extension was
// registered but not StatusSupported, ValidateWithOptions rejected an
// EXPR-declaring template WITHOUT running checkTemplateExpressions over it,
// and this file's main test pinned that: the verdict byte-for-byte, the
// absence of any walk-only error, and an allocation ratio between the gated
// and opt-in paths. That was a cost property as much as a behavior one — the
// walk was measured at ~134 microseconds of CPU per template byte, and
// POST /api/v1/jobs accepts a 4 MiB body anonymously when auth is off.
//
// Sub-project H2 flipped EXPR to StatusSupported, so the condition that test
// described can never hold again and the test was deleted with it. The cost it
// bounded is now bounded instead by sub-project E4c's template-wide budget
// (positions and retained bytes) and H1's wall-clock submission deadline,
// which is what made the flip safe to make.
//
// What remains here is the one assertion the flip does NOT invalidate: the
// gate never applied to a template that does not declare EXPR, and a base-spec
// out-of-scope reference is still rejected either way.

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// TestValidate_EXPRWalkGate_LeavesNonEXPRTemplatesAlone confirms the gate did
// not change anything for a template that does not declare EXPR: the base-spec
// format-string path (validateFormatString) is not gated by it, so the same
// out-of-scope reference is still rejected either way.
func TestValidate_EXPRWalkGate_LeavesNonEXPRTemplatesAlone(t *testing.T) {
	tmpl := mustParse(t, `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
- name: Step1
  hostRequirements:
    attributes:
      - name: attr.custom.dir
        anyOf:
          - "{{Session.WorkingDirectory}}"
  script:
    actions:
      onRun:
        command: echo
`)
	for _, optIn := range []bool{false, true} {
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: optIn,
		})
		if !containsMessage(errs, "Session.WorkingDirectory") {
			t.Fatalf("optIn=%v: a base-spec out-of-scope reference must still be rejected; got: %v", optIn, errs)
		}
	}
}
