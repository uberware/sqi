// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
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
		// RawParam: the raw value may be a path for another OS.
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

// TestSymbolsFor_ConcreteParamsAtPhaseTwo pins that the two evaluation phases
// differ only in the symbol table: with values supplied, Param.* is concrete.
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

// TestSymbolsFor_FloatParamPreservesSubmittedText pins section 1.3.4: a FLOAT
// job parameter submitted as "3.500" keeps that exact text rather than the
// canonical "3.5" strconv.FormatFloat would produce. concreteJobParamValue
// binds it via expr.FloatText, which stores it in Value's fs field
// (value.go); String() reports fs when it is set, so the test observes the
// text without template substitution. It asserts only the binding at
// phase 2; rendering and substitution are tested elsewhere.
func TestSymbolsFor_FloatParamPreservesSubmittedText(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "Scale", Type: "FLOAT"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeJob, map[string]string{"Scale": "3.500"})

	param, ok := syms["Param.Scale"]
	if !ok {
		t.Fatal("Param.Scale missing")
	}
	if param.IsUnresolved() {
		t.Fatal("Param.Scale is still unresolved with a concrete value supplied")
	}
	if got := param.String(); got != "3.500" {
		t.Errorf("Param.Scale.String() = %q, want %q (submitted text)", got, "3.500")
	}

	raw, ok := syms["RawParam.Scale"]
	if !ok {
		t.Fatal("RawParam.Scale missing")
	}
	if got := raw.String(); got != "3.500" {
		t.Errorf("RawParam.Scale.String() = %q, want %q (submitted text)", got, "3.500")
	}

	// The stored text affects only String(), not equality or arithmetic: a
	// canonically formatted float with the same number compares equal and
	// computes identically, per the fs field's invariants (value.go).
	v, err := expr.Eval("Param.Scale + 0", syms, expr.TFloat)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got := v.String(); got != "3.5" {
		t.Errorf("Param.Scale + 0 = %s, want 3.5 (the carry must not propagate through arithmetic)", got)
	}
}

// TestSymbolsFor_FamilyMembersScopedCorrectly builds a template with real
// Task.Param., Task.File. and Env.File. members and checks each lands only in
// the scopes that expose its family; the other tests here check only fixed
// symbols. It also pins that Env.File. comes from the environment's own
// script, not from the step's task script:
// ScopeStepEnvironment (which exposes Env.File. but not Task.File.) must not
// bind "Env.File.TaskScript" from the step's script, and must bind the
// environment's own "EnvScript" file.
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
			errs := checkFormatString(nil, tc.src, "/p", tc.scope, syms, TargetString)
			if tc.wantErr && len(errs) == 0 {
				t.Fatalf("checkFormatString(nil, %q) accepted it; want a rejection", tc.src)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Fatalf("checkFormatString(nil, %q) rejected it: %v", tc.src, errs)
			}
			if tc.wantSub != "" {
				if len(errs) == 0 {
					t.Fatalf("checkFormatString(nil, %q): no errors, but wanted message containing %q", tc.src, tc.wantSub)
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
// A STRING parameter against an int target cannot demonstrate a rejection:
// section 1.2.3 lists "string -> int" as a legal non-destructive coercion
// (checked once a concrete value exists: "3" succeeds, "3.75" fails), so a
// STRING placeholder against TargetInt type-checks clean when no parameter
// values are supplied: expr.Eval("Param.S", MapSymbols{"Param.S":
// Unresolved(TString)}, TInt) returns unresolved[int], nil. A PATH parameter
// has no such rule in either direction (in internal/openjd/expr/coerce.go,
// scalarCoercible's "to == CodeInt" case admits only float and string, and
// coercibleConditional's "from == CodePath" case admits only a target that
// includes string), so the test uses one.
func TestCheckFormatString_LoneRefInheritsTheTarget(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "S", Type: "PATH"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeJob, nil)

	if errs := checkFormatString(nil, "{{ Param.S }}", "/p", ScopeJob, syms, TargetInt); len(errs) == 0 {
		t.Error("a path parameter was accepted for an int field; the lone reference " +
			"must inherit the field's target type")
	}
	if errs := checkFormatString(nil, "x{{ Param.S }}", "/p", ScopeJob, syms, TargetInt); len(errs) != 0 {
		t.Errorf("an EMBEDDED reference must be converted to a string and accepted: %v", errs)
	}
}

// TestCheckFormatString_HostOnlyFunctionPlacement pins that a host-context-only
// function is rejected in a submission-time scope and accepted in a host one.
// The expr package registers apply_path_mapping with no scope restriction, so
// this check is what enforces the specification's "Host-Context Function
// Availability" section.
func TestCheckFormatString_HostOnlyFunctionPlacement(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "P", Type: "PATH"}},
	}
	const src = "{{ apply_path_mapping(RawParam.P) }}"

	syms := symbolsFor(tmpl, nil, nil, ScopeJob, nil)
	errs := checkFormatString(nil, src, "/name", ScopeJob, syms, TargetString)
	if len(errs) == 0 {
		t.Fatal("apply_path_mapping was accepted in a submission-time scope")
	}
	if !strings.Contains(errs[0].Message, "apply_path_mapping") {
		t.Errorf("message %q does not name the function", errs[0].Message)
	}

	syms = symbolsFor(tmpl, nil, nil, ScopeStepScript, nil)
	if errs := checkFormatString(nil, src, "/p", ScopeStepScript, syms, TargetString); len(errs) != 0 {
		t.Errorf("apply_path_mapping must be allowed in a host context: %v", errs)
	}
}

// TestCheckFormatString_HostOnlyFunctionPlacement_EmbeddedAndMethodForm
// covers the shapes TestCheckFormatString_HostOnlyFunctionPlacement does not
// (it exercises only a lone reference calling apply_path_mapping as a plain
// function): checkHostOnlyFunctions also fires when the call sits inside an
// embedded segment (surrounded by other text) and when it is written in
// method-call syntax (RawParam.P.apply_path_mapping()).
// checkHostOnlyFunctions walks Expression.CalledFunctions(), which collects a
// call's function name whether it was written as a free call or a method
// call, and runs before Eval. The method form is therefore rejected for the
// host-context reason, not for lacking a "(path)" method overload of
// apply_path_mapping (which internal/openjd/expr/doc.go says it does not
// have).
func TestCheckFormatString_HostOnlyFunctionPlacement_EmbeddedAndMethodForm(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "P", Type: "PATH"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeJob, nil)

	t.Run("embedded segment", func(t *testing.T) {
		const src = "prefix {{ apply_path_mapping(RawParam.P) }} suffix"
		errs := checkFormatString(nil, src, "/p", ScopeJob, syms, TargetString)
		if len(errs) == 0 {
			t.Fatal("apply_path_mapping was accepted in a submission-time scope")
		}
		if !strings.Contains(errs[0].Message, "apply_path_mapping") {
			t.Errorf("message %q does not name the function", errs[0].Message)
		}
	})

	t.Run("method-call form", func(t *testing.T) {
		const src = "{{ RawParam.P.apply_path_mapping() }}"
		errs := checkFormatString(nil, src, "/p", ScopeJob, syms, TargetString)
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
// list[string] flattens inline, while a list[list[string]] (nested one level
// too deep) does not type-check. checkActionExpressions (exprcheck.go) is
// TargetArgItem's caller in the template walk; this test pins the contract
// at the checkFormatString level, which substitution (performed elsewhere)
// relies on.
//
// Both the concrete-literal shapes and their unresolved equivalents are
// covered, because a concrete literal is not what a real template produces
// at this position. checkTemplateExpressions runs phase 1 with params == nil,
// so every Param./RawParam. reference is an unresolved placeholder
// (symbolsFor), and section 1.3.2's example ("a value that is a string is one
// argument, None means no argument is added, and a list[string] adds each
// element") is written as a conditional picking between those shapes based
// on another parameter, e.g. "{{ Param.S if Param.Flag else None }}". That
// conditional's result type is the union of its branches (here string? =
// string | nulltype), because Param.Flag has no phase-1 value to pick a
// branch with. Concrete 'x'/None/['a','b'] alone would not catch a checker
// that rejects this unresolved-union shape (a null member of a source union
// coerced to a target union that names nulltype); see the CodeNull branch in
// internal/openjd/expr/coerce.go's coercible().
func TestCheckFormatString_ArgItemShapes(t *testing.T) {
	tmpl := &JobTemplate{
		Name: "T",
		ParameterDefinitions: []JobParameter{
			{Name: "Flag", Type: "BOOL"},
			{Name: "S", Type: "STRING"},
		},
	}
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

		// The same three accepted shapes, unresolved as phase 1 produces
		// them, via a conditional whose condition (Param.Flag) has no
		// concrete value. Each branch's type is a concrete member (string,
		// nulltype, or list[string]); the expression's result type is a union
		// because evaluation cannot pick a branch without a value for
		// Param.Flag.
		{"an unresolved string (a Param reference) is one argument", "{{ Param.S }}", false},
		{"an unresolved list[string] flattens inline", "{{ [Param.S] }}", false},
		{
			"a conditional between a string and None is the canonical " +
				"section 1.3.2 shape, and must type-check unresolved",
			"{{ Param.S if Param.Flag else None }}", false,
		},
		{
			"a conditional between a list[string] and None must also " +
				"type-check unresolved",
			"{{ ['--x', Param.S] if Param.Flag else None }}", false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkFormatString(nil, tc.src, "/p", ScopeStepScript, syms, TargetArgItem)
			if tc.wantErr && len(errs) == 0 {
				t.Fatalf("checkFormatString(nil, %q) accepted it; want a rejection", tc.src)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Fatalf("checkFormatString(nil, %q) rejected it: %v", tc.src, errs)
			}
		})
	}
}

// TestCheckFormatString_TimeoutTarget pins the target-type contract
// checkActionExpressions applies at the "action timeout" position (TargetInt).
//
// It calls checkFormatString directly rather than going through
// checkTemplateExpressions and a YAML round trip, because decodeAction
// (parse.go) decodes "timeout" as a strict integer for every template, EXPR
// or not: scalarToInt has no case for a "{{ ... }}" string, so parsing fails
// with "timeout must be an integer" before ValidateWithOptions runs. No
// Action field can carry an unresolved format string at this position, so
// checkActionExpressions' strconv.Itoa(a.TimeoutSeconds) call is wired but
// has no effect on a real template (see checkActionExpressions' doc
// comment). This test pins the contract that call applies.
func TestCheckFormatString_TimeoutTarget(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		ParameterDefinitions: []JobParameter{{Name: "S", Type: "STRING"}},
	}
	syms := symbolsFor(tmpl, nil, nil, ScopeStepScript, nil)

	if errs := checkFormatString(nil, "{{ [Param.S] }}", "/p", ScopeStepScript, syms, TargetInt); len(errs) == 0 {
		t.Error("a list value was accepted against an int timeout target")
	}
	// A decoded timeout is a plain decimal string with no "{{" reference,
	// and is accepted.
	if errs := checkFormatString(nil, "30", "/p", ScopeStepScript, syms, TargetInt); len(errs) != 0 {
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
// values" position (ScopeJob, TargetString). A Session.* reference is out of
// scope at ScopeJob (scope.go's scopeFixed(ScopeJob) returns none), so it is
// rejected at the amount's min pointer.
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
// entries" position (ScopeJob, TargetString). A Session.* reference in a
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

// TestCheckTemplateExpressions_RangeExprOutOfScope pins the whole-field
// RangeExpr form of the range position (TestCheckTemplateExpressions_RangeEntries
// covers the RangeList array form): an out-of-scope reference in RangeExpr is
// rejected whatever target checkParameterSpaceExpressions checks it against
// (rangeExprFieldType(TaskParamTypeString)). The target affects only the
// result type check; an unknown-symbol failure happens at symbol lookup,
// before any target coercion runs.
func TestCheckTemplateExpressions_RangeExprOutOfScope(t *testing.T) {
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
					RangeExpr: func() *string { s := "{{ Session.WorkingDirectory }}"; return &s }(),
				}},
			},
		}},
	}

	errs := checkTemplateExpressions(tmpl, nil)
	const wantPtr = "/steps/0/parameterSpace/taskParameterDefinitions/0/range"
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

// TestCheckTemplateExpressions_RangeExprListValuedAccepted pins that
// checkParameterSpaceExpressions accepts section 1.3.11's list-valued
// RangeExpr fixtures (expr1.3.11--*-range-expression.yaml). It checks this one
// against rangeExprFieldType(TaskParamTypeFloat) (list[float]), which the
// expression's result type matches directly.
// TestCheckParameterSpaceExpressions_RangeTargetTypes is the fuller table,
// covering every declared type and both the accept and reject side at both
// the whole-field and per-entry positions.
func TestCheckTemplateExpressions_RangeExprListValuedAccepted(t *testing.T) {
	tmpl := &JobTemplate{
		Name:                 "T",
		Extensions:           []string{"EXPR"},
		ParameterDefinitions: []JobParameter{{Name: "Scale", Type: "FLOAT"}},
		Steps: []StepTemplate{{
			Name:   "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{Command: "echo"}}},
			ParameterSpace: &StepParameterSpace{
				TaskParameterDefinitions: []TaskParamDefinition{{
					Name: "Factor",
					Type: TaskParamTypeFloat,
					RangeExpr: func() *string {
						s := "{{ [Param.Scale * 2, Param.Scale + 0.5] }}"
						return &s
					}(),
				}},
			},
		}},
	}
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Fatalf("a list-valued RangeExpr must type-check against list[float]: %v", errs)
	}
}

// TestCheckParameterSpaceExpressions_RangeTargetTypes is the per-type range
// target table, exercised end to end through checkParameterSpaceExpressions: for
// each declared task-parameter type, an expression of the position's own
// target type is accepted, and one of the wrong shape is rejected at the
// position's own JSON pointer, at both the whole-field RangeExpr position and
// a RangeList entry, since the two positions have different targets
// (rangeExprFieldType: a list, or a range_expr for INT/CHUNK[INT];
// rangeListItemType: §1.3.2's list item, T? | list[T]).
//
// The shape-mismatch reject case at each position is built from the same
// expression as the accept case, wrapped or unwrapped in a list literal: a
// whole-field RangeExpr (list/range_expr target) rejects the entry's
// accepted scalar, and a RangeList entry rejects the whole field's accepted
// list wrapped in one more list. The whole-field list itself is legal at an
// entry, where it flattens (§1.3.12).
//
// entryReject is a second reject case at the entry position only, present
// for every type except STRING: a value of the wrong scalar type (not only
// the wrong shape) that TargetString, which accepts every scalar, would
// accept. The value is rejected only if rangeExprElemType is threaded through
// the entry position: TargetString accepts a bool or a non-integral float as readily
// as a string. STRING has no entryReject because rangeExprElemType(STRING)
// is TargetString.
//
// wholeReject is entryReject's whole-field counterpart, present for every
// type except STRING: a list whose elements are the wrong scalar type. A bare
// scalar is already covered by "whole-field rejected (wrong shape)" and is
// rejected by every list-shaped target regardless of element type, so
// without this row a rangeExprFieldType that returned
// expr.ListOf(expr.TString) unconditionally would pass this test and the
// rest of the package. wholeAccept's list literals ({{ [1,2,3] }},
// {{ [1.0,2.5] }}, {{ [path('/a'),path('/b')] }}) all coerce to list[string]
// as readily as to their own element type (section 1.2.3's scalar->string
// coercion applies elementwise), so only wholeReject checks the element
// type. STRING has none: list[string] is STRING's own target, so no
// wrong-element-type list exists for it.
//
// Neither reject expression references an unbound symbol, so a failure here
// comes from target-type coercion, not from the scope check that
// TestCheckTemplateExpressions_RangeEntries/_RangeExprOutOfScope cover.
func TestCheckParameterSpaceExpressions_RangeTargetTypes(t *testing.T) {
	tests := []struct {
		name        string
		typ         TaskParamType
		wholeAccept string
		wholeReject string // "" when there is no wrong-element-type list to build (STRING)
		// wholeShapeReject is a bare scalar the whole-field target rejects.
		// It defaults to entryAccept, because for FLOAT/STRING/PATH the field
		// target is a plain list and every bare scalar is the wrong shape.
		// INT/CHUNK[INT] name their own: section 1.3.12's RangeString note
		// makes an int and a string legal at that position (they are range
		// text; see rangeExprFieldType), so those rows need a scalar that is
		// neither.
		//
		// A bool does not work: under openjd-specifications#175's rules a
		// bool's destination is string and the target offers string, so a
		// bool becomes the range text "true", which is not a valid
		// <IntRangeExpr> and is rejected at expansion, as "{{ 'abc' }}" is
		// (resolve substitutes the text without parsing it, and
		// ExpandParameterSpace is the only layer that reads it; see
		// TestExpandParameterSpace_NonRangeTextIsRejected). The checker
		// checks the type at this position, not the text.
		//
		// null works because no conversion produces null, so nulltype is
		// never a destination, and a target that does not name nulltype
		// cannot receive one.
		wholeShapeReject string
		entryAccept      string
		entryReject      string // "" when there is no type-mismatch (as opposed to shape-mismatch) case
	}{
		{
			name: "INT", typ: TaskParamTypeInt,
			wholeAccept: "{{ [1, 2, 3] }}", entryAccept: "{{ 5 }}",
			wholeShapeReject: "{{ null }}",
			// A list of strings: string->int coercion needs a value that
			// parses as an int, and "a" does not, so this is rejected on the
			// value (list[string] -> list[int] is coercible at the type
			// level, per scalarCoercible's string->int rule). An
			// expr.ListOf(expr.TString) target would accept it with no
			// conversion, so the row still tells the two targets apart.
			wholeReject: "{{ ['a'] }}",
			// 2.5 is not integral: TargetString would render it "2.5"
			// unchanged, while TInt narrows float to int and rejects a
			// non-exact value (the INT/Scale=2.5 case
			// TestResolveParameterSpaceParams_RangeListEntryINTNonIntegralRejected
			// covers, here as a literal rather than a job-parameter
			// reference).
			entryReject: "{{ 2.5 }}",
		},
		{
			name: "CHUNK[INT]", typ: TaskParamTypeChunkInt,
			wholeAccept: "{{ [1, 2, 3] }}", entryAccept: "{{ 5 }}",
			wholeShapeReject: "{{ null }}",
			wholeReject:      "{{ ['a'] }}",
			entryReject:      "{{ 2.5 }}",
		},
		{
			name: "FLOAT", typ: TaskParamTypeFloat,
			wholeAccept: "{{ [1.0, 2.5] }}", entryAccept: "{{ 2.5 }}",
			// "abc" does not parse as a float, so it is rejected on the
			// value, as expand.go's validateFloatList would reject it later,
			// here at the field's own pointer.
			wholeReject: "{{ ['abc'] }}",
			// A bool coerces to string unconditionally (section 1.2.3) but
			// has no bool->float rule.
			entryReject: "{{ true }}",
		},
		{
			name: "STRING", typ: TaskParamTypeString,
			wholeAccept: "{{ ['a', 'b'] }}", entryAccept: "{{ 'a' }}",
		},
		{
			name: "PATH", typ: TaskParamTypePath,
			wholeAccept: "{{ [path('/a'), path('/b')] }}", entryAccept: "{{ path('/a') }}",
			// An int coerces to string unconditionally but only a string
			// coerces to path (section 1.2.3: path <- string only), so
			// list[path] rejects a list[int] on its type, unlike the
			// value-based INT/FLOAT rows above.
			wholeReject: "{{ [1, 2] }}",
			// An int coerces to string unconditionally but only a string
			// coerces to path (section 1.2.3: path <- string only).
			entryReject: "{{ 5 }}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("whole-field accepted", func(t *testing.T) {
				errs := checkRangeField(tc.typ, tc.wholeAccept)
				if len(errs) != 0 {
					t.Fatalf("%s range %q: want accepted, got %v", tc.typ, tc.wholeAccept, errs)
				}
			})
			t.Run("whole-field rejected (wrong shape)", func(t *testing.T) {
				// A bare scalar that is not one of the shapes section 1.3.12
				// admits at this position. For FLOAT/STRING/PATH that is any
				// scalar, so the one the entry position accepts is used;
				// INT/CHUNK[INT] name their own, because an
				// int and a string are legal range text there.
				body := tc.wholeShapeReject
				if body == "" {
					body = tc.entryAccept
				}
				errs := checkRangeField(tc.typ, body)
				wantPtr := "/steps/0/parameterSpace/taskParameterDefinitions/0/range"
				assertRejectedAt(t, errs, wantPtr, body)
			})
			if tc.wholeReject != "" {
				t.Run("whole-field rejected (wrong element type)", func(t *testing.T) {
					errs := checkRangeField(tc.typ, tc.wholeReject)
					wantPtr := "/steps/0/parameterSpace/taskParameterDefinitions/0/range"
					assertRejectedAt(t, errs, wantPtr, tc.wholeReject)
				})
			}
			t.Run("entry accepted", func(t *testing.T) {
				errs := checkRangeEntry(tc.typ, tc.entryAccept)
				if len(errs) != 0 {
					t.Fatalf("%s entry %q: want accepted, got %v", tc.typ, tc.entryAccept, errs)
				}
			})
			t.Run("entry accepted (a list flattens)", func(t *testing.T) {
				if errs := checkRangeEntry(tc.typ, tc.wholeAccept); len(errs) != 0 {
					t.Fatalf("%s entry %q: want accepted, got %v", tc.typ, tc.wholeAccept, errs)
				}
			})
			t.Run("entry rejected (wrong shape)", func(t *testing.T) {
				// A list of lists: a list item flattens one level, not two.
				inner := strings.TrimSuffix(strings.TrimPrefix(tc.wholeAccept, "{{ "), " }}")
				body := "{{ [" + inner + "] }}"
				errs := checkRangeEntry(tc.typ, body)
				wantPtr := "/steps/0/parameterSpace/taskParameterDefinitions/0/range/0"
				assertRejectedAt(t, errs, wantPtr, body)
			})
			if tc.entryReject != "" {
				t.Run("entry rejected (wrong scalar type)", func(t *testing.T) {
					errs := checkRangeEntry(tc.typ, tc.entryReject)
					wantPtr := "/steps/0/parameterSpace/taskParameterDefinitions/0/range/0"
					assertRejectedAt(t, errs, wantPtr, tc.entryReject)
				})
			}
		})
	}
}

// checkRangeField runs checkTemplateExpressions over a single-step template
// whose one task-parameter definition (type typ) carries body as its
// whole-field RangeExpr.
func checkRangeField(typ TaskParamType, body string) ValidationErrors {
	tmpl := rangeCheckTemplate(typ, nil, &body)
	return checkTemplateExpressions(tmpl, nil)
}

// checkRangeEntry is checkRangeField's sibling for a single RangeList entry.
func checkRangeEntry(typ TaskParamType, body string) ValidationErrors {
	tmpl := rangeCheckTemplate(typ, []string{body}, nil)
	return checkTemplateExpressions(tmpl, nil)
}

func rangeCheckTemplate(typ TaskParamType, rangeList []string, rangeExpr *string) *JobTemplate {
	return &JobTemplate{
		Name:       "T",
		Extensions: []string{"EXPR"},
		Steps: []StepTemplate{{
			Name:   "Step1",
			Script: &StepScript{Actions: StepActions{OnRun: Action{Command: "echo"}}},
			ParameterSpace: &StepParameterSpace{
				TaskParameterDefinitions: []TaskParamDefinition{{
					Name:      "P",
					Type:      typ,
					RangeList: rangeList,
					RangeExpr: rangeExpr,
				}},
			},
		}},
	}
}

// assertRejectedAt fails t unless errs contains a ValidationError at exactly
// wantPtr, the position's own pointer; the rejection cases are what catch a
// target wired too loosely.
func assertRejectedAt(t *testing.T, errs ValidationErrors, wantPtr, body string) {
	t.Helper()
	if len(errs) == 0 {
		t.Fatalf("%q: want a rejection at %s, got none", body, wantPtr)
	}
	for _, e := range errs {
		if e.Pointer == wantPtr {
			return
		}
	}
	t.Fatalf("%q: want a rejection at %s, got %v", body, wantPtr, errs)
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
// applies [ExprLimits.evalOptions] (a much tighter budget than expr.Eval's own
// execution-time defaults) through the template walk, as well as when
// checkFormatString is called directly with no opts.
//
// Each budget is pinned with a value that exceeds one limit only, so neither
// sub-test can pass for the wrong reason. A value such as 'a' * 3_000_000
// exceeds both defaultSubmissionMemoryBytes and defaultSubmissionOperations,
// and for a string repeat the operation charge (callShape's chargeResult,
// ceil(len/256) per section 1.3.10 rule 3) runs before the memory charge
// (evalNode's ec.m.alloc, per section 1.3.9), so the memory check would never
// be reached and swapped Options would not be detected.
//
//   - "over memory, under operations": 'a' * 1,500,000 costs
//     ceil(1,500,000/256) = 5,860 operations (under
//     defaultSubmissionOperations's 10,000) but allocates a 1,500,000-byte
//     string (over defaultSubmissionMemoryBytes's 1,000,000), and stays under
//     limits.go's separate maxStringBytes cap (10,000,000), so that cap does
//     not reject it either.
//   - "over operations, under memory": a comprehension iterating range(11,000)
//     with a filter that is always false (`x > 999999`) costs one operation
//     per iteration plus the call itself, 11,001 operations, over
//     defaultSubmissionOperations, while the result list stays empty, so live
//     memory stays far below defaultSubmissionMemoryBytes.
func TestCheckTemplateExpressions_AppliesSubmissionLimits(t *testing.T) {
	newTmpl := func(arg string) *JobTemplate {
		return &JobTemplate{
			Name:       "T",
			Extensions: []string{"EXPR"},
			Steps: []StepTemplate{{
				Name: "Step1",
				Script: &StepScript{Actions: StepActions{OnRun: Action{
					Command: "echo",
					Args:    []string{arg},
					ArgsSet: true,
				}}},
			}},
		}
	}

	t.Run("over memory limit, under operation limit", func(t *testing.T) {
		errs := checkTemplateExpressions(newTmpl("{{ 'a' * 1500000 }}"), nil)
		if len(errs) == 0 {
			t.Fatal("a 1,500,000-byte literal repeat was accepted; defaultSubmissionMemoryBytes must reject it")
		}
		if !strings.Contains(errs[0].Message, "memory limit exceeded") {
			t.Errorf("want a memory-limit error (defaultSubmissionMemoryBytes); got: %v", errs[0].Message)
		}
	})

	t.Run("over operation limit, under memory limit", func(t *testing.T) {
		errs := checkTemplateExpressions(
			newTmpl("{{ len([x for x in range(11000) if x > 999999]) }}"), nil,
		)
		if len(errs) == 0 {
			t.Fatal("an 11,001-operation, empty-result comprehension was accepted; " +
				"defaultSubmissionOperations must reject it")
		}
		if !strings.Contains(errs[0].Message, "operation limit exceeded") {
			t.Errorf("want an operation-limit error (defaultSubmissionOperations); got: %v", errs[0].Message)
		}
	})
}
