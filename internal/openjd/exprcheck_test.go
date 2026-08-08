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

// TestCheckFormatString_HostOnlyFunctionPlacement_EmbeddedAndMethodForm closes
// a coverage gap the committed suite left open: TestCheckFormatString_
// HostOnlyFunctionPlacement above only exercises a LONE reference calling
// apply_path_mapping as a plain function. A reviewer confirmed with
// throwaway tests that checkHostOnlyFunctions also fires when the call sits
// inside an embedded segment (surrounded by other text) and when it is
// written in method-call syntax (RawParam.P.apply_path_mapping()) -- neither
// shape was pinned in the repo. checkHostOnlyFunctions walks
// Expression.CalledFunctions(), which collects a call's function name
// regardless of whether it was written as a free call or a method call, and
// runs BEFORE Eval -- so the method form is rejected for the host-context
// reason here, not for lacking a "(path)" overload of apply_path_mapping as
// a method (which internal/openjd/expr/doc.go notes it does not have).
func TestCheckFormatString_HostOnlyFunctionPlacement_EmbeddedAndMethodForm(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "P", Type: "PATH"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeJob, nil)

	t.Run("embedded segment", func(t *testing.T) {
		const src = "prefix {{ apply_path_mapping(RawParam.P) }} suffix"
		errs := checkFormatString(src, "/p", ScopeJob, syms, TargetString)
		if len(errs) == 0 {
			t.Fatal("apply_path_mapping was accepted in a submission-time scope")
		}
		if !strings.Contains(errs[0].Message, "apply_path_mapping") {
			t.Errorf("message %q does not name the function", errs[0].Message)
		}
	})

	t.Run("method-call form", func(t *testing.T) {
		const src = "{{ RawParam.P.apply_path_mapping() }}"
		errs := checkFormatString(src, "/p", ScopeJob, syms, TargetString)
		if len(errs) == 0 {
			t.Fatal("apply_path_mapping() written as a method call was accepted in a submission-time scope")
		}
		if !strings.Contains(errs[0].Message, "apply_path_mapping") {
			t.Errorf("message %q does not name the function", errs[0].Message)
		}
	})
}

// TestCheckFormatString_ArgItemShapes pins section 1.3.2's list-item rule for
// TargetArgItem, the union expr.UnionOf(expr.OptionalOf(expr.TString),
// expr.ListOf(expr.TString)): a string is one argument, None drops it, and a
// list[string] flattens inline -- but a list[list[string]] (nesting one level
// too deep) does not type-check. checkActionExpressions (exprcheck.go) is
// TargetArgItem's first real caller in the template walk; this test pins the
// contract at the checkFormatString level so a later sub-project (E4, which
// performs the substitution TargetArgItem only type-checks here) inherits a
// pinned contract rather than a prose description.
func TestCheckFormatString_ArgItemShapes(t *testing.T) {
	tmpl := &JobTemplate{Name: "T"}
	syms := symbolsFor(tmpl, nil, nil, ScopeStepScript, nil)

	tests := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{"a string is one argument", "{{ 'x' }}", false},
		{"None drops the argument", "{{ None }}", false},
		{"a list[string] flattens inline", "{{ ['a', 'b'] }}", false},
		{"a list[list[string]] does not type-check", "{{ [['a'], ['b']] }}", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkFormatString(tc.src, "/p", ScopeStepScript, syms, TargetArgItem)
			if tc.wantErr && len(errs) == 0 {
				t.Fatalf("checkFormatString(%q) accepted it; want a rejection", tc.src)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Fatalf("checkFormatString(%q) rejected it: %v", tc.src, errs)
			}
		})
	}
}

// TestCheckFormatString_TimeoutTarget pins the target-type contract
// checkActionExpressions applies at the "action timeout" position (TargetInt).
//
// This exercises checkFormatString directly rather than through
// checkTemplateExpressions and a full YAML round trip, because decodeAction
// (parse.go) decodes "timeout" as a STRICT integer for every template, EXPR
// or not -- scalarToInt has no case for a "{{ ... }}" string, so it errors
// "timeout must be an integer" at PARSE time before ValidateWithOptions ever
// runs. There is therefore no field on Action that can carry an unresolved
// format-string body at this position today; checkActionExpressions'
// strconv.Itoa(a.TimeoutSeconds) call is wired for the position but is
// necessarily a no-op against a real template until that decoder changes,
// which is a separate gap this task does not close (see
// checkActionExpressions' doc comment). This test pins the contract that
// call applies, independent of whether decodeAction can reach it yet.
func TestCheckFormatString_TimeoutTarget(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "S", Type: "STRING"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeStepScript, nil)

	if errs := checkFormatString("{{ [Param.S] }}", "/p", ScopeStepScript, syms, TargetInt); len(errs) == 0 {
		t.Error("a list value was accepted against an int timeout target")
	}
	// What a real, already-decoded timeout looks like today: a plain decimal
	// string with no "{{" reference. Must stay accepted.
	if errs := checkFormatString("30", "/p", ScopeStepScript, syms, TargetInt); len(errs) != 0 {
		t.Errorf("a plain decoded timeout value must not be rejected: %v", errs)
	}
}

// ─── checkTemplateExpressions ───────────────────────────────────────────────

// TestCheckTemplateExpressions_NoOpWithoutEXPR pins that checkTemplateExpressions
// does nothing for a template that does not declare the EXPR extension. The
// job name below would be rejected by checkFormatString (it is not a bare
// dotted identifier) if the walk ran unconditionally; validate.go's base-spec
// path covers this template instead.
func TestCheckTemplateExpressions_NoOpWithoutEXPR(t *testing.T) {
	tmpl := &JobTemplate{
		Name: "{{ this is not a dotted identifier }}",
		Steps: []StepTemplate{{
			Name:   "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{Command: "echo"}}},
		}},
	}
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Fatalf("checkTemplateExpressions must no-op for a template that does not declare EXPR; got: %v", errs)
	}
	if errs := checkTemplateExpressions(nil, nil); len(errs) != 0 {
		t.Fatalf("checkTemplateExpressions must no-op for a nil template; got: %v", errs)
	}
}

// TestCheckTemplateExpressions_HostRequirements pins the "host requirement
// values" position (ScopeJob, TargetString) -- one of the two positions that
// had NO format-string scope validation at all before sub-project E2's Task
// 9. A Session.* reference is out of scope at ScopeJob (scope.go's
// scopeFixed(ScopeJob) returns none), so it must be rejected at the amount's
// min pointer.
func TestCheckTemplateExpressions_HostRequirements(t *testing.T) {
	minRef := "{{ Session.WorkingDirectory }}"
	tmpl := &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name:   "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{Command: "echo"}}},
			HostRequirements: &HostRequirements{
				Amounts: []AmountRequirement{{Name: "amount.worker.vcpu", Min: &minRef}},
			},
		}},
	}

	errs := checkTemplateExpressions(tmpl, nil)
	const wantPtr = "/steps/0/hostRequirements/amounts/0/min"
	found := false
	for _, e := range errs {
		if e.Pointer == wantPtr && strings.Contains(e.Message, "Session") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an out-of-scope error at %s mentioning Session; got: %v", wantPtr, errs)
	}
}

// TestCheckTemplateExpressions_RangeEntries pins the "task-parameter range
// entries" position (ScopeJob, TargetString) -- the other position with no
// format-string scope validation before Task 9. A Session.* reference in a
// RangeList entry is out of scope at ScopeJob (task parameters, like host
// requirements, are resolved before any session exists).
func TestCheckTemplateExpressions_RangeEntries(t *testing.T) {
	tmpl := &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name:   "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{Command: "echo"}}},
			ParameterSpace: &StepParameterSpace{
				TaskParameterDefinitions: []TaskParamDefinition{{
					Name:      "Shot",
					Type:      TaskParamTypeString,
					RangeList: []string{"{{ Session.WorkingDirectory }}"},
				}},
			},
		}},
	}

	errs := checkTemplateExpressions(tmpl, nil)
	const wantPtr = "/steps/0/parameterSpace/taskParameterDefinitions/0/range/0"
	found := false
	for _, e := range errs {
		if e.Pointer == wantPtr && strings.Contains(e.Message, "Session") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an out-of-scope error at %s mentioning Session; got: %v", wantPtr, errs)
	}
}

// TestCheckTemplateExpressions_ArgsPositionUsesArgItemTarget pins that the
// walk applies TargetArgItem (not TargetString) at an action's args entries,
// by round-tripping a value shape TargetString would reject but
// TargetArgItem accepts: a list[string], which flattens inline per section
// 1.3.2's list-item rule.
func TestCheckTemplateExpressions_ArgsPositionUsesArgItemTarget(t *testing.T) {
	tmpl := &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{
				Command: "echo",
				Args:    []string{"{{ ['--quality', 'final'] }}"},
				ArgsSet: true,
			}}},
		}},
	}
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Fatalf("a list[string] args entry must type-check against TargetArgItem: %v", errs)
	}
}

// TestCheckTemplateExpressions_AppliesSubmissionLimits pins that the walk
// applies submissionLimits (a much tighter operation budget than expr.Eval's
// own execution-time default of 10,000,000) end to end, not just when
// checkFormatString is called directly with no opts. A single literal
// 'a' * 3_000_000 costs ceil(3_000_000/256) = 11,719 operations to repeat --
// under limits.go's fixed maxStringBytes floor (10,000,000 bytes) so it is
// not rejected by that unrelated hard cap, but over submissionOperationLimit
// (10,000) -- so it must be rejected as an operation-limit error, and
// quickly: this is a single allocation, not a loop, so the test itself stays
// fast regardless of what it is pinning.
func TestCheckTemplateExpressions_AppliesSubmissionLimits(t *testing.T) {
	tmpl := &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name: "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{
				Command: "echo",
				Args:    []string{"{{ 'a' * 3000000 }}"},
				ArgsSet: true,
			}}},
		}},
	}
	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatal("a 3,000,000-character literal repeat was accepted; submissionLimits must reject it")
	}
	if !strings.Contains(errs[0].Message, "operation limit exceeded") {
		t.Errorf("want an operation-limit error (submissionOperationLimit, not limits.go's unrelated "+
			"maxStringBytes floor); got: %v", errs[0].Message)
	}
}
