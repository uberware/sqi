// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"reflect"
	"testing"
)

// TestDerivedPrefixes_MatchTheShippedLiterals is the safety net for making the
// scope declaration the single source of truth. The base-spec path keeps
// prefix-matching and keeps its error text; only the SOURCE of the prefixes
// moves. If a derived list ever differs from the literal it replaced, a
// template that validates today could start failing, so this pins all three
// byte for byte INCLUDING ORDER -- validateFormatString joins them into the
// "allowed: ..." message a user reads.
func TestDerivedPrefixes_MatchTheShippedLiterals(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		want  []string
	}{
		{"job", ScopeJob, []string{"Param.", "RawParam."}},
		{
			"job environment", ScopeJobEnvironment,
			[]string{"Param.", "RawParam.", "Env.File.", "Session."},
		},
		{
			"step environment", ScopeStepEnvironment,
			[]string{"Param.", "RawParam.", "Env.File.", "Session."},
		},
		{
			"step script", ScopeStepScript,
			[]string{"Param.", "RawParam.", "Task.Param.", "Task.RawParam.", "Task.File.", "Session."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := derivedPrefixes(tc.scope); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("derivedPrefixes(%s) = %q, want %q", tc.scope, got, tc.want)
			}
		})
	}
}

// TestDerivedPrefixes_EnvironmentScopesAreIdenticalToBaseSpec states the
// property that makes the four-scope split safe: the only symbol separating a
// job environment from a step environment is Step.Name, which is EXPR-only, so
// the two derive to the same base-spec list and no base-spec template can tell
// them apart.
func TestDerivedPrefixes_EnvironmentScopesAreIdenticalToBaseSpec(t *testing.T) {
	job := derivedPrefixes(ScopeJobEnvironment)
	step := derivedPrefixes(ScopeStepEnvironment)
	if !reflect.DeepEqual(job, step) {
		t.Errorf("job env %q and step env %q derive differently; base-spec templates "+
			"would now be treated differently depending on where the environment sits", job, step)
	}
}

// TestScopeHostContext pins which scopes may call a host-context-only function.
// Submission-time positions -- the job name, host requirements, the parameter
// space -- are not host contexts; environments and step scripts are.
func TestScopeHostContext(t *testing.T) {
	tests := []struct {
		scope Scope
		want  bool
	}{
		{ScopeJob, false},
		{ScopeJobEnvironment, true},
		{ScopeStepEnvironment, true},
		{ScopeStepScript, true},
	}
	for _, tc := range tests {
		t.Run(tc.scope.String(), func(t *testing.T) {
			if got := tc.scope.IsHostContext(); got != tc.want {
				t.Errorf("%s.IsHostContext() = %v, want %v", tc.scope, got, tc.want)
			}
		})
	}
}

// TestScopeStepScriptExcludesEnvFile pins the rule stepScriptRefs' own comment
// already states: an environment's attachments belong to the environment.
func TestScopeStepScriptExcludesEnvFile(t *testing.T) {
	for _, f := range scopeFamilies(ScopeStepScript) {
		if f.Prefix == "Env.File." {
			t.Fatal("step script scope exposes Env.File.; an environment's attachments " +
				"belong to the environment, not to a step's script")
		}
	}
}
