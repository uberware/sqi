// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"
)

// EXPR sub-project E4b Task 4: the agreement instrument design spec §4
// describes and the task brief calls "the wave's only real instrument".
//
// Conformance cannot judge section 1.3.12's extended range: the suite scores
// job_templates, which is parse-and-validate, and never expands a parameter
// space (design spec §6). So the checker (checkParameterSpaceExpressions,
// exprcheck.go) and the resolver (ResolveParameterSpaceParams + Expand-
// ParameterSpace, resolve.go/expand.go) could disagree about a range's
// validity and the score would not move. This file is the thing that would
// catch that: for a battery of range expressions, it runs BOTH the real
// checker and the real resolver+expander against the SAME *JobTemplate and
// *StepParameterSpace, and proves, in both directions:
//
//   - a range the checker ACCEPTS must resolve and expand without error;
//   - a range the checker REJECTS must never reach expansion (proven by the
//     resolver refusing it too, independent of production's own call order,
//     which already short-circuits on a checker rejection --
//     checkExpressionsAtSubmit runs before ResolveParameterSpaceParams in
//     submit.go's Submit; see that function's own doc comment).
//
// checkTemplateExpressions is called directly (not through
// ValidateWithOptions) because it is what submit.go's checkExpressionsAtSubmit
// itself calls, with the SAME concrete, bound job-parameter map
// ResolveParameterSpaceParams receives -- ValidateWithOptions only ever calls
// it with params=nil (phase 1, every job parameter still unresolved), which
// would not exercise the Param.<name>-dependent cases below at all. This is
// the real, unexported, production entry point for phase 2's expression
// re-check, not a reconstruction of it.

// buildRangeAgreementTemplate builds a single-step EXPR-declaring template
// whose one task-parameter definition (type typ) carries either a whole-field
// RangeExpr (rangeExpr non-nil) or a single-entry RangeList (rangeList),
// mirroring exprcheck_test.go's rangeCheckTemplate -- the same shape
// checkTemplateExpressions and ResolveParameterSpaceParams both walk in
// production, since submit.go's Submit hands the identical *JobTemplate to
// both (checkExpressionsAtSubmit, then per-step expandStepTaskParams).
func buildRangeAgreementTemplate(typ TaskParamType, rangeList []string, rangeExpr *string, jobParams []JobParameter) *JobTemplate {
	return &JobTemplate{
		Name:                 "T",
		Extensions:           []string{"EXPR"},
		ParameterDefinitions: jobParams,
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

// assertRowAccepted runs the real checker and the real resolver+expander
// against tmpl's one task-parameter definition and fails t unless ALL THREE
// agree the range is valid: the checker reports no error, the resolver
// reports no error (agreement's accept-side claim), and the resolved space
// expands without error (design spec §4's "must resolve and expand without
// error"). It returns the resolved space and the expanded rows so callers
// that pin an exact value (order, count, canonical text) can assert further.
func assertRowAccepted(t *testing.T, tmpl *JobTemplate, boundParams map[string]string) (*StepParameterSpace, []TaskParams) {
	t.Helper()
	ps := tmpl.Steps[0].ParameterSpace

	checkErrs := checkTemplateExpressions(tmpl, boundParams)
	if len(checkErrs) != 0 {
		t.Fatalf("checker rejected a range this row expects accepted: %v", checkErrs)
	}

	resolved, resolveErrs := ResolveParameterSpaceParams(tmpl, ps, boundParams)
	if len(resolveErrs) != 0 {
		t.Fatalf("AGREEMENT FAILURE: checker accepted but resolver rejected: %v", resolveErrs)
	}
	if resolved == nil {
		t.Fatalf("resolver reported no errors but returned a nil space")
	}

	rows, err := ExpandParameterSpace(resolved)
	if err != nil {
		t.Fatalf("AGREEMENT FAILURE: checker and resolver accepted but expansion failed: %v", err)
	}
	return resolved, rows
}

// assertRowRejected is assertRowAccepted's sibling for design spec §4's other
// direction: fails t unless the checker reports an error AND the resolver,
// run independently and directly (not gated behind the checker's verdict, the
// way production submit.go's call ordering would gate it), ALSO reports an
// error and returns a nil space -- so there is nothing for a caller to hand
// ExpandParameterSpace even if it tried. "Must never reach expansion" is
// proven from the resolver's own refusal, not inferred from checker-first
// call order.
func assertRowRejected(t *testing.T, tmpl *JobTemplate, boundParams map[string]string) {
	t.Helper()
	ps := tmpl.Steps[0].ParameterSpace

	checkErrs := checkTemplateExpressions(tmpl, boundParams)
	if len(checkErrs) == 0 {
		t.Fatalf("checker accepted a range this row expects rejected")
	}

	resolved, resolveErrs := ResolveParameterSpaceParams(tmpl, ps, boundParams)
	if len(resolveErrs) == 0 {
		t.Fatalf("AGREEMENT FAILURE: checker rejected but resolver accepted: resolved=%+v", resolved)
	}
	if resolved != nil {
		t.Fatalf("resolver reported errors but returned a non-nil space: %+v", resolved)
	}
}

// TestRangeCheckerResolverAgreement_PerType is the table the task brief asks
// for: "one row per declared type, each with an accepted and a rejected
// expression" (Step 1), at BOTH positions section 1.3.12 defines -- the
// whole-field RangeExpr and a single RangeList entry -- for every declared
// task-parameter type (design spec §3's table: INT, CHUNK[INT], FLOAT,
// STRING, PATH).
//
// The accept/reject bodies mirror exprcheck_test.go's
// TestCheckParameterSpaceExpressions_RangeTargetTypes on purpose: that test
// already established, for the checker alone, exactly which shapes the
// per-type target (rangeExprElemType/rangeExprFieldType) accepts and rejects.
// Reusing the same bodies here means this test proves the SAME verdicts hold
// when the real resolver and real expander run on the identical input, not a
// second, independently-chosen set of examples that might happen to agree by
// accident.
func TestRangeCheckerResolverAgreement_PerType(t *testing.T) {
	tests := []struct {
		name        string
		typ         TaskParamType
		wholeAccept string
		wholeReject string
		entryAccept string
		entryReject string
	}{
		{
			name: "INT", typ: TaskParamTypeInt,
			wholeAccept: "{{ [1, 2, 3] }}",
			wholeReject: "{{ ['a'] }}",
			// design spec §4 / task brief's own distinguishing case: before
			// Task 3 the checker (TargetString) accepted this and expansion
			// failed with "invalid integer \"5.0\"". The checker now targets
			// TInt, under which 5.0 is a legal, exact float->int coercion, so
			// this row proves the fix: checker accepts, resolver renders the
			// canonical int text "5" (not "5.0"), and expansion succeeds.
			entryAccept: "{{ 5.0 }}",
			// 2.5 is not integral -- TInt's float->int coercion rejects it
			// exactly (not merely widened to a string TargetString would have
			// accepted).
			entryReject: "{{ 2.5 }}",
		},
		{
			name: "CHUNK[INT]", typ: TaskParamTypeChunkInt,
			wholeAccept: "{{ [1, 2, 3] }}",
			wholeReject: "{{ ['a'] }}",
			entryAccept: "{{ 5.0 }}",
			entryReject: "{{ 2.5 }}",
		},
		{
			name: "FLOAT", typ: TaskParamTypeFloat,
			wholeAccept: "{{ [1.0, 2.5] }}",
			wholeReject: "{{ ['abc'] }}",
			entryAccept: "{{ 2.5 }}",
			// A bool coerces to string unconditionally but has no bool->float
			// rule at all -- TString would have accepted this; TFloat does
			// not.
			entryReject: "{{ true }}",
		},
		{
			name: "STRING", typ: TaskParamTypeString,
			wholeAccept: "{{ ['a', 'b'] }}",
			// Every scalar coerces to string, so STRING has no wrong-
			// element-type whole-field rejection to build (matching
			// exprcheck_test.go's own tc.wholeReject == "" for this row) --
			// the shape rejection below (entryAccept's bare scalar in the
			// whole-field position) is the only rejection this type has at
			// the field position.
			wholeReject: "{{ 5 }}",
			entryAccept: "{{ 'a' }}",
			// Symmetric to the whole-field case: nothing type-mismatches a
			// STRING entry, so its rejection is the wrong SHAPE -- a list
			// where a scalar belongs.
			entryReject: "{{ ['a', 'b'] }}",
		},
		{
			name: "PATH", typ: TaskParamTypePath,
			wholeAccept: "{{ [path('/a'), path('/b')] }}",
			// An int coerces to string unconditionally but only a string
			// coerces to path (section 1.2.3: path <- string only).
			wholeReject: "{{ [1, 2] }}",
			entryAccept: "{{ path('/a') }}",
			entryReject: "{{ 5 }}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("whole-field accepted", func(t *testing.T) {
				tmpl := buildRangeAgreementTemplate(tc.typ, nil, new(tc.wholeAccept), nil)
				assertRowAccepted(t, tmpl, nil)
			})
			t.Run("whole-field rejected", func(t *testing.T) {
				tmpl := buildRangeAgreementTemplate(tc.typ, nil, new(tc.wholeReject), nil)
				assertRowRejected(t, tmpl, nil)
			})
			t.Run("entry accepted", func(t *testing.T) {
				tmpl := buildRangeAgreementTemplate(tc.typ, []string{tc.entryAccept}, nil, nil)
				assertRowAccepted(t, tmpl, nil)
			})
			t.Run("entry rejected", func(t *testing.T) {
				tmpl := buildRangeAgreementTemplate(tc.typ, []string{tc.entryReject}, nil, nil)
				assertRowRejected(t, tmpl, nil)
			})
		})
	}
}

// TestRangeCheckerResolverAgreement_LoneRangeExprPolicy pins Task 1's own
// whole-field guarantee (task brief): a LONE {{ range_expr(...) }} whole-field
// expression is section 1.3.12's extended form, evaluated as a VALUE under
// internal/openjd/expr's OWN policy (the zero intrange.Policy) -- never
// re-parsed as literal <IntRangeExpr> text under internal/openjd's stricter
// one (design spec §2.1's trap). Both texts below are cases where the two
// policies genuinely diverge (resolve_test.go's
// TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy /
// TestResolveParameterSpaceParams_LiteralIntRangeKeepsOpenJDPolicy already
// pin the resolver side of this fact alone); here the point is that the
// CHECKER accepts the same lone expression the resolver does, and the
// resolved-and-expanded result is exactly the expr-policy answer, not the
// stricter one:
//
//   - range_expr("5-1"): start > end is legal under expr's policy (section
//     3.4.1.1.1's own formula collapses it to the single value [5]) -- the
//     literal text "5-1" alone is REJECTED under internal/openjd's own
//     policy ("must be ≤ end").
//   - range_expr("10-15:2,1-5"): expr's policy orders the result INCREASING
//     ([1,2,3,4,5,10,12,14]) -- the literal text alone orders FIRST-SEEN
//     ([10,12,14,1,2,3,4,5]) under internal/openjd's own policy.
func TestRangeCheckerResolverAgreement_LoneRangeExprPolicy(t *testing.T) {
	tests := []struct {
		name string
		call string
		want []string
	}{
		{name: "start > end collapses to [start]", call: `range_expr("5-1")`, want: []string{"5"}},
		{
			name: "overlapping sub-ranges order INCREASING, not first-seen",
			call: `range_expr("10-15:2,1-5")`,
			want: []string{"1", "2", "3", "4", "5", "10", "12", "14"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new("{{ "+tc.call+" }}"), nil)
			resolved, rows := assertRowAccepted(t, tmpl, nil)

			def := resolved.TaskParameterDefinitions[0]
			if def.RangeExpr != nil {
				t.Errorf("RangeExpr = %q, want nil (cleared per design spec §2)", *def.RangeExpr)
			}
			if len(def.RangeList) != len(tc.want) {
				t.Fatalf("RangeList = %v, want %v", def.RangeList, tc.want)
			}
			for i, want := range tc.want {
				if def.RangeList[i] != want {
					t.Errorf("RangeList[%d] = %q, want %q", i, def.RangeList[i], want)
				}
			}
			if len(rows) != len(tc.want) {
				t.Fatalf("expanded %d rows, want %d", len(rows), len(tc.want))
			}
			for i, want := range tc.want {
				if got := rows[i]["P"]; got != want {
					t.Errorf("rows[%d][P] = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// TestRangeCheckerResolverAgreement_NonLoneRangeString pins Task 2's
// non-lone RangeString guarantee (task brief): with EXPR declared,
// range: "1-{{ Param.End * 2 }}" is section 1.3.2's ordinary format string,
// not section 1.3.12's whole-field list form (the reference is embedded in
// surrounding text, so fmtstring.LoneRef is false) -- accepted at both layers
// when the embedded expression evaluates cleanly, and rejected at both layers
// on an unknown symbol or a parse error, exactly like any other EXPR format
// string position.
func TestRangeCheckerResolverAgreement_NonLoneRangeString(t *testing.T) {
	jobParams := []JobParameter{{Name: "End", Type: JobParamTypeInt}}
	boundParams := map[string]string{"End": "4"}

	t.Run("accepted: embedded reference evaluates cleanly", func(t *testing.T) {
		tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new("1-{{ Param.End * 2 }}"), jobParams)
		resolved, rows := assertRowAccepted(t, tmpl, boundParams)

		def := resolved.TaskParameterDefinitions[0]
		if def.RangeExpr == nil || *def.RangeExpr != "1-8" {
			t.Fatalf("RangeExpr = %v, want \"1-8\" (literal text, still parsed by internal/openjd's own parseIntRangeExpr)", def.RangeExpr)
		}
		want := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
		if len(rows) != len(want) {
			t.Fatalf("expanded %d rows, want %d: %v", len(rows), len(want), rows)
		}
		for i, w := range want {
			if got := rows[i]["P"]; got != w {
				t.Errorf("rows[%d][P] = %q, want %q", i, got, w)
			}
		}
	})

	t.Run("rejected: unknown symbol", func(t *testing.T) {
		tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new("1-{{ Param.Missing * 2 }}"), jobParams)
		assertRowRejected(t, tmpl, boundParams)
	})

	t.Run("rejected: parse error", func(t *testing.T) {
		tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new("1-{{ Param.End * }}"), jobParams)
		assertRowRejected(t, tmpl, boundParams)
	})
}

// TestRangeCheckerResolverAgreement_EmbeddedRangeExprReorder pins the
// embedded range_expr() case the task brief names explicitly: a NON-lone
// whole-field RangeString whose embedded reference itself evaluates to a
// genuine range_expr value. resolveRangeExprField's own doc comment rules
// this is CORRECT, not a defect, and the task brief is equally direct: "that
// reorder is deliberate and ruled on -- it must stay pinned, not be 'fixed'."
//
// {{ range_expr("10-15:2,1-5") }},7 composes to the literal text
// "10-15:2,1-5,7", which is section 2.1's whole point: Value.String() renders
// the embedded range_expr value back to ITS OWN <IntRangeExpr> text, and the
// COMPOSED result is ordinary base-spec range syntax a human could have typed
// by hand -- so it is parsed by internal/openjd's OWN parseIntRangeExpr
// (first-seen order), not internal/openjd/expr's (increasing order). This is
// intentionally the OPPOSITE result from
// TestRangeCheckerResolverAgreement_LoneRangeExprPolicy's identical range
// text, because here the expression is EMBEDDED rather than the WHOLE field.
func TestRangeCheckerResolverAgreement_EmbeddedRangeExprReorder(t *testing.T) {
	tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new(`{{ range_expr("10-15:2,1-5") }},7`), nil)
	resolved, rows := assertRowAccepted(t, tmpl, nil)

	def := resolved.TaskParameterDefinitions[0]
	if def.RangeExpr == nil || *def.RangeExpr != "10-15:2,1-5,7" {
		t.Fatalf("RangeExpr = %v, want \"10-15:2,1-5,7\"", def.RangeExpr)
	}
	want := []string{"10", "12", "14", "1", "2", "3", "4", "5", "7"}
	if len(rows) != len(want) {
		t.Fatalf("expanded %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if got := rows[i]["P"]; got != w {
			t.Errorf("rows[%d][P] = %q, want %q", i, got, w)
		}
	}
}

// TestRangeCheckerResolverAgreement_KnownNonLoneDivergences documents and
// pins the two cases design spec §6 and the task brief both call out by
// name as KNOWN, ACCEPTED divergences from the accept-implies-expand-succeeds
// claim the rest of this file proves -- NOT agreement failures, and not rows
// for assertRowAccepted/assertRowRejected's generic contract.
//
// Both are properties of the NON-LONE position specifically: checkFormatString
// (exprcheck.go) and resolveFormatStringExpr (resolve.go) BOTH evaluate a
// non-lone segment's embedded reference at expr.TAny -- the declared element
// or field target is structurally unused for that segment, by design, on
// both sides identically (section 1.3.2: an embedded reference renders to
// text regardless of its value's type). So the checker's "no error" verdict
// for these two inputs means only "every embedded reference evaluated
// without error", never "the composed text is valid range syntax for this
// parameter's type" -- and a base-spec (non-EXPR) template with the
// equivalent literal text fails identically, which is what makes this a
// pre-existing, structural property rather than something this wave
// introduced or could reasonably "fix" without changing what a non-lone
// format string means everywhere else in the codebase.
func TestRangeCheckerResolverAgreement_KnownNonLoneDivergences(t *testing.T) {
	// INT entry "x{{ 2.5 }}": the "x" prefix makes this non-lone, so the
	// embedded 2.5 is rendered with Value.String() ("2.5") and concatenated,
	// giving RangeList entry "x2.5" -- text validateIntList rejects outright,
	// exactly as base-spec literal range: ["x2.5"] would (no EXPR needed to
	// reproduce this failure).
	t.Run("INT entry x{{ 2.5 }}: checker+resolver accept, expansion fails (base-spec-equivalent)", func(t *testing.T) {
		tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, []string{"x{{ 2.5 }}"}, nil, nil)
		ps := tmpl.Steps[0].ParameterSpace

		if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
			t.Fatalf("checker unexpectedly rejected: %v", errs)
		}
		resolved, errs := ResolveParameterSpaceParams(tmpl, ps, nil)
		if len(errs) != 0 {
			t.Fatalf("resolver unexpectedly rejected: %v", errs)
		}
		if got := resolved.TaskParameterDefinitions[0].RangeList[0]; got != "x2.5" {
			t.Fatalf("resolved entry = %q, want %q", got, "x2.5")
		}
		_, err := ExpandParameterSpace(resolved)
		if err == nil || !strings.Contains(err.Error(), `invalid integer "x2.5"`) {
			t.Fatalf("ExpandParameterSpace error = %v, want it to contain %q", err, `invalid integer "x2.5"`)
		}
	})

	// FLOAT/STRING/PATH whole-field "1-{{ Param.S }}": the composed text
	// resolves fine and is assigned to RangeExpr -- but expand.go's
	// expandTaskParam reads RangeList, never RangeExpr, for these three
	// types (only INT/CHUNK[INT] ever consult RangeExpr at expansion). A
	// base-spec literal range: "1-3" on a FLOAT/STRING/PATH parameter sets
	// RangeExpr the same way (design spec §1.1) and fails identically.
	for _, typ := range []TaskParamType{TaskParamTypeFloat, TaskParamTypeString, TaskParamTypePath} {
		t.Run(string(typ)+` whole-field "1-{{ Param.S }}": checker+resolver accept, expansion fails (base-spec-equivalent)`, func(t *testing.T) {
			jobParams := []JobParameter{{Name: "S", Type: JobParamTypeString}}
			boundParams := map[string]string{"S": "3"}
			tmpl := buildRangeAgreementTemplate(typ, nil, new("1-{{ Param.S }}"), jobParams)
			ps := tmpl.Steps[0].ParameterSpace

			if errs := checkTemplateExpressions(tmpl, boundParams); len(errs) != 0 {
				t.Fatalf("checker unexpectedly rejected: %v", errs)
			}
			resolved, errs := ResolveParameterSpaceParams(tmpl, ps, boundParams)
			if len(errs) != 0 {
				t.Fatalf("resolver unexpectedly rejected: %v", errs)
			}
			if got := resolved.TaskParameterDefinitions[0].RangeExpr; got == nil || *got != "1-3" {
				t.Fatalf("resolved RangeExpr = %v, want \"1-3\"", got)
			}
			_, err := ExpandParameterSpace(resolved)
			if err == nil || !strings.Contains(err.Error(), "range list is empty") {
				t.Fatalf("ExpandParameterSpace error = %v, want it to contain %q", err, "range list is empty")
			}
		})
	}
}
