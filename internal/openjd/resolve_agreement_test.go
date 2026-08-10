// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"slices"
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
	return buildRangeAgreementTemplateWithLet(typ, rangeList, rangeExpr, jobParams, nil)
}

// buildRangeAgreementTemplateWithLet is buildRangeAgreementTemplate with the
// step template's own let: block filled in (section 3.6.2 row 1: the names it
// binds are visible in parameterSpace).
//
// It exists because the table above had NO Let field, and that blind spot is
// exactly what let EXPR sub-project E4b ship a checker that saw a step's let
// names at the range position and a resolver that did not -- a template that
// validated at upload, passed the phase-2 re-check, and then died in
// expandStepTaskParams with unknown symbol "base". A test table cannot catch
// a divergence in a field it cannot express, so the field is here now and
// TestRangeCheckerResolverAgreement_StepLet exercises all three range shapes
// through it.
func buildRangeAgreementTemplateWithLet(
	typ TaskParamType, rangeList []string, rangeExpr *string, jobParams []JobParameter, let []string,
) *JobTemplate {
	return &JobTemplate{
		Name:                 "T",
		Extensions:           []string{"EXPR"},
		ParameterDefinitions: jobParams,
		Steps: []StepTemplate{{
			Name:   "Step1",
			Let:    let,
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

	resolved, resolveErrs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, boundParams)
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

	resolved, resolveErrs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, boundParams)
	if len(resolveErrs) == 0 {
		t.Fatalf("AGREEMENT FAILURE: checker rejected but resolver accepted: resolved=%+v", resolved)
	}
	if resolved != nil {
		t.Fatalf("resolver reported errors but returned a non-nil space: %+v", resolved)
	}

	// Same verdict is not enough: the two layers must say the SAME THING.
	// EXPR sub-project E4b's whole-branch review found them reporting
	// "cannot be coerced to list[int] | range_expr" at validate-time and
	// "cannot be coerced to list[int]" at submit-time for one and the same
	// template -- a symptom of two separately-chosen targets, which is the
	// disease this file exists to detect. Now that both layers call
	// rangeExprFieldType/rangeExprElemType, the message is one message; this
	// assertion is what keeps it that way. Only the pointer differs, by
	// design: the checker walks from the template root, the resolver from the
	// step (submit.go prefixes "/steps/<i>").
	checkMsgs := make([]string, len(checkErrs))
	for i, e := range checkErrs {
		checkMsgs[i] = e.Message
	}
	resolveMsgs := make([]string, len(resolveErrs))
	for i, e := range resolveErrs {
		resolveMsgs[i] = e.Message
	}
	if !slices.Equal(checkMsgs, resolveMsgs) {
		t.Errorf(
			"MESSAGE DRIFT: checker said %q, resolver said %q for the same input",
			checkMsgs, resolveMsgs,
		)
	}
}

// TestRangeCheckerResolverAgreement_INTWholeFieldRangeString is the
// regression test for EXPR sub-project E4b's whole-branch review Critical 1:
// the INT whole-field target omitted two required union members.
//
// Section 1.3.12 leaves the INT row "(unchanged, but see RangeString note
// below)" and that note extends the RangeString with an expression evaluating
// to a range_expr or a list[int] "IN ADDITION TO the original <IntRangeExpr>
// grammar". The conformance suite states the resulting target verbatim --
// EXPR/jobs/expr1.2.3--union-target-type.test.yaml:12,
// "INT task 'range': int | string | range_expr | list[int] (match-first)" --
// and this table is that fixture's own six range cases, each carried all the
// way through to expanded task rows, which the fixture's job-execution suite
// does and sqi's job_templates conformance scoring structurally cannot.
//
// Measured at the reviewed HEAD, the first four rows were REJECTED, and
// rejected at PHASE 1 (template upload, params still unresolved), with
// "unresolved[string] cannot be coerced to list[int] | range_expr". Declaring
// EXPR therefore REMOVED base-spec capability at this field: the identical
// template without extensions: [EXPR] expanded correctly, and all six of this
// repo's own reference render presets (presets/sqi/*.yaml) use exactly the
// first row's shape -- range: "{{Param.Frames}}" with a STRING Frames.
//
// Phase 1 is asserted separately and deliberately: a fix that only made
// phase 2 pass would leave every such template rejected at upload, which is
// where the presets actually failed. That half needed its own fix, in
// expr/coerce.go's coerceUnresolved -- see the carve-out comment there.
func TestRangeCheckerResolverAgreement_INTWholeFieldRangeString(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		jobParams []JobParameter
		bound     map[string]string
		// wantRangeExpr is the text the resolver writes back into RangeExpr
		// (the int/string arms, which are range TEXT); wantRangeList is the
		// list it writes instead (the range_expr/list arms). Exactly one is
		// set per row.
		wantRangeExpr string
		wantRangeList []string
		wantRows      []string
	}{
		{
			// The preset shape, and fixture case 1.
			name:          "string member: a STRING job parameter holding range text",
			body:          "{{Param.Frames}}",
			jobParams:     []JobParameter{{Name: "Frames", Type: JobParamTypeString}},
			bound:         map[string]string{"Frames": "1-7:2"},
			wantRangeExpr: "1-7:2",
			wantRows:      []string{"1", "3", "5", "7"},
		},
		{
			// Fixture case 2: an INT job parameter is a one-value range.
			name:          "int member: an INT job parameter",
			body:          "{{Param.N}}",
			jobParams:     []JobParameter{{Name: "N", Type: JobParamTypeInt}},
			bound:         map[string]string{"N": "7"},
			wantRangeExpr: "7",
			wantRows:      []string{"7"},
		},
		{
			// Fixture case 5: arithmetic evaluates unconstrained and its int
			// result satisfies the int member.
			name:          "int member: arithmetic",
			body:          "{{Param.N + 1}}",
			jobParams:     []JobParameter{{Name: "N", Type: JobParamTypeInt}},
			bound:         map[string]string{"N": "7"},
			wantRangeExpr: "8",
			wantRows:      []string{"8"},
		},
		{
			name:          "string member: a string literal",
			body:          "{{ '1-4' }}",
			wantRangeExpr: "1-4",
			wantRows:      []string{"1", "2", "3", "4"},
		},
		{
			// Fixture case 3, with range_expr() standing in for the fixture's
			// RANGE_EXPR job-parameter type (section 1.2.2's job-parameter
			// types are sub-project F's, not shipped).
			name:          "range_expr member",
			body:          `{{ range_expr("10-11") }}`,
			wantRangeList: []string{"10", "11"},
			wantRows:      []string{"10", "11"},
		},
		{
			// Fixture case 4.
			name:          "list[int] member",
			body:          "{{ [100, 101] }}",
			wantRangeList: []string{"100", "101"},
			wantRows:      []string{"100", "101"},
		},
	}

	for _, typ := range []TaskParamType{TaskParamTypeInt, TaskParamTypeChunkInt} {
		t.Run(string(typ), func(t *testing.T) {
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					tmpl := buildRangeAgreementTemplate(typ, nil, new(tc.body), tc.jobParams)

					// Phase 1: template upload, every job parameter still an
					// unresolved placeholder. This is where the reviewed HEAD
					// rejected, so it is asserted on its own.
					if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
						t.Fatalf("phase 1 (upload) rejected %q: %v", tc.body, errs)
					}

					resolved, rows := assertRowAccepted(t, tmpl, tc.bound)
					def := resolved.TaskParameterDefinitions[0]

					if tc.wantRangeExpr != "" {
						if def.RangeExpr == nil || *def.RangeExpr != tc.wantRangeExpr {
							t.Errorf("RangeExpr = %v, want %q (range TEXT, read by parseIntRangeExpr)",
								def.RangeExpr, tc.wantRangeExpr)
						}
						if len(def.RangeList) != 0 {
							t.Errorf("RangeList = %v, want empty for a text result", def.RangeList)
						}
					} else {
						if def.RangeExpr != nil {
							t.Errorf("RangeExpr = %q, want nil (cleared for a list result)", *def.RangeExpr)
						}
						if !slices.Equal(def.RangeList, tc.wantRangeList) {
							t.Errorf("RangeList = %v, want %v", def.RangeList, tc.wantRangeList)
						}
					}

					got := expandedValues(rows, "P", typ)
					if !slices.Equal(got, tc.wantRows) {
						t.Errorf("expanded values = %v, want %v", got, tc.wantRows)
					}
				})
			}
		})
	}
}

// expandedValues pulls the value of task parameter name out of every expanded
// row. A CHUNK[INT] definition groups its integers into chunks, so with
// chunks unset each row's value is the chunk's own <IntRangeExpr> text for a
// single value -- identical to the INT rendering for these one-value chunks,
// which is why both types share one expectation column above.
func expandedValues(rows []TaskParams, name string, _ TaskParamType) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r[name])
	}
	return out
}

// TestRangeCheckerResolverAgreement_StepLet is the regression test for EXPR
// sub-project E4b's whole-branch review Critical 2: a step template's let:
// names were visible to the CHECKER at the parameterSpace position (section
// 3.6.2 row 1, which exprcheck.go implemented) and invisible to the RESOLVER,
// which was never handed the step at all.
//
// Measured at the reviewed HEAD with step let: ["base = 10"], all three range
// shapes: the checker reported no errors and the resolver reported
// unknown symbol "base". The template validated at upload, passed the phase-2
// re-check, and then died in expandStepTaskParams naming a symbol the checker
// had just certified -- E4a's Critical repeated one sub-project later, at the
// position E4b owns.
//
// The table above could not have caught it, because buildRangeAgreementTemplate
// had no Let field at all; that is why buildRangeAgreementTemplateWithLet now
// exists.
func TestRangeCheckerResolverAgreement_StepLet(t *testing.T) {
	tests := []struct {
		name      string
		rangeExpr *string
		rangeList []string
		wantRows  []string
	}{
		{
			name:      "whole-field list expression over a let name",
			rangeExpr: new("{{ [base, base + 1] }}"),
			wantRows:  []string{"10", "11"},
		},
		{
			name:      "non-lone RangeString with a let name embedded",
			rangeExpr: new("1-{{ base }}"),
			wantRows:  []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"},
		},
		{
			name:      "range-list entry that is a let name",
			rangeList: []string{"{{ base }}"},
			wantRows:  []string{"10"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := buildRangeAgreementTemplateWithLet(
				TaskParamTypeInt, tc.rangeList, tc.rangeExpr, nil, []string{"base = 10"},
			)
			_, rows := assertRowAccepted(t, tmpl, nil)
			got := expandedValues(rows, "P", TaskParamTypeInt)
			if !slices.Equal(got, tc.wantRows) {
				t.Errorf("expanded values = %v, want %v", got, tc.wantRows)
			}
		})
	}
}

// TestRangeCheckerResolverAgreement_StepLetRejectionsAgree is the other half
// of Critical 2: threading the step in must not make the resolver ACCEPT more
// than the checker does either. A name no let: binds is still unknown at both
// layers, and a let: binding that cannot be evaluated is reported by both --
// the resolver at its own /let/<i> pointer rather than as a downstream
// "unknown symbol" on every range that referenced it.
func TestRangeCheckerResolverAgreement_StepLetRejectionsAgree(t *testing.T) {
	t.Run("a name no let binds is unknown at both layers", func(t *testing.T) {
		tmpl := buildRangeAgreementTemplateWithLet(
			TaskParamTypeInt, nil, new("{{ [other] }}"), nil, []string{"base = 10"},
		)
		assertRowRejected(t, tmpl, nil)
	})

	t.Run("an unevaluatable let binding is reported by both", func(t *testing.T) {
		tmpl := buildRangeAgreementTemplateWithLet(
			TaskParamTypeInt, nil, new("{{ [base] }}"), nil, []string{"base = 1 / 0"},
		)
		assertRowRejected(t, tmpl, nil)

		_, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], tmpl.Steps[0].ParameterSpace, nil)
		if len(errs) == 0 || errs[0].Pointer != "/let/0" {
			t.Fatalf("resolver errors = %v, want the first one at /let/0", errs)
		}
	})
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
// pins the KNOWN, ACCEPTED divergences from the accept-implies-expand-succeeds
// claim the rest of this file proves -- NOT agreement failures, and not rows
// for assertRowAccepted/assertRowRejected's generic contract.
//
// THE CONTRACT, RESTATED. An earlier revision of this comment said these were
// "properties of the NON-LONE position specifically", which implied the LONE
// whole-field position was exception-free. It is not, and EXPR sub-project
// E4b's whole-branch review found the counterexample (Minor 1): lone
// range: "{{ [] }}" is accepted by both layers at every type and then fails
// expansion with "range list is empty". The true statement is one line, and
// it covers every case in this function:
//
//	THE CHECKER JUDGES AN EXPRESSION'S TYPE. IT NEVER JUDGES THE SYNTAX,
//	LENGTH OR VALUE OF THE RANGE TEXT OR RANGE LIST THAT EXPRESSION PRODUCES.
//
// Three distinct shapes fall out of that one property, and all three are
// pinned below:
//
//  1. NON-LONE composition. checkFormatString (exprcheck.go) and
//     resolveFormatStringExpr (resolve.go) BOTH evaluate a non-lone segment's
//     embedded reference at expr.TAny -- the declared element or field target
//     is structurally unused for that segment, by design, on both sides
//     identically (section 1.3.2: an embedded reference renders to text
//     regardless of its value's type). "No error" therefore means only "every
//     embedded reference evaluated", never "the composed text is valid range
//     syntax for this parameter's type".
//  2. LONE, TEXT ARM. Since this fix widened the INT/CHUNK[INT] whole-field
//     target to section 1.3.12's full "int | string | range_expr | list[int]"
//     (rangeExprFieldType), a lone expression may now legitimately produce
//     range TEXT -- and the checker judges only that the result IS an int or
//     a string, not that the text parses as <IntRangeExpr>. So
//     range: "{{ 'abc' }}" type-checks and fails at expansion.
//  3. LONE, EMPTY LIST. An empty list is a perfectly well-typed
//     list[int]/list[float]/list[string]/list[path]. Its LENGTH is what is
//     wrong, and length is not a type.
//
// WHY NONE OF THE THREE IS "FIXED" HERE, which is a ruling, not an omission.
// Every one of them is base-spec-equivalent: a template with the literal text
// the expression computes fails the same way, with the same message. Measured
// for shapes 2 and 3 -- base-spec range: "abc" is rejected at VALIDATE with
// `range expression "abc": invalid integer "abc"`, and the EXPR form is
// rejected at EXPANSION with the identical `invalid integer "abc"` inside a
// SubmitValidationError. So the divergence is which LAYER reports and which
// JSON pointer it carries, never whether a bad template is caught: no
// malformed range reaches a task in any of the three.
//
// Closing them properly means giving the checker something it does not have
// and was deliberately not built with -- the evaluated VALUE (checkFormatString
// discards it) -- and even then it would only work at phase 2, since at phase
// 1 a symbol-dependent expression has no value to inspect. That is a real
// design change with a real cost, not a patch: the reviewer's own suggestion
// (re-running validateRangeListValues on the RESOLVED space alongside
// submit.go's validateParameterSpaceLimits) would close shapes 2 and 3 and
// the PATH-empty gap together, and it belongs in a change that owns that
// decision, gated correctly -- validateParameterSpaceLimits sits behind
// Submitter.enforceLimits, which is the wrong gate for a structural check.
// Recorded here so the next reader inherits the ruling rather than
// rediscovering the symptom.
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
		resolved, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil)
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
			resolved, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, boundParams)
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

	// Shape 2, LONE TEXT ARM. Both bodies are lone {{...}} expressions whose
	// result is a perfectly good string -- section 1.3.12's RangeString arm --
	// carrying text that is not valid <IntRangeExpr> syntax. The checker sees
	// a string, which is exactly what its target admits; parseIntRangeExpr is
	// what sees the syntax. Base-spec range: "abc" is rejected at VALIDATE
	// with the identical message, which is what makes this a layer/pointer
	// difference rather than a hole.
	for _, tc := range []struct{ body, wantErr string }{
		{body: "{{ 'abc' }}", wantErr: `invalid integer "abc"`},
		{body: "{{ '1-abc' }}", wantErr: `invalid range end "abc"`},
	} {
		t.Run("INT lone "+tc.body+": checker+resolver accept, expansion fails (base-spec-equivalent)", func(t *testing.T) {
			tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new(tc.body), nil)
			ps := tmpl.Steps[0].ParameterSpace

			if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
				t.Fatalf("checker unexpectedly rejected: %v", errs)
			}
			resolved, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil)
			if len(errs) != 0 {
				t.Fatalf("resolver unexpectedly rejected: %v", errs)
			}
			_, err := ExpandParameterSpace(resolved)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ExpandParameterSpace error = %v, want it to contain %q", err, tc.wantErr)
			}

			// The base-spec proof, run rather than asserted from memory: the
			// same text as a LITERAL range on a non-EXPR template is rejected
			// by validation with the same message, at the same field.
			lit := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new(strings.Trim(tc.body, "{} '")), nil)
			lit.Extensions = nil
			litErrs := ValidateWithOptions(lit, ValidateOptions{EnforceLimits: true})
			if !strings.Contains(litErrs.Error(), tc.wantErr) {
				t.Fatalf("base-spec literal validation = %v, want it to contain %q", litErrs, tc.wantErr)
			}
		})
	}

	// Shape 3, LONE EMPTY LIST -- the review's Minor 1, at every declared
	// type. An empty list is well-typed at every one of them, so no target
	// this checker could name would reject it; "range list is empty" is a
	// LENGTH check, and expand.go is where lengths are checked (base-spec
	// range: [] fails there too).
	for typ, wantErr := range map[TaskParamType]string{
		TaskParamTypeInt:    "range list is empty",
		TaskParamTypeFloat:  "range list is empty",
		TaskParamTypeString: "range list is empty",
		TaskParamTypePath:   "range list is empty",
		// CHUNK[INT] takes expandChunkInt's own path, which reports the empty
		// case in its own words rather than validateIntList's.
		TaskParamTypeChunkInt: "range produces no values",
	} {
		t.Run(string(typ)+` lone "{{ [] }}": checker+resolver accept, expansion fails (base-spec-equivalent)`, func(t *testing.T) {
			tmpl := buildRangeAgreementTemplate(typ, nil, new("{{ [] }}"), nil)
			ps := tmpl.Steps[0].ParameterSpace

			if errs := checkTemplateExpressions(tmpl, nil); len(errs) != 0 {
				t.Fatalf("checker unexpectedly rejected: %v", errs)
			}
			resolved, errs := ResolveParameterSpaceParams(tmpl, &tmpl.Steps[0], ps, nil)
			if len(errs) != 0 {
				t.Fatalf("resolver unexpectedly rejected: %v", errs)
			}
			def := resolved.TaskParameterDefinitions[0]
			if def.RangeExpr != nil || len(def.RangeList) != 0 {
				t.Fatalf("resolved def = %+v, want a cleared RangeExpr and an empty RangeList", def)
			}
			_, err := ExpandParameterSpace(resolved)
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("ExpandParameterSpace error = %v, want it to contain %q", err, wantErr)
			}
		})
	}
}
