// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

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
