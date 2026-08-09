// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"reflect"
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
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

// TestScopeStepScriptExcludesEnvFile pins the rule scopeFamilies' own
// ScopeStepScript case already encodes structurally (Task 5's refactor
// deleted the old stepScriptRefs literal, whose comment used to state this
// in words: "Env.File is NOT among them -- an environment's attachments
// belong to the environment"; scope.go now expresses it only by which
// symbolFamily entries each scope's case appends): an environment's
// attachments belong to the environment, not to a step's script.
func TestScopeStepScriptExcludesEnvFile(t *testing.T) {
	for _, f := range scopeFamilies(ScopeStepScript) {
		if f.Prefix == "Env.File." {
			t.Fatal("step script scope exposes Env.File.; an environment's attachments " +
				"belong to the environment, not to a step's script")
		}
	}
}

// TestScopeFixed_PerScopeMembership pins, per scope, exactly which fixed
// symbols scopeFixed exposes -- both presence AND absence, and each one's
// type. Absence is the load-bearing half: it is what makes Step.Name illegal
// in a job environment while legal in a step environment, which is the entire
// reason the four-scope split exists. derivedPrefixes cannot assert this,
// because every fixed symbol is EXPR-only and so all four are filtered out of
// it by design (TestDerivedPrefixes_EnvironmentScopesAreIdenticalToBaseSpec).
// A membership-only check would also pass with every type wrong, so each
// expected symbol's type is checked with expr.Type.Equal too.
func TestScopeFixed_PerScopeMembership(t *testing.T) {
	tests := []struct {
		scope Scope
		want  map[string]expr.Type
	}{
		{ScopeJob, map[string]expr.Type{}},
		{ScopeJobEnvironment, map[string]expr.Type{
			"Job.Name":                     expr.TString,
			"Session.WorkingDirectory":     expr.TPath,
			"Session.PathMappingRulesFile": expr.TPath,
			"Session.HasPathMappingRules":  expr.TBool,
		}},
		{ScopeStepEnvironment, map[string]expr.Type{
			"Job.Name":                     expr.TString,
			"Session.WorkingDirectory":     expr.TPath,
			"Session.PathMappingRulesFile": expr.TPath,
			"Session.HasPathMappingRules":  expr.TBool,
			"Step.Name":                    expr.TString,
		}},
		{ScopeStepScript, map[string]expr.Type{
			"Job.Name":                     expr.TString,
			"Session.WorkingDirectory":     expr.TPath,
			"Session.PathMappingRulesFile": expr.TPath,
			"Session.HasPathMappingRules":  expr.TBool,
			"Step.Name":                    expr.TString,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.scope.String(), func(t *testing.T) {
			got := scopeFixed(tc.scope)

			gotNames := make(map[string]expr.Type, len(got))
			for _, sym := range got {
				if _, dup := gotNames[sym.Name]; dup {
					t.Fatalf("scopeFixed(%s) declares %q more than once", tc.scope, sym.Name)
				}
				gotNames[sym.Name] = sym.Type
			}

			for name, wantType := range tc.want {
				gotType, ok := gotNames[name]
				if !ok {
					t.Errorf("scopeFixed(%s) is missing %q, want present with type %s",
						tc.scope, name, wantType)
					continue
				}
				if !gotType.Equal(wantType) {
					t.Errorf("scopeFixed(%s) has %q with type %s, want %s",
						tc.scope, name, gotType, wantType)
				}
			}
			for name := range gotNames {
				if _, want := tc.want[name]; !want {
					t.Errorf("scopeFixed(%s) unexpectedly exposes %q", tc.scope, name)
				}
			}
		})
	}
}

// TestScopeFixed_AllEXPROnly guards the invariant derivedPrefixes' correctness
// depends on: every fixed symbol must be EXPROnly, because derivedPrefixes
// only excludes EXPR-only entries from the base-spec prefix list. If a future
// fixed symbol were added without the flag, derivedPrefixes would silently
// gain a prefix and a base-spec template would start accepting a reference it
// used to reject, with nothing to catch it. Asserted as an invariant over
// scopeFixed for every scope, so a new entry is covered automatically rather
// than needing its own line here.
func TestScopeFixed_AllEXPROnly(t *testing.T) {
	for _, s := range []Scope{ScopeJob, ScopeJobEnvironment, ScopeStepEnvironment, ScopeStepScript} {
		for _, sym := range scopeFixed(s) {
			if !sym.EXPROnly {
				t.Errorf("scopeFixed(%s): %q is not EXPROnly; every fixed symbol must be, "+
					"or derivedPrefixes would silently expose it to base-spec templates", s, sym.Name)
			}
		}
	}
}
