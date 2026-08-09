// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for E3 task 2: a let: block requires the EXPR extension. This is the
// one let rule that fires on the base-spec path -- see validateLetExtension's
// doc comment in validate.go for why it cannot live in exprcheck.go alongside
// every other let rule.

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestValidate_LetRequiresEXPR(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template string
		pointer  string
	}{
		{
			name: "step template",
			template: `specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
- name: Step1
  let: ["a = 1"]
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`,
			pointer: "/steps/0/let",
		},
		{
			name: "step script",
			template: `specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
- name: Step1
  script:
    let: ["a = 1"]
    actions:
      onRun:
        command: echo
        args: ["hi"]
`,
			pointer: "/steps/0/script/let",
		},
		{
			name: "job environment script",
			template: `specificationVersion: jobtemplate-2023-09
name: TestJob
jobEnvironments:
- name: Env1
  script:
    let: ["a = 1"]
    actions:
      onEnter:
        command: echo
        args: ["hi"]
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`,
			pointer: "/jobEnvironments/0/script/let",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := openjd.Parse([]byte(tc.template), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			// openjd.Validate returns the concrete ValidationErrors type, not
			// error -- kept as its own variable (rather than reusing the
			// error-typed err above) so the empty-slice/nil check below is a
			// real len() check, not a typed-nil-in-interface comparison that
			// staticcheck (SA4023) correctly flags as always false.
			verrs := openjd.Validate(tmpl)
			if len(verrs) == 0 {
				t.Fatal("Validate = nil, want an error: let: requires the EXPR extension")
			}
			if !strings.Contains(verrs.Error(), tc.pointer) {
				t.Errorf("error %q does not name pointer %q", verrs, tc.pointer)
			}
			if !strings.Contains(verrs.Error(), "EXPR") {
				t.Errorf("error %q does not mention the EXPR extension", verrs)
			}
		})
	}
}

func TestValidate_LetAcceptedWithEXPR(t *testing.T) {
	// EXPR is registered-but-unsupported, so the ONLY error must be the status
	// gate -- never a "let requires EXPR" complaint.
	y := `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  let: ["a = 1"]
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`
	tmpl, err := openjd.Parse([]byte(y), openjd.FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := openjd.Validate(tmpl); err != nil && strings.Contains(err.Error(), "let") {
		t.Errorf("Validate complained about let: %v", err)
	}
}
