// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/test/conformance"
)

const exprFixture = `specificationVersion: jobtemplate-2023-09
extensions:
- EXPR
name: TestJob
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: python
        args:
        - "-c"
        - "print(r'{{ Param.X + 3 }}')"
        - "{{ '--flag' if true else null }}"
`

func TestExtractExpressions(t *testing.T) {
	got, err := conformance.ExtractExpressions([]byte(exprFixture))
	if err != nil {
		t.Fatalf("ExtractExpressions: %v", err)
	}
	want := []string{"Param.X + 3", "'--flag' if true else null"}
	if len(got) != len(want) {
		t.Fatalf("got %d expressions %q; want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("expression %d = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestExtractExpressions_Shapes(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{"none", "name: TestJob\n", nil},
		{"two in one scalar", `a: "{{ X }}-{{ Y }}"`, []string{"X", "Y"}},
		{"whitespace is trimmed", `a: "{{   X   }}"`, []string{"X"}},
		{"no inner whitespace", `a: "{{X}}"`, []string{"X"}},
		{"nested sequences and mappings", "a:\n  - b: \"{{ X }}\"\n  - \"{{ Y }}\"", []string{"X", "Y"}},
		{"newline inside a body", "a: \"{{ X +\\n1 }}\"", []string{"X +\n1"}},
		{"braces inside the body", `a: "{{ {1, 2} }}"`, []string{"{1, 2}"}},
		{"unclosed is still reported", `a: "{{ X"`, []string{"X"}},
		{"literal closing braces outside a reference", `a: "}} tail"`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := conformance.ExtractExpressions([]byte(tt.doc))
			if err != nil {
				t.Fatalf("ExtractExpressions: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %q; want %q", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got %q; want %q", got, tt.want)
				}
			}
		})
	}
}

func TestExtractExpressions_MalformedYAML(t *testing.T) {
	if _, err := conformance.ExtractExpressions([]byte("a: [1,\n  b: 2\n")); err == nil {
		t.Error("ExtractExpressions on malformed YAML = nil error; want an error")
	}
}

func TestRunExprCase(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		doc         string
		wantAccept  bool
		wantPass    bool
		reasonMatch string
	}{
		{
			name:       "valid fixture whose expressions parse",
			path:       "EXPR/job_templates/expr1.1--ok.yaml",
			doc:        `a: "{{ Param.X + 3 }}"`,
			wantAccept: true,
			wantPass:   true,
		},
		{
			name:       "invalid fixture whose expression does not parse",
			path:       "EXPR/job_templates/expr1.1--syntax-error.invalid.yaml",
			doc:        `a: "{{ 1 + }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			name:        "valid fixture whose expression does not parse is a failure",
			path:        "EXPR/job_templates/expr1.1--list.yaml",
			doc:         `a: "{{ [1, 2] }}"`,
			wantAccept:  false,
			wantPass:    false,
			reasonMatch: "list expressions are not supported",
		},
		{
			name:        "invalid fixture whose expressions all parse is a failure",
			path:        "EXPR/job_templates/expr1.1--type-error.invalid.yaml",
			doc:         `a: "{{ Param.Name + 5 }}"`,
			wantAccept:  true,
			wantPass:    false,
			reasonMatch: "marked .invalid",
		},
		{
			name:       "fixture with no expressions at all is accepted",
			path:       "EXPR/job_templates/expr-extension-enabled.yaml",
			doc:        "name: TestJob\n",
			wantAccept: true,
			wantPass:   true,
		},
		{
			name:       "malformed yaml counts as a rejection",
			path:       "EXPR/job_templates/broken.invalid.yaml",
			doc:        "a: [1,\n  b: 2\n",
			wantAccept: false,
			wantPass:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := conformance.ParseTestCase(tt.path)
			res := conformance.RunExprCase(tc, []byte(tt.doc))
			if res.State != conformance.StateLive {
				t.Errorf("State = %v; want StateLive — this path always scores", res.State)
			}
			if res.Accepted != tt.wantAccept {
				t.Errorf("Accepted = %v; want %v (reason: %s)", res.Accepted, tt.wantAccept, res.Reason)
			}
			if res.Passed != tt.wantPass {
				t.Errorf("Passed = %v; want %v (reason: %s)", res.Passed, tt.wantPass, res.Reason)
			}
			if tt.reasonMatch != "" && !strings.Contains(res.Reason, tt.reasonMatch) {
				t.Errorf("Reason = %q; want it to contain %q", res.Reason, tt.reasonMatch)
			}
			if tt.wantPass && res.Reason != "" {
				t.Errorf("Reason = %q; want it empty on a pass", res.Reason)
			}
		})
	}
}
