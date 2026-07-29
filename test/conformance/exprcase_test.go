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
		{"empty body yields an empty string", `a: "{{ }}"`, []string{""}},
		{"whitespace-only body yields an empty string", `a: "{{   }}"`, []string{""}},
		{
			"alias resolves through its anchor's definition, not again at the alias site",
			"a: &anchor \"{{ X }}\"\nb: *anchor\n",
			[]string{"X"},
		},
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
			name: "valid fixture whose expressions parse",
			path: "EXPR/job_templates/expr1.1--ok.yaml",
			// Param.X must be declared for RunExprCase to evaluate it now that
			// every declared symbol is bound (Task 16 retires the Names() gate).
			doc:        "parameterDefinitions:\n  - name: X\n    type: INT\na: \"{{ Param.X + 3 }}\"",
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
			name: "invalid fixture whose expressions all parse is a failure",
			path: "EXPR/job_templates/expr1.1--type-error.invalid.yaml",
			// Param.Name is declared INT (not STRING) so this case exercises
			// "parses and type-checks cleanly, but still fails because the
			// fixture is marked .invalid" rather than a type error, which is a
			// different case covered by TestRunExprCase_TypeChecksAgainstDeclaredTypes.
			doc:         "parameterDefinitions:\n  - name: Name\n    type: INT\na: \"{{ Param.Name + 5 }}\"",
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
		{
			name:        "empty expression body reaches expr.Parse and is rejected",
			path:        "EXPR/job_templates/expr1.1--empty-body.yaml",
			doc:         `a: "{{ }}"`,
			wantAccept:  false,
			wantPass:    false,
			reasonMatch: "empty expression",
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

func TestRunExprCase_EvaluatesLiteralOnlyExpressions(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		doc         string
		wantAccept  bool
		wantPass    bool
		reasonMatch string
	}{
		{
			name:       "arithmetic that evaluates cleanly is accepted",
			path:       "EXPR/job_templates/expr2.1.1--ok.yaml",
			doc:        `a: "{{ 1 + 2 }}"`,
			wantAccept: true,
			wantPass:   true,
		},
		{
			name:       "division by zero is rejected",
			path:       "EXPR/job_templates/expr2.1.1--division-by-zero.invalid.yaml",
			doc:        `a: "{{ 1 / 0 }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			name:       "int64 overflow on addition is rejected",
			path:       "EXPR/job_templates/expr2.1.1--int64-overflow-add.invalid.yaml",
			doc:        `a: "{{ 9223372036854775807 + 1 }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			name:       "int64 overflow on a large power is rejected without looping",
			path:       "EXPR/job_templates/expr2.1.1--int64-overflow-pow-large.invalid.yaml",
			doc:        `a: "{{ 2 ** 1000000 }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			// A symbolic expression is now evaluated against its declared type
			// (Task 16 retires the Names() gate), so Frame must be declared for
			// this case to still exercise "a symbol reference is fine, not just
			// syntax".
			name:       "an expression referencing a declared symbol evaluates cleanly",
			path:       "EXPR/job_templates/expr2.1.1--symbolic.yaml",
			doc:        "parameterDefinitions:\n  - name: Frame\n    type: INT\na: \"{{ Param.Frame + 1 }}\"",
			wantAccept: true,
			wantPass:   true,
		},
		{
			name:       "a symbolic expression that cannot parse is still rejected",
			path:       "EXPR/job_templates/expr1.1--bad.invalid.yaml",
			doc:        `a: "{{ Param.Frame + }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			name:        "a valid fixture whose literal arithmetic fails is a failure",
			path:        "EXPR/job_templates/expr2.1.1--surprise.yaml",
			doc:         `a: "{{ 1 / 0 }}"`,
			wantAccept:  false,
			wantPass:    false,
			reasonMatch: "division by zero",
		},
		{
			// The loop must keep checking later expressions after one evaluates
			// cleanly, not stop early: a declared symbol that type-checks fine
			// followed by a failing literal expression must still catch the
			// later failure. The fixture is marked valid (not .invalid) so a
			// wrongly-accepted result fails the test with the reason still
			// visible — Reason is cleared on a pass (see the assertion below),
			// so a correctly rejected .invalid fixture couldn't carry the
			// reasonMatch check this case needs.
			name:        "a clean symbolic expression followed by a failing literal one is rejected",
			path:        "EXPR/job_templates/expr2.1.1--mixed-symbolic-then-failure.yaml",
			doc:         "parameterDefinitions:\n  - name: Frame\n    type: INT\na: \"{{ Param.Frame }}-{{ 1 / 0 }}\"",
			wantAccept:  false,
			wantPass:    false,
			reasonMatch: "division by zero",
		},
		{
			// Same as above with the order reversed, so the assertion does
			// not depend on which expression in the document comes first.
			name:        "a failing literal expression followed by a symbolic one is rejected",
			path:        "EXPR/job_templates/expr2.1.1--mixed-failure-then-symbolic.yaml",
			doc:         `a: "{{ 1 / 0 }}-{{ Param.Frame }}"`,
			wantAccept:  false,
			wantPass:    false,
			reasonMatch: "division by zero",
		},
		{
			// A declared symbolic expression alongside a literal one that
			// evaluates cleanly: both must be checked, and both must accept, for
			// the whole fixture to accept.
			name:       "a declared symbolic expression and a clean literal expression are both accepted",
			path:       "EXPR/job_templates/expr2.1.1--mixed-symbolic-and-clean.yaml",
			doc:        "parameterDefinitions:\n  - name: Frame\n    type: INT\na: \"{{ Param.Frame }}-{{ 1 + 2 }}\"",
			wantAccept: true,
			wantPass:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := conformance.ParseTestCase(tt.path)
			res := conformance.RunExprCase(tc, []byte(tt.doc))
			if res.State != conformance.StateLive {
				t.Errorf("State = %v; want StateLive", res.State)
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

func TestDeclaredSymbols_CoversEverySection122Family(t *testing.T) {
	// This is the guard against over-rejection, which is this task's main risk.
	// A family missing from the table turns a VALID fixture into a false
	// failure, so every family section 1.2.2 defines is asserted present.
	doc := []byte(`
specificationVersion: jobtemplate-2023-09
name: Test
parameterDefinitions:
  - name: Frame
    type: INT
  - name: Scene
    type: PATH
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Chunk
          type: CHUNK[INT]
    script:
      embeddedFiles:
        - name: Bake
          type: TEXT
          data: "x"
      actions:
        onRun:
          command: echo
`)
	syms, err := conformance.DeclaredSymbols(doc)
	if err != nil {
		t.Fatalf("DeclaredSymbols: %v", err)
	}
	want := map[string]string{
		// Job parameters, both forms. PATH differs between them.
		"Param.Frame":    "unresolved[int]",
		"RawParam.Frame": "unresolved[int]",
		"Param.Scene":    "unresolved[path]",
		"RawParam.Scene": "unresolved[string]",
		// Task parameters. CHUNK[INT] is a range_expr, not a list.
		"Task.Param.Chunk":    "unresolved[range_expr]",
		"Task.RawParam.Chunk": "unresolved[range_expr]",
		// Embedded files.
		"Task.File.Bake": "unresolved[path]",
		"Env.File.Bake":  "unresolved[path]",
		// The fixed symbols.
		"Job.Name":                     "unresolved[string]",
		"Step.Name":                    "unresolved[string]",
		"Session.WorkingDirectory":     "unresolved[path]",
		"Session.PathMappingRulesFile": "unresolved[path]",
		"Session.HasPathMappingRules":  "unresolved[bool]",
	}
	for name, wantType := range want {
		v, ok := syms.Lookup(name)
		if !ok {
			t.Errorf("%s is not bound — a valid fixture using it would be falsely rejected", name)
			continue
		}
		if got := v.Type.String(); got != wantType {
			t.Errorf("%s has type %s; want %s", name, got, wantType)
		}
	}
}

func TestDeclaredSymbols_JobParameterTypes(t *testing.T) {
	// Every row of section 1.2.2's job-parameter table.
	tests := []struct {
		declared     string
		wantParam    string
		wantRawParam string
	}{
		{"STRING", "unresolved[string]", "unresolved[string]"},
		{"INT", "unresolved[int]", "unresolved[int]"},
		{"FLOAT", "unresolved[float]", "unresolved[float]"},
		{"PATH", "unresolved[path]", "unresolved[string]"},
		{"BOOL", "unresolved[bool]", "unresolved[bool]"},
		{"RANGE_EXPR", "unresolved[range_expr]", "unresolved[range_expr]"},
		{"LIST[STRING]", "unresolved[list[string]]", "unresolved[list[string]]"},
		{"LIST[INT]", "unresolved[list[int]]", "unresolved[list[int]]"},
		{"LIST[FLOAT]", "unresolved[list[float]]", "unresolved[list[float]]"},
		{"LIST[PATH]", "unresolved[list[path]]", "unresolved[list[string]]"},
		{"LIST[BOOL]", "unresolved[list[bool]]", "unresolved[list[bool]]"},
		{"LIST[LIST[INT]]", "unresolved[list[list[int]]]", "unresolved[list[list[int]]]"},
	}
	for _, tt := range tests {
		t.Run(tt.declared, func(t *testing.T) {
			doc := []byte("parameterDefinitions:\n  - name: P\n    type: " + tt.declared + "\n")
			syms, err := conformance.DeclaredSymbols(doc)
			if err != nil {
				t.Fatalf("DeclaredSymbols: %v", err)
			}
			p, ok := syms.Lookup("Param.P")
			if !ok {
				t.Fatal("Param.P is not bound")
			}
			if got := p.Type.String(); got != tt.wantParam {
				t.Errorf("Param.P = %s; want %s", got, tt.wantParam)
			}
			raw, ok := syms.Lookup("RawParam.P")
			if !ok {
				t.Fatal("RawParam.P is not bound")
			}
			if got := raw.Type.String(); got != tt.wantRawParam {
				t.Errorf("RawParam.P = %s; want %s", got, tt.wantRawParam)
			}
		})
	}
}

func TestRunExprCase_TypeChecksAgainstDeclaredTypes(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		doc        string
		wantAccept bool
		wantPass   bool
	}{
		{
			name: "arithmetic on a declared int is accepted",
			path: "EXPR/job_templates/expr1.1--ok.yaml",
			doc: `parameterDefinitions:
  - name: Frame
    type: INT
a: "{{ Param.Frame + 1 }}"`,
			wantAccept: true,
			wantPass:   true,
		},
		{
			name: "adding an int to a declared string is rejected",
			path: "EXPR/job_templates/expr1.1--type-error.invalid.yaml",
			doc: `parameterDefinitions:
  - name: Name
    type: STRING
a: "{{ Param.Name + 5 }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			name: "an undeclared parameter is rejected",
			path: "EXPR/job_templates/expr1.1--unknown-variable.invalid.yaml",
			doc: `parameterDefinitions:
  - name: Frame
    type: INT
a: "{{ Param.DoesNotExist }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			name: "comparing a declared string to an int is rejected",
			path: "EXPR/job_templates/expr1.1--bad-compare.invalid.yaml",
			doc: `parameterDefinitions:
  - name: Name
    type: STRING
a: "{{ Param.Name < 5 }}"`,
			wantAccept: false,
			wantPass:   true,
		},
		{
			name: "promotion across int and float is accepted",
			path: "EXPR/job_templates/expr1.2.3--promote.yaml",
			doc: `parameterDefinitions:
  - name: Frame
    type: INT
  - name: Scale
    type: FLOAT
a: "{{ Param.Frame * Param.Scale }}"`,
			wantAccept: true,
			wantPass:   true,
		},
		{
			name:       "a fixed session symbol is accepted",
			path:       "EXPR/job_templates/expr1.2.2--session.yaml",
			doc:        `a: "{{ Session.HasPathMappingRules }}"`,
			wantAccept: true,
			wantPass:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := conformance.ParseTestCase(tt.path)
			res := conformance.RunExprCase(tc, []byte(tt.doc))
			if res.Accepted != tt.wantAccept {
				t.Errorf("Accepted = %v; want %v (reason: %s)", res.Accepted, tt.wantAccept, res.Reason)
			}
			if res.Passed != tt.wantPass {
				t.Errorf("Passed = %v; want %v (reason: %s)", res.Passed, tt.wantPass, res.Reason)
			}
		})
	}
}

func TestDeclaredSymbols_LetBindings(t *testing.T) {
	// "let:" (Template Schemas section 3.6.2) is outside section 1.2.2 and
	// outside real scoping, which belongs to sub-project E. Binding a "let"
	// name UNTYPED is only meant to stop it being falsely rejected as an
	// unknown symbol — it must still be bound, from any "let:" block anywhere
	// in the document, regardless of where the name is referenced.
	doc := []byte(`
specificationVersion: jobtemplate-2023-09
name: Test
steps:
  - name: Render
    let:
    - job_tag = Job.Name
    script:
      let:
      - script_tag = Job.Name + ':' + Step.Name
      - malformed
      - " = empty-name-is-skipped"
      actions:
        onRun:
          command: echo
`)
	syms, err := conformance.DeclaredSymbols(doc)
	if err != nil {
		t.Fatalf("DeclaredSymbols: %v", err)
	}
	for _, name := range []string{"job_tag", "script_tag"} {
		v, ok := syms.Lookup(name)
		if !ok {
			t.Errorf("%s is not bound — a valid fixture using it would be falsely rejected", name)
			continue
		}
		if got := v.Type.String(); got != "unresolved" {
			t.Errorf("%s has type %s; want unresolved (untyped — real \"let\" typing is sub-project E's)", name, got)
		}
	}
	if _, ok := syms.Lookup("malformed"); ok {
		t.Error(`"malformed" (no "=") must not be bound`)
	}
	if _, ok := syms.Lookup(""); ok {
		t.Error(`an empty name (from " = empty-name-is-skipped") must not be bound`)
	}
}

func TestRunExprCase_LetBoundNameIsAccepted(t *testing.T) {
	// The regression this guards against: a symbol introduced by "let:" must
	// not be rejected as unknown just because it is outside section 1.2.2.
	tc := conformance.ParseTestCase("EXPR/job_templates/7.3.1--job-step-name-in-step-let.yaml")
	doc := `steps:
- name: Render
  script:
    let:
    - script_tag = Job.Name + ':' + Step.Name
    actions:
      onRun:
        command: echo
    embeddedFiles:
    - name: summary
      type: TEXT
      data: "{{ script_tag }}"`
	res := conformance.RunExprCase(tc, []byte(doc))
	if !res.Accepted {
		t.Errorf("Accepted = false; want true (reason: %s)", res.Reason)
	}
	if !res.Passed {
		t.Errorf("Passed = false; want true (reason: %s)", res.Reason)
	}
}
