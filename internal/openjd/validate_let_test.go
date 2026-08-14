// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for E3 task 2: a let: block requires the EXPR extension. This is the
// one let rule that fires on the base-spec path -- see validateLetExtension's
// doc comment in validate.go for why it cannot live in exprcheck.go alongside
// every other let rule.

import (
	"fmt"
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

// TestValidate_LetStructuralConstraints covers E3 task 4: Template Schemas
// 3.6's element-count bounds on a let: block -- "If defined, then there must
// be at least one element in this list" and "Maximum number of items: 50."
// The 50/51 boundary rows are the point of this test: a cap that only checked
// 1 and 100 would pass with an off-by-one error.
func TestValidate_LetStructuralConstraints(t *testing.T) {
	body := func(inject string) string {
		return fmt.Sprintf(`specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
%s  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`, inject)
	}
	many := func(n int) string {
		var b strings.Builder
		b.WriteString("  let:\n")
		for i := range n {
			fmt.Fprintf(&b, "  - a%d = 1\n", i)
		}
		return b.String()
	}

	for _, tc := range []struct {
		name    string
		inject  string
		wantErr string
	}{
		{"absent is fine", "", ""},
		{"empty list rejected", "  let: []\n", "at least one"},
		{"one is fine", many(1), ""},
		{"fifty is fine", many(50), ""},
		{"fifty-one rejected", many(51), "50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := openjd.Parse([]byte(body(tc.inject)), openjd.FormatYAML)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			// openjd.Validate returns the concrete ValidationErrors type, not
			// error -- kept as its own variable (rather than reusing the
			// error-typed err above) so the empty check below is a real
			// len() check, not a typed-nil-in-interface comparison that
			// staticcheck (SA4023) correctly flags as always true (see
			// TestValidate_LetRequiresEXPR above for the same fix).
			verrs := openjd.Validate(tmpl)
			got := ""
			if len(verrs) > 0 {
				got = verrs.Error()
			}
			if tc.wantErr == "" {
				if strings.Contains(got, "let") {
					t.Errorf("Validate complained about let: %v", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantErr) {
				t.Errorf("Validate = %q, want it to contain %q", got, tc.wantErr)
			}
		})
	}
}
