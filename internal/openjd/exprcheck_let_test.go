// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

func TestCheckLetBindings_SequentialTypePropagation(t *testing.T) {
	syms := expr.MapSymbols{"Param.Count": expr.Unresolved(expr.TInt)}
	errs := checkLetBindings(
		[]string{"x = Param.Count", "y = x + 1", "z = string(y)"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 0 {
		t.Fatalf("checkLetBindings = %v, want no errors", errs)
	}
	for name, want := range map[string]expr.Type{"x": expr.TInt, "y": expr.TInt, "z": expr.TString} {
		v, ok := syms[name]
		if !ok {
			t.Errorf("%q not bound", name)
			continue
		}
		// Param.Count is itself an unresolved placeholder (this test's syms
		// carries no concrete value for it), so every binding derived from it
		// stays unresolved too -- correctly: Eval propagates "value not known
		// yet" through arithmetic and calls exactly as it propagates a known
		// value, per E2's one-code-path model. What this test pins is the
		// NATURAL type each binding carries underneath that placeholder
		// status (3.6.1's "the type of the binding is the natural result type
		// of the expression"), so the comparison unwraps one layer of
		// "unresolved[T]" to T before comparing -- the same unwrap
		// unwrapUnresolved performs inside the expr package, done here by
		// hand since that helper is unexported.
		if !v.IsUnresolved() {
			t.Errorf("%q bound as %v, want an unresolved placeholder (Param.Count carries no concrete value in this test)", name, v.Type)
		}
		got := v.Type
		if got.Code == expr.CodeUnresolved && len(got.Params) == 1 {
			got = got.Params[0]
		}
		if !got.Equal(want) {
			t.Errorf("%q bound as %v, want %v", name, v.Type, want)
		}
	}
}

func TestCheckLetBindings_TypeErrorReportsAtItsOwnPointer(t *testing.T) {
	syms := expr.MapSymbols{"Param.Count": expr.Unresolved(expr.TInt)}
	errs := checkLetBindings(
		[]string{"x = Param.Count", "bad = x + 'hello'", "after = 1"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	if errs[0].Pointer != "/steps/0/let/1" {
		t.Errorf("pointer = %q, want %q", errs[0].Pointer, "/steps/0/let/1")
	}
	if _, ok := syms["bad"]; ok {
		t.Error("a failed binding was inserted into the table; it must not be")
	}
	if _, ok := syms["after"]; !ok {
		t.Error("a later binding was skipped; one bad binding must not stop the block")
	}
}

func TestCheckLetBindings_SelfReferenceIsAnUnknownName(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings([]string{"x = x + 1"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	// The REASON matters: self-reference must fail because the name does not
	// exist yet, which is what makes it fall out of ordering with no special
	// case. A test asserting only "it failed" would pass even if some unrelated
	// rule were doing the work.
	if !strings.Contains(errs[0].Message, "x") {
		t.Errorf("message %q does not name the unresolved symbol", errs[0].Message)
	}
	if _, ok := syms["x"]; ok {
		t.Error("x was bound despite its own binding failing")
	}
}

func TestCheckLetBindings_MalformedBindingReportsAtItsIndex(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings([]string{"a = 1", "Foo = 2"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/let/1" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/let/1", errs)
	}
}

func TestCheckLetBindings_HostOnlyFunctionRejectedInNonHostScope(t *testing.T) {
	syms := expr.MapSymbols{"Param.P": expr.Unresolved(expr.TPath)}
	errs := checkLetBindings(
		[]string{"p = apply_path_mapping(Param.P)"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	if !strings.Contains(errs[0].Message, "host-context") {
		t.Errorf("message %q is not the host-context rejection", errs[0].Message)
	}
}

// TestCheckLetBindings_OverBudgetEvalIsRejectedAtSubmissionLimit pins that
// checkLetBindings' Eval call is metered by submissionLimits(), not by
// expr.Eval's own much looser execution-time defaults (10,000,000
// operations). Without that, an unmetered evaluation on the synchronous
// POST /api/v1/jobs path is the same class of Critical E2's whole-branch
// review already found once (~9 minutes of server CPU per request) --
// nothing else in the repo can catch a dropped submissionLimits() here:
// checkLetBindings has no caller yet, so neither the conformance suite nor
// the oracle differential test ever reaches this code.
//
// The binding below costs ~10,953 operations (a 200-iteration comprehension,
// each iteration building and upper-casing a 900,000-byte string) -- comfortably
// over submissionOperationLimit (10,000) but a small fraction of expr.Eval's
// 10,000,000-operation default, so this asserts specifically on the
// SUBMISSION limit tripping, not merely on some error occurring: a generic
// "contains an error" assertion would still pass at the default budget and
// pin nothing about which limit fired.
func TestCheckLetBindings_OverBudgetEvalIsRejectedAtSubmissionLimit(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings(
		[]string{"a = max([len(('y' * 900000).upper()) for i in range(200)])"},
		"/steps/0/let", ScopeStepTemplate, syms,
	)
	if len(errs) != 1 {
		t.Fatalf("checkLetBindings = %v, want exactly one error", errs)
	}
	if !strings.Contains(errs[0].Message, "limit of 10000") {
		t.Errorf("message %q does not name the submission operation limit (10000)", errs[0].Message)
	}
	if _, ok := syms["a"]; ok {
		t.Error("an over-budget binding was inserted into the table; it must not be")
	}
}

func TestCheckLetBindings_RejectsDuplicateInSameBlock(t *testing.T) {
	syms := expr.MapSymbols{}
	errs := checkLetBindings([]string{"x = 1", "x = 2"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/let/1" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/let/1", errs)
	}
	if !strings.Contains(errs[0].Message, "shadow") {
		t.Errorf("message %q does not say the binding shadows", errs[0].Message)
	}
	v, ok := syms["x"]
	if !ok {
		t.Fatal("x is not bound at all")
	}
	if got := v.AsInt(); got != 1 {
		t.Errorf("x = %v, want the FIRST binding's value 1", got)
	}
}

func TestCheckLetBindings_RejectsShadowingAnEnclosingBlock(t *testing.T) {
	// The enclosing block's names are already in syms -- that is what makes
	// "same block" and "any enclosing scope" one check rather than two.
	syms := expr.MapSymbols{}
	if errs := checkLetBindings([]string{"x = 1"}, "/steps/0/let", ScopeStepTemplate, syms); len(errs) != 0 {
		t.Fatalf("outer block: %v", errs)
	}
	errs := checkLetBindings([]string{"x = 2"}, "/steps/0/script/let", ScopeStepScript, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/script/let/0" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/script/let/0", errs)
	}
}

// TestCheckLetBindings_RejectsShadowingAPreexistingTableEntry replaces the
// task brief's original third test, which seeded syms with the name
// "lower.Thing" to prove the check is keyed off the syms TABLE rather than
// some parallel list of names checkLetBindings itself has bound. That input
// is unreachable through the real grammar: "." is not a legal
// <UserIdentifier> character (letbinding.go's isLetBindingNameCont), so
// parseLetBinding rejects "lower.Thing = 1" for its name before the
// shadowing check this task adds ever runs -- the test would still see a
// non-empty errs slice, but for the wrong reason, and would keep "passing"
// even if the shadowing check were deleted entirely.
//
// This restructures the same intent with a name the grammar actually admits:
// syms is seeded directly (bypassing checkLetBindings, standing in for
// whatever future mechanism -- a wider identifier rule, a scope model with
// its own pre-bound names -- might one day place a non-let-derived entry in
// the table) with a plain lowercase identifier, "thing", that a let binding
// COULD legally produce. Binding over it still proves the check consults
// syms itself rather than a self-maintained set of "names seen so far in
// this call", since "thing" was never seen by this call until the lookup
// that rejects it.
func TestCheckLetBindings_RejectsShadowingAPreexistingTableEntry(t *testing.T) {
	syms := expr.MapSymbols{"thing": expr.Unresolved(expr.TString)}
	errs := checkLetBindings([]string{"thing = 1"}, "/steps/0/let", ScopeStepTemplate, syms)
	if len(errs) != 1 || errs[0].Pointer != "/steps/0/let/0" {
		t.Fatalf("checkLetBindings = %v, want one error at /steps/0/let/0", errs)
	}
	if !strings.Contains(errs[0].Message, "shadow") {
		t.Errorf("message %q does not say the binding shadows", errs[0].Message)
	}
}

// ─── wiring: the three let locations in the template walk ─────────────────

// mustParseEXPR parses yaml and fails the test immediately on error. It does
// NOT call Validate: these tests exercise checkTemplateExpressions directly,
// and Validate would stop at the EXPR extension's status gate (StatusInProgress)
// before checkTemplateExpressions is ever reached.
func mustParseEXPR(t *testing.T, yaml string) *JobTemplate {
	t.Helper()
	tmpl, err := Parse([]byte(yaml), FormatYAML)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	return tmpl
}

func TestCheckTemplateExpressions_StepLetVisibleInItsScript(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: N
  type: INT
  default: 3
steps:
- name: Step1
  let:
  - doubled = Param.N * 2
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ doubled }}"]
`)
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Errorf("checkTemplateExpressions = %v, want no errors", errs)
	}
}

func TestCheckTemplateExpressions_StepLetVisibleInParameterSpaceAndHostRequirements(t *testing.T) {
	// Section 3.6.2 row 1 lists parameterSpace and hostRequirements among the
	// places a step-template binding is visible, even though both positions are
	// evaluated at ScopeJob. Scope and symbol table are separate.
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: N
  type: INT
  default: 4
steps:
- name: Step1
  let:
  - last = Param.N - 1
  parameterSpace:
    taskParameterDefinitions:
    - name: Frame
      type: INT
      range: "0-{{ last }}"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Errorf("checkTemplateExpressions = %v, want no errors", errs)
	}
}

func TestCheckTemplateExpressions_StepLetNotVisibleInAnotherStep(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  let:
  - only_here = 1
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
- name: Step2
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ only_here }}"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatal("a step's let binding leaked into a sibling step")
	}
	if !strings.Contains(errs[0].Pointer, "/steps/1/") {
		t.Errorf("error at %q, want it in step 1", errs[0].Pointer)
	}
}

func TestCheckTemplateExpressions_ScriptLetSeesStepLet(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  let:
  - base = 10
  script:
    let:
    - derived = base * 2
    actions:
      onRun:
        command: echo
        args: ["{{ derived }}"]
`)
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Errorf("checkTemplateExpressions = %v, want no errors", errs)
	}
}

func TestCheckTemplateExpressions_StepTemplateLetCannotSeeTaskSymbols(t *testing.T) {
	// Section 3.6.2 row 1's negative half: a step template's let is evaluated at
	// submission, so Task.Param is not available even though the step declares it.
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  let:
  - bad = Task.Param.Frame
  parameterSpace:
    taskParameterDefinitions:
    - name: Frame
      type: INT
      range: "1-3"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatal("Task.Param was available in a step-template let binding")
	}
	if errs[0].Pointer != "/steps/0/let/0" {
		t.Errorf("error at %q, want /steps/0/let/0", errs[0].Pointer)
	}
}

func TestCheckTemplateExpressions_EnvironmentLetVisibleInItsActions(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: Who
  type: STRING
  default: world
jobEnvironments:
- name: Setup
  script:
    let:
    - greeting = 'hello ' + Param.Who
    actions:
      onEnter:
        command: echo
        args: ["{{ greeting }}"]
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Errorf("checkTemplateExpressions = %v, want no errors", errs)
	}
}

// ─── fix round 1: mutation-pinned leak tests ───────────────────────────────

// TestCheckTemplateExpressions_StepEnvironmentSymbolsDoNotLeakIntoHostRequirements
// pins checkEnvironmentExpressions' baseSyms clone (`maps.Clone(outerLet)`).
// outerLet, for a step's stepEnvironments, IS stepLet -- the SAME map object
// checkStepExpressions later merges into jobSyms for hostRequirements and
// parameterSpace. Without the clone, a step environment's own symbolsFor
// output (here: ScopeStepEnvironment's Step.Name/Session.*/Env.File.) would
// be written directly into that shared map, and checkStepExpressions runs
// checkEnvironmentExpressions for stepEnvironments BEFORE it builds jobSyms
// -- so the pollution would flow forward into hostRequirements, silently
// re-legalizing a symbol section 3.6.2 row 1 and section 7.3.1 both forbid
// there. A bare Step.Name in a host requirement value must be rejected
// regardless of whether the step happens to declare a stepEnvironments
// entry.
func TestCheckTemplateExpressions_StepEnvironmentSymbolsDoNotLeakIntoHostRequirements(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  stepEnvironments:
  - name: Env1
    script:
      actions:
        onEnter:
          command: echo
          args: ["hi"]
  hostRequirements:
    attributes:
    - name: attr.custom.dir
      anyOf: ["{{ Step.Name }}"]
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	const wantPtr = "/steps/0/hostRequirements/attributes/0/anyOf/0"
	found := false
	for _, e := range errs {
		if e.Pointer == wantPtr && strings.Contains(e.Message, "Step.Name") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an out-of-scope error at %s mentioning Step.Name (a step environment's "+
			"own symbols must not leak into hostRequirements via the shared stepLet map); got: %v",
			wantPtr, errs)
	}
}

// TestCheckTemplateExpressions_StepScriptLetDoesNotLeakIntoHostRequirements
// pins checkStepExpressions' syms clone (`maps.Clone(stepLet)`, used for the
// step script's own table) from the OTHER direction: a name the STEP
// SCRIPT's own let: binds must not become visible in hostRequirements or
// parameterSpace. Without the clone, `syms := stepLet` would alias the same
// map checkStepExpressions later copies into jobSyms, so
// checkLetBindings(s.Script.Let, ...) mutating syms would mutate stepLet
// itself, and "derived" would leak forward into jobSyms right alongside it.
func TestCheckTemplateExpressions_StepScriptLetDoesNotLeakIntoHostRequirements(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  script:
    let:
    - derived = 5
    actions:
      onRun:
        command: echo
        args: ["hi"]
  hostRequirements:
    attributes:
    - name: attr.custom.dir
      anyOf: ["{{ derived }}"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	const wantPtr = "/steps/0/hostRequirements/attributes/0/anyOf/0"
	found := false
	for _, e := range errs {
		if e.Pointer == wantPtr && strings.Contains(e.Message, "derived") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an unknown-symbol error at %s naming \"derived\" (a step SCRIPT's own "+
			"let name must not leak into hostRequirements); got: %v", wantPtr, errs)
	}
}

// TestCheckTemplateExpressions_StepEnvironmentLetDoesNotLeakToSiblingEnvironment
// pins the same baseSyms clone from the sibling-environment angle: two
// stepEnvironments entries sharing one checkEnvironmentExpressions call and
// one outerLet map. Environment 0's own EnvironmentScript.let must not
// become visible to environment 1.
func TestCheckTemplateExpressions_StepEnvironmentLetDoesNotLeakToSiblingEnvironment(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  stepEnvironments:
  - name: Env0
    script:
      let:
      - only_in_env0 = 1
      actions:
        onEnter:
          command: echo
          args: ["hi"]
  - name: Env1
    script:
      actions:
        onEnter:
          command: echo
          args: ["{{ only_in_env0 }}"]
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	const wantPtr = "/steps/0/stepEnvironments/1/script/actions/onEnter/args/0"
	found := false
	for _, e := range errs {
		if e.Pointer == wantPtr && strings.Contains(e.Message, "only_in_env0") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an unknown-symbol error at %s naming \"only_in_env0\" (environment 0's "+
			"own let must not leak into environment 1); got: %v", wantPtr, errs)
	}
}

// TestCheckTemplateExpressions_EnvironmentOwnLetNotVisibleInItsVariables pins
// the Variables/scriptSyms split: section 3.6.2's <EnvironmentScript>.let row
// lists actions and embeddedFiles as the only visible-in positions, because
// Variables sits on the parent Environment, a SIBLING of Script -- not a
// descendant of it. An environment's own let name must therefore be UNKNOWN
// when referenced from that same environment's variables.
func TestCheckTemplateExpressions_EnvironmentOwnLetNotVisibleInItsVariables(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  stepEnvironments:
  - name: Env1
    script:
      let:
      - greeting = 'hi'
      actions:
        onEnter:
          command: echo
          args: ["{{ greeting }}"]
    variables:
      MSG: "{{ greeting }}"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	const wantPtr = "/steps/0/stepEnvironments/0/variables/MSG"
	found := false
	for _, e := range errs {
		if e.Pointer == wantPtr && strings.Contains(e.Message, "greeting") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an unknown-symbol error at %s naming \"greeting\" (an environment's own "+
			"let must not be visible in its own variables); got: %v", wantPtr, errs)
	}
}

// TestCheckTemplateExpressions_StepLetVisibleInStepEnvironmentVariables is
// the positive half of the Variables split above: row 1 names the WHOLE
// stepEnvironments subtree as visible to the enclosing StepTemplate.let, and
// Variables is a descendant of that subtree (unlike EnvironmentScript.let,
// which cannot reach it). A step-template let name must resolve inside a
// step environment's variables.
func TestCheckTemplateExpressions_StepLetVisibleInStepEnvironmentVariables(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
steps:
- name: Step1
  let:
  - tag = 'hello'
  stepEnvironments:
  - name: Env1
    script:
      actions:
        onEnter:
          command: echo
          args: ["hi"]
    variables:
      MSG: "{{ tag }}"
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Errorf("checkTemplateExpressions = %v, want no errors (a step-template let name "+
			"must be visible in a step environment's variables); got: %v", errs, errs)
	}
}

// ─── section 1.3.7: a comprehension's loop variable vs a "let" binding ─────
//
// EXPR/job_templates/3.6--let-comprehension-shadows*.invalid.yaml has five
// fixtures, and every one of them declares a LIST[INT] job parameter with a
// YAML sequence default ("default: [1, 2, 3]") -- sub-project F's work,
// unsupported today. Verified directly (Parse against each fixture file):
// all five fail at PARSE time, before checkTemplateExpressions -- let alone
// this rule -- ever runs, with "openjd: default must be a string, number, or
// boolean". That error fires generically against the YAML shape of the
// default value; it is not even a dedicated "unknown parameter type"
// rejection; a LIST[INT] parameter with a scalar default parses fine and
// only then fails Validate with "unknown type \"LIST[INT]\"". Either way,
// every one of the five scores as a pass in test-conformance whether or not
// section 1.3.7's shadowing rule works at all. That is the same trap
// sub-project E2 fell into, crediting its scope model with rejecting a
// fixture the YAML decoder was rejecting on its own; that whole-branch
// review had to correct the claim in two places. So the tests below
// transcribe each fixture's INTENT with a parameter type that exists today
// (INT), naming the fixture each one stands in for, rather than trusting the
// conformance score to prove anything here.

// TestCheckTemplateExpressions_ComprehensionCannotShadowStepLet stands in for
// BOTH EXPR/job_templates/3.6--let-comprehension-shadows.invalid.yaml and
// 3.6--let-comprehension-shadows-step-let.invalid.yaml. The two fixtures are
// byte-for-byte identical (verified with diff): the design doc
// (docs/superpowers/specs/2026-08-08-expr-let-bindings-design.md, section
// 5.2) names them the "plain" and "step-template" cases respectively, but
// both bind "x" on a step template's own let and comprehend over "x" in that
// step's script -- the same position, so one test stands in for both rather
// than duplicating an identical body under a second name.
func TestCheckTemplateExpressions_ComprehensionCannotShadowStepLet(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: N
  type: INT
  default: 3
steps:
- name: Step1
  let:
  - x = 10
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ [x for x in range(Param.N)] }}"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatal("a comprehension variable shadowed a let binding and was accepted")
	}
	if !strings.Contains(errs[0].Message, "shadows an existing binding") {
		t.Errorf("message %q is not section 1.3.7's shadowing rejection", errs[0].Message)
	}
}

// TestCheckTemplateExpressions_ComprehensionCannotShadowStepScriptLet stands
// in for EXPR/job_templates/3.6--let-comprehension-shadows-script-let.invalid.yaml.
// It binds "x" on the step SCRIPT's own let (ScopeStepScript) rather than the
// step template's, and comprehends over "x" in that same script's action --
// the design doc's "step-script" case.
func TestCheckTemplateExpressions_ComprehensionCannotShadowStepScriptLet(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: N
  type: INT
  default: 3
steps:
- name: Step1
  script:
    let:
    - x = 10
    actions:
      onRun:
        command: echo
        args: ["{{ [x for x in range(Param.N)] }}"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatal("a comprehension variable shadowed a step script's let binding and was accepted")
	}
	if !strings.Contains(errs[0].Message, "shadows an existing binding") {
		t.Errorf("message %q is not section 1.3.7's shadowing rejection", errs[0].Message)
	}
}

// TestCheckTemplateExpressions_ComprehensionCannotShadowEnvironmentLet stands
// in for EXPR/job_templates/3.6--let-comprehension-shadows-env.invalid.yaml --
// the design doc's "environment" case. It binds "x" on a JOB environment's
// script let (ScopeJobEnvironment) and comprehends over "x" in that same
// environment's onEnter action.
func TestCheckTemplateExpressions_ComprehensionCannotShadowEnvironmentLet(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: N
  type: INT
  default: 3
jobEnvironments:
- name: Env1
  script:
    let:
    - x = 10
    actions:
      onEnter:
        command: echo
        args: ["{{ [x for x in range(Param.N)] }}"]
steps:
- name: Step1
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
`)
	errs := checkTemplateExpressions(tmpl, nil)
	if len(errs) == 0 {
		t.Fatal("a comprehension variable shadowed a job environment's let binding and was accepted")
	}
	if !strings.Contains(errs[0].Message, "shadows an existing binding") {
		t.Errorf("message %q is not section 1.3.7's shadowing rejection", errs[0].Message)
	}
}

// EXPR/job_templates/3.6--let-comprehension-shadows-simple-action.invalid.yaml
// -- the design doc's "simple-action" case -- has no sqi equivalent: it binds
// "x" on a bash SimpleAction's own let and comprehends over "x" in that
// action's inline script, but SimpleAction (bash/etc. as shorthand for a full
// Script) is a FEATURE_BUNDLE_1 element sqi does not model (design spec
// section 1.1). There is nothing to transcribe it into, so it is recorded
// here rather than silently covering four of the five fixtures.

// TestCheckTemplateExpressions_ComprehensionMayReuseAnUnboundName is the
// negative half: without it, a checker that rejected every comprehension
// outright would still pass every test above. "y" is not bound anywhere in
// scope, so comprehending over it must be accepted.
func TestCheckTemplateExpressions_ComprehensionMayReuseAnUnboundName(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: N
  type: INT
  default: 3
steps:
- name: Step1
  let:
  - x = 10
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ [y for y in range(Param.N)] }}"]
`)
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Errorf("checkTemplateExpressions = %v, want no errors", errs)
	}
}

// TestCheckTemplateExpressions_ComprehensionMayReuseAStepLetFromAnotherStep is
// the stronger negative: a "let" name bound in one step's scope must not make
// a comprehension over that same name illegal in a SIBLING step. Task 8's
// review verified this behavior by construction -- checkTemplateExpressions
// builds a fresh stepTemplateSyms table per step (checkStepExpressions calls
// symbolsFor(tmpl, &s, nil, ScopeStepTemplate, params) once per iteration of
// its loop) -- but no repo test pinned it before this one. Step0's "x" binding
// must never reach Step1's table, so Step1's "[x for x in ...]" comprehends
// over an unbound name from Step1's own point of view, exactly like the
// unbound-name case above. Without this test, a checker that (wrongly) shared
// one symbol table across every step -- the mirror image of what
// TestCheckTemplateExpressions_StepLetNotVisibleInAnotherStep already pins
// for a bare reference -- would still pass every test above by rejecting
// every comprehension whose loop variable happens to match ANY step's let
// binding, not just the enclosing one.
func TestCheckTemplateExpressions_ComprehensionMayReuseAStepLetFromAnotherStep(t *testing.T) {
	tmpl := mustParseEXPR(t, `specificationVersion: jobtemplate-2023-09
extensions: [EXPR]
name: TestJob
parameterDefinitions:
- name: N
  type: INT
  default: 3
steps:
- name: Step0
  let:
  - x = 10
  script:
    actions:
      onRun:
        command: echo
        args: ["hi"]
- name: Step1
  script:
    actions:
      onRun:
        command: echo
        args: ["{{ [x for x in range(Param.N)] }}"]
`)
	if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
		t.Errorf("checkTemplateExpressions = %v, want no errors (Step0's let binding must not "+
			"leak into Step1's comprehension check); got: %v", errs, errs)
	}
}
