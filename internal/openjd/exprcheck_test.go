// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"
)

func TestSymbolsFor_ScopeGatesTheFixedSymbols(t *testing.T) {
	tmpl := &JobTemplate{Name: "T"}
	tests := []struct {
		scope   Scope
		present []string
		absent  []string
	}{
		{ScopeJob, nil, []string{"Job.Name", "Step.Name", "Session.WorkingDirectory"}},
		{
			ScopeJobEnvironment,
			[]string{"Job.Name", "Session.WorkingDirectory", "Session.HasPathMappingRules"},
			[]string{"Step.Name"},
		},
		{ScopeStepEnvironment, []string{"Job.Name", "Step.Name"}, nil},
		{ScopeStepScript, []string{"Job.Name", "Step.Name"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.scope.String(), func(t *testing.T) {
			syms := symbolsFor(tmpl, nil, nil, tc.scope, nil)
			for _, name := range tc.present {
				if _, ok := syms[name]; !ok {
					t.Errorf("%s: %s missing", tc.scope, name)
				}
			}
			for _, name := range tc.absent {
				if _, ok := syms[name]; ok {
					t.Errorf("%s: %s present but must not be", tc.scope, name)
				}
			}
		})
	}
}

func TestSymbolsFor_JobParameterTypes(t *testing.T) {
	tmpl := &JobTemplate{
		Name: "T",
		ParameterDefinitions: []JobParameter{
			{Name: "S", Type: "STRING"},
			{Name: "N", Type: "INT"},
			{Name: "P", Type: "PATH"},
		},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeJob, nil)
	tests := []struct{ name, want string }{
		{"Param.S", "unresolved[string]"},
		{"Param.N", "unresolved[int]"},
		// Section 1.2.2: a PATH parameter is `path` via Param and `string` via
		// RawParam -- the raw value may be a path for another OS.
		{"Param.P", "unresolved[path]"},
		{"RawParam.P", "unresolved[string]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := syms[tc.name]
			if !ok {
				t.Fatalf("%s missing", tc.name)
			}
			if got := v.Type.String(); got != tc.want {
				t.Errorf("%s typed %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestSymbolsFor_ConcreteParamsAtPhaseTwo pins the ONLY difference between the
// two evaluation phases: the symbol table, not the code path.
func TestSymbolsFor_ConcreteParamsAtPhaseTwo(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "N", Type: "INT"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeJob, map[string]string{"N": "42"})
	v, ok := syms["Param.N"]
	if !ok {
		t.Fatal("Param.N missing")
	}
	if v.IsUnresolved() {
		t.Fatal("Param.N is still unresolved with a concrete value supplied")
	}
	if got := v.String(); got != "42" {
		t.Errorf("Param.N = %s, want 42", got)
	}
}

// TestSymbolsFor_FamilyMembersScopedCorrectly builds a template with real
// Task.Param., Task.File. and Env.File. members and checks each lands only in
// the scopes that expose its family -- not merely that fixed symbols do,
// which is all the other tests here check. It also pins a real regression:
// Env.File. must come from the ENVIRONMENT's own script, not from the step's
// task script. Before the fix, ScopeStepEnvironment (which exposes Env.File.
// but not Task.File.) fabricated "Env.File.TaskScript" from step's script,
// while the environment's own "EnvScript" file was never bound at all.
func TestSymbolsFor_FamilyMembersScopedCorrectly(t *testing.T) {
	tmpl := &JobTemplate{Name: "T"}
	step := &StepTemplate{
		Name: "Step1",
		Script: &StepScript{
			EmbeddedFiles: []EmbeddedFile{{Name: "TaskScript", Type: EmbeddedFileTypeText}},
		},
		ParameterSpace: &StepParameterSpace{
			TaskParameterDefinitions: []TaskParamDefinition{{Name: "Frame", Type: TaskParamTypeInt}},
		},
	}
	env := &Environment{
		Name: "Env1",
		Script: &EnvironmentScript{
			EmbeddedFiles: []EmbeddedFile{{Name: "EnvScript", Type: EmbeddedFileTypeText}},
		},
	}

	tests := []struct {
		scope   Scope
		present []string
		absent  []string
	}{
		{
			ScopeJob, nil,
			[]string{"Task.Param.Frame", "Task.File.TaskScript", "Env.File.EnvScript", "Env.File.TaskScript"},
		},
		{
			ScopeJobEnvironment,
			[]string{"Env.File.EnvScript"},
			[]string{"Task.Param.Frame", "Task.File.TaskScript", "Env.File.TaskScript"},
		},
		{
			ScopeStepEnvironment,
			[]string{"Env.File.EnvScript"},
			[]string{"Task.Param.Frame", "Task.File.TaskScript", "Env.File.TaskScript"},
		},
		{
			ScopeStepScript,
			[]string{"Task.Param.Frame", "Task.File.TaskScript"},
			[]string{"Env.File.EnvScript", "Env.File.TaskScript"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.scope.String(), func(t *testing.T) {
			syms := symbolsFor(tmpl, step, env, tc.scope, nil)
			for _, name := range tc.present {
				if _, ok := syms[name]; !ok {
					t.Errorf("%s: %s missing", tc.scope, name)
				}
			}
			for _, name := range tc.absent {
				if _, ok := syms[name]; ok {
					t.Errorf("%s: %s present but must not be", tc.scope, name)
				}
			}
		})
	}
}

func TestCheckFormatString(t *testing.T) {
	tmpl := &JobTemplate{
		Name: "T",
		ParameterDefinitions: []JobParameter{
			{Name: "N", Type: "INT"},
			{Name: "S", Type: "STRING"},
		},
	}
	tests := []struct {
		name    string
		src     string
		scope   Scope
		wantErr bool
		wantSub string
	}{
		{"a literal is always fine", "hello", ScopeJob, false, ""},
		{"arithmetic on an int parameter", "{{ Param.N + 1 }}", ScopeJob, false, ""},
		{"embedded reference converts to string", "n is {{ Param.N }}", ScopeJob, false, ""},
		{"unknown symbol", "{{ Param.Missing }}", ScopeJob, true, "Param.Missing"},
		{"a step symbol is out of scope in a job position", "{{ Step.Name }}", ScopeJob, true, "Step.Name"},
		{"a session symbol is out of scope in a job position", "{{ Session.WorkingDirectory }}", ScopeJob, true, "Session"},
		{"a step symbol is in scope in a step script", "{{ Step.Name }}", ScopeStepScript, false, ""},
		{"a type error is caught with everything unresolved", "{{ Param.N.upper() }}", ScopeJob, true, "upper"},
		{"a syntax error", "{{ Param.N + }}", ScopeJob, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			syms := symbolsFor(tmpl, nil, nil, tc.scope, nil)
			errs := checkFormatString(tc.src, "/p", tc.scope, syms, TargetString)
			if tc.wantErr && len(errs) == 0 {
				t.Fatalf("checkFormatString(%q) accepted it; want a rejection", tc.src)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Fatalf("checkFormatString(%q) rejected it: %v", tc.src, errs)
			}
			if tc.wantSub != "" {
				if len(errs) == 0 {
					t.Fatalf("checkFormatString(%q): no errors, but wanted message containing %q", tc.src, tc.wantSub)
				}
				if !strings.Contains(errs[0].Message, tc.wantSub) {
					t.Errorf("message %q does not mention %q", errs[0].Message, tc.wantSub)
				}
			}
		})
	}
}

// TestCheckFormatString_LoneRefInheritsTheTarget pins section 1.3.2's
// transparency rule: "{{expr}}" alone takes the field's type, while the same
// expression with surrounding text is converted to a string and so is always
// acceptable.
//
// The brief's own example used a STRING parameter against an int target, but
// section 1.2.3 lists "string -> int" as a legal non-destructive coercion
// (deferred to runtime: "3" succeeds, "3.75" fails, checked only once a
// concrete value exists) -- so a STRING placeholder against TargetInt
// type-checks CLEAN at this unresolved, no-params-supplied phase, and cannot
// demonstrate a rejection. Confirmed directly: expr.Eval("Param.S",
// MapSymbols{"Param.S": Unresolved(TString)}, TInt) returns
// unresolved[int], nil. A PATH parameter has no such rule in either
// direction (scalarCoercible's "to == CodeInt" case admits only float and
// string; coercibleConditional's "from == CodePath" case admits only a
// target that includes string) -- see internal/openjd/expr/coerce.go -- so it
// is used here instead, preserving the test's intent unchanged.
func TestCheckFormatString_LoneRefInheritsTheTarget(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "S", Type: "PATH"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeJob, nil)

	if errs := checkFormatString("{{ Param.S }}", "/p", ScopeJob, syms, TargetInt); len(errs) == 0 {
		t.Error("a path parameter was accepted for an int field; the lone reference " +
			"must inherit the field's target type")
	}
	if errs := checkFormatString("x{{ Param.S }}", "/p", ScopeJob, syms, TargetInt); len(errs) != 0 {
		t.Errorf("an EMBEDDED reference must be converted to a string and accepted: %v", errs)
	}
}

// TestCheckFormatString_HostOnlyFunctionPlacement pins that a host-context-only
// function is rejected in a submission-time scope and accepted in a host one.
// apply_path_mapping is registered FLAT in the leaf -- it has no scope model --
// so this gate is the only thing enforcing section "Host-Context Function
// Availability".
func TestCheckFormatString_HostOnlyFunctionPlacement(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "P", Type: "PATH"}},
	}
	const src = "{{ apply_path_mapping(RawParam.P) }}"

	syms := symbolsFor(tmpl, nil, nil, ScopeJob, nil)
	errs := checkFormatString(src, "/name", ScopeJob, syms, TargetString)
	if len(errs) == 0 {
		t.Fatal("apply_path_mapping was accepted in a submission-time scope")
	}
	if !strings.Contains(errs[0].Message, "apply_path_mapping") {
		t.Errorf("message %q does not name the function", errs[0].Message)
	}

	syms = symbolsFor(tmpl, nil, nil, ScopeStepScript, nil)
	if errs := checkFormatString(src, "/p", ScopeStepScript, syms, TargetString); len(errs) != 0 {
		t.Errorf("apply_path_mapping must be allowed in a host context: %v", errs)
	}
}
