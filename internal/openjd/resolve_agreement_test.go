// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"slices"
	"strings"
	"testing"
)

// The checker/resolver agreement tests for parameter-space ranges.
//
// Conformance cannot judge section 1.3.12's extended range: the suite scores
// job_templates, which is parse-and-validate, and never expands a parameter
// space. The checker (checkParameterSpaceExpressions, exprcheck.go) and the
// resolver (ResolveParameterSpaceParams + ExpandParameterSpace,
// resolve.go/expand.go) could disagree about a range's validity without the
// score moving. For a set of range expressions, this file runs both the
// checker and the resolver plus expander against the same *JobTemplate and
// *StepParameterSpace, and checks both directions:
//
//   - a range the checker accepts resolves and expands without error;
//   - a range the checker rejects never reaches expansion, shown by the
//     resolver refusing it too, independent of production's call order
//     (checkExpressionsAtSubmit runs before ResolveParameterSpaceParams in
//     submit.go's Submit, so a checker rejection already stops it).
//
// checkTemplateExpressions is called directly, not through
// ValidateWithOptions, because it is what submit.go's checkExpressionsAtSubmit
// calls, with the same bound job-parameter map ResolveParameterSpaceParams
// receives. ValidateWithOptions calls it with params=nil (phase 1, every job
// parameter unresolved), which would not exercise the Param.<name>-dependent
// cases below.

// buildRangeAgreementTemplate builds a single-step EXPR-declaring template
// whose one task-parameter definition (type typ) carries either a whole-field
// RangeExpr (rangeExpr non-nil) or a single-entry RangeList (rangeList),
// mirroring exprcheck_test.go's rangeCheckTemplate. submit.go's Submit hands
// the same *JobTemplate to both checkTemplateExpressions and
// ResolveParameterSpaceParams (checkExpressionsAtSubmit, then per-step
// expandStepTaskParams).
func buildRangeAgreementTemplate(typ TaskParamType, rangeList []string, rangeExpr *string, jobParams []JobParameter) *JobTemplate {
	return buildRangeAgreementTemplateWithLet(typ, rangeList, rangeExpr, jobParams, nil)
}

// buildRangeAgreementTemplateWithLet is buildRangeAgreementTemplate with the
// step template's own let: block filled in (section 3.6.2 row 1: the names it
// binds are visible in parameterSpace).
//
// Without a Let field, a checker that sees a step's let names at the range
// position and a resolver that does not would agree on every row, while such
// a template validates at upload, passes the phase-2 re-check, and then fails
// in expandStepTaskParams with unknown symbol "base".
// TestRangeCheckerResolverAgreement_StepLet exercises all three range shapes
// through this builder.
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

// assertRowAccepted runs the checker, the resolver and the expander against
// tmpl's one task-parameter definition and fails t unless all three accept
// the range: the checker reports no error, the resolver reports no error, and
// the resolved space expands without error. It returns the resolved space and the expanded rows
// so callers that pin an exact value (order, count, canonical text) can
// assert further.
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

// assertRowRejected is assertRowAccepted's counterpart for the other
// direction: it fails t unless the checker reports an error and the resolver,
// run directly rather than behind the checker's verdict as in submit.go, also
// reports an error and returns a nil space, leaving nothing to hand
// ExpandParameterSpace.
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

	// The two layers must also report the same message. Separately chosen
	// targets would show up as, for example, "cannot be coerced to list[int]
	// | range_expr" at validation and "cannot be coerced to list[int]" at
	// submission for the same template. Both layers call
	// rangeExprFieldType/rangeExprElemType, so the message is the same. Only
	// the pointer differs: the checker walks from the template root, the
	// resolver from the step (submit.go prefixes "/steps/<i>").
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

// TestRangeCheckerResolverAgreement_INTWholeFieldRangeString pins that the
// INT whole-field target includes all four required union members.
//
// Section 1.3.12 leaves the INT row "(unchanged, but see RangeString note
// below)" and that note extends the RangeString with an expression evaluating
// to a range_expr or a list[int] "in addition to the original <IntRangeExpr>
// grammar". The conformance suite states the resulting target in
// EXPR/jobs/expr1.2.3--union-target-type.test.yaml:12,
// "INT task 'range': int | string | range_expr | list[int] (match-first)",
// and this table is that fixture's six range cases, each carried through to
// expanded task rows, which the job_templates conformance scoring cannot do.
//
// A target of only "list[int] | range_expr" would reject the first four rows
// at phase 1 (template upload, parameters unresolved) with
// "unresolved[string] cannot be coerced to list[int] | range_expr", so
// declaring EXPR would remove base-spec behavior at this field: the same
// template without extensions: [EXPR] expands correctly, and most of the
// presets in presets/sqi/*.yaml use the first row's shape,
// range: "{{Param.Frames}}" with a STRING Frames.
//
// Phase 1 is asserted separately: a checker that passed only phase 2 would
// still reject every such template at upload. Phase 1 acceptance depends on
// expr/coerce.go's coerceUnresolved.
func TestRangeCheckerResolverAgreement_INTWholeFieldRangeString(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		jobParams []JobParameter
		bound     map[string]string
		// wantRangeExpr is the text the resolver writes back into RangeExpr
		// (the int/string arms, which are range text); wantRangeList is the
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
			// RANGE_EXPR job-parameter type.
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
					// unresolved placeholder. Asserted on its own (see the doc
					// comment above).
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
					want := tc.wantRows
					if typ == TaskParamTypeChunkInt {
						// Default chunk size 1: each CONTIGUOUS chunk is "N-N".
						want = make([]string, len(tc.wantRows))
						for i, v := range tc.wantRows {
							want[i] = v + "-" + v
						}
					}
					if !slices.Equal(got, want) {
						t.Errorf("expanded values = %v, want %v", got, want)
					}
				})
			}
		})
	}
}

// expandedValues pulls the value of task parameter name out of every expanded
// row. A CHUNK[INT] definition groups its integers into chunks, so with
// chunks unset each row's value is a one-value CONTIGUOUS chunk, "N-N"; the
// caller derives that from the INT expectation column.
func expandedValues(rows []TaskParams, name string, _ TaskParamType) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r[name])
	}
	return out
}

// TestRangeCheckerResolverAgreement_StepLet pins that a step template's let:
// names, visible to the checker at the parameterSpace position (section
// 3.6.2 row 1, implemented in exprcheck.go), are visible to the resolver
// too, which must therefore be handed the step.
//
// With step let: ["base = 10"] and a resolver that is not handed the step,
// all three range shapes below would pass the checker and fail the resolver
// with unknown symbol "base": the template validates at upload, passes the
// phase-2 re-check, and then fails in expandStepTaskParams.
// buildRangeAgreementTemplate has no Let field, so this uses
// buildRangeAgreementTemplateWithLet.
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
// of the step-let agreement: passing the step in must not make the resolver
// accept more than the checker does. A name no let: binds is unknown at both
// layers, and a let: binding that cannot be evaluated is reported by both,
// the resolver at its /let/<i> pointer rather than as an "unknown symbol" on
// every range that referenced it.
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

// TestRangeCheckerResolverAgreement_PerType has one row per declared
// task-parameter type (INT, CHUNK[INT], FLOAT, STRING, PATH), each with an
// accepted and a rejected expression, at both positions section 1.3.12
// defines: the whole-field RangeExpr and a single RangeList entry.
//
// The accept/reject bodies mirror exprcheck_test.go's
// TestCheckParameterSpaceExpressions_RangeTargetTypes, which pins for the
// checker alone which shapes the per-type target
// (rangeExprElemType/rangeExprFieldType) accepts and rejects. Reusing the
// same bodies shows the same verdicts hold when the resolver and expander run
// on the same input.
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
			// A checker targeting TargetString would accept this and
			// expansion would fail with "invalid integer \"5.0\"". The
			// checker targets TInt, under which 5.0 is an exact float->int
			// coercion, so the checker accepts, the resolver renders the
			// canonical int text "5", and expansion succeeds.
			entryAccept: "{{ 5.0 }}",
			// 2.5 is not integral, so TInt's float->int coercion rejects it
			// (a string target would have accepted it).
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
			// A bool coerces to string but has no bool->float rule: TString
			// would accept this; TFloat does not.
			entryReject: "{{ true }}",
		},
		{
			name: "STRING", typ: TaskParamTypeString,
			wholeAccept: "{{ ['a', 'b'] }}",
			// Every scalar coerces to string, so STRING has no wrong
			// element type to reject at the whole field (exprcheck_test.go
			// leaves tc.wholeReject empty for this row). Its only rejection
			// at the field position is the wrong shape: a bare scalar.
			wholeReject: "{{ 5 }}",
			entryAccept: "{{ 'a' }}",
			// As for the whole field, nothing type-mismatches a STRING
			// entry, so its rejection is the wrong shape. A list of
			// strings is a legal list item (it flattens, §1.3.12); a list of
			// lists is not.
			entryReject: "{{ [['a', 'b']] }}",
		},
		{
			name: "PATH", typ: TaskParamTypePath,
			wholeAccept: "{{ [path('/a'), path('/b')] }}",
			// An int coerces to string, but only a string coerces to path
			// (section 1.2.3: path <- string only).
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

// TestRangeCheckerResolverAgreement_LoneRangeExprPolicy pins that a lone
// {{ range_expr(...) }} whole-field expression is section 1.3.12's extended
// form, evaluated as a value under internal/openjd/expr's policy (the zero
// intrange.Policy) and never re-parsed as literal <IntRangeExpr> text under
// internal/openjd's stricter one. Both texts below are cases where the two
// policies diverge. resolve_test.go's
// TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy and
// TestResolveParameterSpaceParams_LiteralIntRangeKeepsOpenJDPolicy pin the
// resolver alone; this test pins that the checker accepts the same lone
// expression and that the expanded result is the expr-policy answer:
//
//   - range_expr("5-1"): start > end is legal under expr's policy (section
//     3.4.1.1.1's formula collapses it to the single value [5]); the literal
//     text "5-1" is rejected under internal/openjd's policy ("must be ≤ end").
//   - range_expr("10-15:2,1-5"): expr's policy orders the result increasing
//     ([1,2,3,4,5,10,12,14]); the literal text orders first-seen
//     ([10,12,14,1,2,3,4,5]) under internal/openjd's policy.
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
				t.Errorf("RangeExpr = %q, want nil (cleared for a list result)", *def.RangeExpr)
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

// TestRangeCheckerResolverAgreement_NonLoneRangeString pins the non-lone
// RangeString case: with EXPR declared, range: "1-{{ Param.End * 2 }}" is
// section 1.3.2's ordinary format string, not section 1.3.12's whole-field
// list form (the reference is embedded in surrounding text, so
// fmtstring.LoneRef is false). It is accepted at both layers when the
// embedded expression evaluates, and rejected at both layers on an unknown
// symbol or a parse error, like any other EXPR format-string position.
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
// embedded range_expr() case: a non-lone whole-field RangeString whose
// embedded reference evaluates to a range_expr value. resolveRangeExprField's
// doc comment explains why the reorder is intended.
//
// {{ range_expr("10-15:2,1-5") }},7 composes to the literal text
// "1-5,10-14:2,7": Value.String() renders the embedded range_expr value in its
// canonical form (Template Schemas §3.4.1.1.1), and the composed result is
// ordinary base-spec range syntax, so it is parsed by internal/openjd's
// parseIntRangeExpr (first-seen order), not internal/openjd/expr's
// (increasing order), and the trailing 7 lands last. The embedded values keep
// the order TestRangeCheckerResolverAgreement_LoneRangeExprPolicy gives the
// same call as the whole field; only the trailing 7 shows the first-seen
// order.
func TestRangeCheckerResolverAgreement_EmbeddedRangeExprReorder(t *testing.T) {
	tmpl := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new(`{{ range_expr("10-15:2,1-5") }},7`), nil)
	resolved, rows := assertRowAccepted(t, tmpl, nil)

	def := resolved.TaskParameterDefinitions[0]
	if def.RangeExpr == nil || *def.RangeExpr != "1-5,10-14:2,7" {
		t.Fatalf("RangeExpr = %v, want \"1-5,10-14:2,7\"", def.RangeExpr)
	}
	want := []string{"1", "2", "3", "4", "5", "10", "12", "14", "7"}
	if len(rows) != len(want) {
		t.Fatalf("expanded %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if got := rows[i]["P"]; got != w {
			t.Errorf("rows[%d][P] = %q, want %q", i, got, w)
		}
	}
}

// TestRangeCheckerResolverAgreement_KnownNonLoneDivergences pins the known,
// accepted exceptions to the accept-implies-expand-succeeds property the rest
// of this file tests. They are not agreement failures and do not go through
// assertRowAccepted/assertRowRejected.
//
// They are not limited to the non-lone position: lone range: "{{ [] }}" is
// accepted by both layers at every type and then fails expansion with
// "range list is empty". One rule covers every case in this function: the
// checker judges an expression's type, never the syntax, length or value of
// the range text or range list that expression produces. Three shapes follow
// from it, all pinned below:
//
//  1. Non-lone composition. checkFormatString (exprcheck.go) and
//     resolveFormatStringExpr (resolve.go) both evaluate a non-lone segment's
//     embedded reference at expr.TAny, so the declared element or field
//     target is unused for that segment on both sides (section 1.3.2: an
//     embedded reference renders to text regardless of its value's type).
//     "No error" means every embedded reference evaluated, not that the
//     composed text is valid range syntax for the parameter's type.
//  2. Lone, text arm. The INT/CHUNK[INT] whole-field target is section
//     1.3.12's "int | string | range_expr | list[int]" (rangeExprFieldType),
//     so a lone expression may produce range text, and the checker judges
//     only that the result is an int or a string, not that the text parses
//     as <IntRangeExpr>. So range: "{{ 'abc' }}" type-checks and fails at
//     expansion.
//  3. Lone, empty list. An empty list is a well-typed
//     list[int]/list[float]/list[string]/list[path]; its length is what is
//     wrong, and length is not a type.
//
// The three are accepted because each is base-spec-equivalent: a template
// with the literal text the expression computes fails the same way with the
// same message. For shape 2, base-spec range: "abc" is rejected at validation
// with `range expression "abc": invalid integer "abc"`, and the EXPR form is
// rejected at expansion with the same `invalid integer "abc"` inside a
// SubmitValidationError. The difference is which layer reports it and which
// JSON pointer it carries; no malformed range reaches a task.
//
// Rejecting them earlier would need the checker to see the evaluated value,
// which checkFormatString discards, and would work only at phase 2, since at
// phase 1 a symbol-dependent expression has no value. Re-running
// validateRangeListValues on the resolved space alongside submit.go's
// validateParameterSpaceLimits would cover shapes 2 and 3 and the PATH-empty
// case together, but needs its own gate: validateParameterSpaceLimits sits
// behind Submitter.enforceLimits, which is the wrong gate for a structural
// check.
func TestRangeCheckerResolverAgreement_KnownNonLoneDivergences(t *testing.T) {
	// INT entry "x{{ 2.5 }}": the "x" prefix makes this non-lone, so the
	// embedded 2.5 is rendered with Value.String() ("2.5") and concatenated,
	// giving RangeList entry "x2.5", which validateIntList rejects as it
	// would a base-spec literal range: ["x2.5"].
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
	// resolves and is assigned to RangeExpr, but expand.go's expandTaskParam
	// reads RangeList, never RangeExpr, for these three types (only
	// INT/CHUNK[INT] consult RangeExpr at expansion). A
	// base-spec literal range: "1-3" on a FLOAT/STRING/PATH parameter sets
	// RangeExpr the same way and fails identically.
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

	// Shape 2, lone text arm. Both bodies are lone {{...}} expressions whose
	// result is a string (section 1.3.12's RangeString arm) carrying text
	// that is not valid <IntRangeExpr> syntax. The checker sees a string,
	// which its target admits; parseIntRangeExpr checks the syntax.
	// Base-spec range: "abc" is rejected at validation with the same
	// message, so only the layer and pointer differ.
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

			// The same text as a literal range on a non-EXPR template is
			// rejected by validation with the same message, at the same
			// field.
			lit := buildRangeAgreementTemplate(TaskParamTypeInt, nil, new(strings.Trim(tc.body, "{} '")), nil)
			lit.Extensions = nil
			litErrs := ValidateWithOptions(lit, ValidateOptions{EnforceLimits: true})
			if !strings.Contains(litErrs.Error(), tc.wantErr) {
				t.Fatalf("base-spec literal validation = %v, want it to contain %q", litErrs, tc.wantErr)
			}
		})
	}

	// Shape 3, lone empty list, at every declared type. An empty list is
	// well-typed at each of them, so no target the checker could name would
	// reject it; "range list is empty" is a length check, done in expand.go.
	//
	// Base-spec range: [] does not fail at expansion: a literal range: [] is
	// rejected at validation with "required" (validate.go's
	// validateTaskParamRangeAndChunks) and never reaches expand.go. Base-spec
	// rejects it earlier; the shared property is only that both refuse.
	for typ, wantErr := range map[TaskParamType]string{
		TaskParamTypeInt:    "range list is empty",
		TaskParamTypeFloat:  "range list is empty",
		TaskParamTypeString: "range list is empty",
		TaskParamTypePath:   "range list is empty",
		// CHUNK[INT] takes expandChunkInt's path, which reports the empty
		// case in its own words.
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
