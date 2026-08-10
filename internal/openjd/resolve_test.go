// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestResolveParameterSpaceParams_NilInput(t *testing.T) {
	got, errs := openjd.ResolveParameterSpaceParams(nil, nil, map[string]string{"X": "1"})
	if got != nil {
		t.Errorf("expected nil output for nil input, got %v", got)
	}
	if len(errs) != 0 {
		t.Errorf("expected no errors for nil input, got %v", errs)
	}
}

func TestResolveParameterSpaceParams(t *testing.T) {
	type tc struct {
		name      string
		ps        *openjd.StepParameterSpace
		jobParams map[string]string
		// expected outcomes:
		wantErrCount int
		wantErrPtr   []string // substring each error pointer must contain
		// if wantErrCount == 0:
		wantRangeExpr *string  // if non-nil, first def's RangeExpr after resolve
		wantRangeList []string // if non-nil, first def's RangeList after resolve
	}

	cases := []tc{
		{
			name: "RangeExpr resolved with Param",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Param.Start}}-{{Param.End}}"),
					},
				},
			},
			jobParams:     map[string]string{"Start": "1", "End": "5"},
			wantRangeExpr: ptr("1-5"),
		},
		{
			name: "RangeExpr resolved with RawParam",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{RawParam.Start}}-{{RawParam.End}}"),
					},
				},
			},
			jobParams:     map[string]string{"Start": "10", "End": "20"},
			wantRangeExpr: ptr("10-20"),
		},
		{
			name: "RangeList entries resolved",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Layer",
						Type:      openjd.TaskParamTypeString,
						RangeList: []string{"prefix-{{Param.X}}", "literal", "{{Param.X}}-suffix"},
					},
				},
			},
			jobParams:     map[string]string{"X": "bg"},
			wantRangeList: []string{"prefix-bg", "literal", "bg-suffix"},
		},
		{
			name: "no references — values unchanged",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("1-10"),
					},
				},
			},
			jobParams:     map[string]string{"Start": "1"},
			wantRangeExpr: ptr("1-10"),
		},
		{
			name: "unknown variable in RangeExpr",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Param.Nope}}-5"),
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 1,
			wantErrPtr:   []string{"/parameterSpace/taskParameterDefinitions/0/range"},
		},
		{
			name: "unknown variable Task.Param.X not available at submit",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Task.Param.X}}-5"),
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 1,
			wantErrPtr:   []string{"/parameterSpace/taskParameterDefinitions/0/range"},
		},
		{
			name: "unknown variable in RangeList entry",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Layer",
						Type:      openjd.TaskParamTypeString,
						RangeList: []string{"{{Param.Missing}}"},
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 1,
			wantErrPtr:   []string{"/parameterSpace/taskParameterDefinitions/0/range/0"},
		},
		{
			name: "multiple task params — errors accumulated",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Param.MissingA}}-5"),
					},
					{
						Name:      "Layer",
						Type:      openjd.TaskParamTypeString,
						RangeList: []string{"{{Param.MissingB}}"},
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 2,
			wantErrPtr: []string{
				"/parameterSpace/taskParameterDefinitions/0/range",
				"/parameterSpace/taskParameterDefinitions/1/range/0",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Snapshot the original RangeExpr pointer so we can assert no mutation.
			var originalRangeExpr *string
			if c.ps != nil && len(c.ps.TaskParameterDefinitions) > 0 {
				originalRangeExpr = c.ps.TaskParameterDefinitions[0].RangeExpr
			}

			got, errs := openjd.ResolveParameterSpaceParams(nil, c.ps, c.jobParams)

			// Error-path assertions.
			if c.wantErrCount > 0 {
				if len(errs) != c.wantErrCount {
					t.Errorf("expected %d errors, got %d: %v", c.wantErrCount, len(errs), errs)
				}
				for i, wantPtr := range c.wantErrPtr {
					if i >= len(errs) {
						t.Errorf("missing error[%d] with pointer %q", i, wantPtr)
						continue
					}
					if !strings.Contains(errs[i].Pointer, wantPtr) {
						t.Errorf("errs[%d].Pointer = %q, want it to contain %q", i, errs[i].Pointer, wantPtr)
					}
				}
				if got != nil {
					t.Errorf("expected nil output on error, got %v", got)
				}
				return
			}

			// Success-path assertions.
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if got == nil {
				t.Fatal("expected non-nil output")
			}

			if c.wantRangeExpr != nil {
				if len(got.TaskParameterDefinitions) == 0 {
					t.Fatal("got has no task parameter definitions")
				}
				def := got.TaskParameterDefinitions[0]
				if def.RangeExpr == nil {
					t.Errorf("RangeExpr is nil, want %q", *c.wantRangeExpr)
				} else if *def.RangeExpr != *c.wantRangeExpr {
					t.Errorf("RangeExpr = %q, want %q", *def.RangeExpr, *c.wantRangeExpr)
				}
			}

			if c.wantRangeList != nil {
				if len(got.TaskParameterDefinitions) == 0 {
					t.Fatal("got has no task parameter definitions")
				}
				def := got.TaskParameterDefinitions[0]
				if len(def.RangeList) != len(c.wantRangeList) {
					t.Errorf("RangeList len = %d, want %d", len(def.RangeList), len(c.wantRangeList))
				} else {
					for i, want := range c.wantRangeList {
						if def.RangeList[i] != want {
							t.Errorf("RangeList[%d] = %q, want %q", i, def.RangeList[i], want)
						}
					}
				}
			}

			// Assert input not mutated: if there was an original RangeExpr, the
			// pointer must still point to the same underlying string.
			if originalRangeExpr != nil && c.ps != nil && len(c.ps.TaskParameterDefinitions) > 0 {
				if c.ps.TaskParameterDefinitions[0].RangeExpr != originalRangeExpr {
					t.Error("input parameter space was mutated (RangeExpr pointer changed)")
				}
			}
		})
	}
}

func TestResolveParameterSpaceParams_NoMutation(t *testing.T) {
	// Verify that the returned *StepParameterSpace is a distinct object and
	// that modifying the original does not affect the result.
	original := "{{Param.X}}-5"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "Frame", Type: openjd.TaskParamTypeInt, RangeExpr: &original},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(nil, ps, map[string]string{"X": "1"})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got == ps {
		t.Error("ResolveParameterSpaceParams returned the same pointer — input was not copied")
	}
	// The original RangeExpr should still point to its unmodified value.
	if *ps.TaskParameterDefinitions[0].RangeExpr != original {
		t.Errorf("original RangeExpr was mutated: got %q", *ps.TaskParameterDefinitions[0].RangeExpr)
	}
	// The resolved result should have the substituted value.
	if got.TaskParameterDefinitions[0].RangeExpr == nil || *got.TaskParameterDefinitions[0].RangeExpr != "1-5" {
		t.Errorf("resolved RangeExpr = %v, want %q", got.TaskParameterDefinitions[0].RangeExpr, "1-5")
	}
}

// jobParamDefsOfType declares every name in jobParams as a job parameter of
// type typ, for building a *openjd.JobTemplate whose ParameterDefinitions
// give symbolsFor (resolve.go's phase-2 ScopeJob symbol table) something to
// bind Param.<name>/RawParam.<name> against. Unlike the base-spec
// fmtstring.MapScope built directly from the jobParams map, the EXPR-aware
// path only binds symbols for parameters the TEMPLATE declares — exactly the
// real submission contract (Submitter.prepareTemplate's BindJobParameters
// output only ever contains declared names), so every EXPR-declared test
// below must declare its parameters, not just supply values for them.
func jobParamDefsOfType(jobParams map[string]string, typ openjd.JobParamType) []openjd.JobParameter {
	defs := make([]openjd.JobParameter, 0, len(jobParams))
	for name := range jobParams {
		defs = append(defs, openjd.JobParameter{Name: name, Type: typ})
	}
	return defs
}

// TestResolveParameterSpaceParams_WholeFieldListExpression covers section
// 1.3.12's extended range field (design spec
// docs/superpowers/specs/2026-08-09-expr-server-substitution-design.md,
// section 2): a whole-field range that is a LONE {{...}} expression
// evaluating to a list (or, for INT, a range_expr) becomes RangeList, with
// RangeExpr cleared so expand.go's expandTaskParam takes the list branch.
//
// The FLOAT case's want values ("5.0", "3.0") are not a guess copied from the
// task brief — see evalRangeExprList's own doc comment (resolve.go) for the
// verification (spec citation + a throwaway empirical check) behind them.
func TestResolveParameterSpaceParams_WholeFieldListExpression(t *testing.T) {
	for _, tc := range []struct {
		name, typ, rangeExpr string
		jobParams            map[string]string
		want                 []string
	}{
		{
			name: "FLOAT list expression",
			typ:  "FLOAT", rangeExpr: "{{ [Param.Scale * 2, Param.Scale + 0.5] }}",
			jobParams: map[string]string{"Scale": "2.5"},
			want:      []string{"5.0", "3.0"},
		},
		{
			name: "STRING list expression",
			typ:  "STRING", rangeExpr: "{{ [Param.A, Param.B] }}",
			jobParams: map[string]string{"A": "x", "B": "y"},
			want:      []string{"x", "y"},
		},
		{
			name: "INT list expression",
			typ:  "INT", rangeExpr: "{{ [1, 2, 3] }}",
			want: []string{"1", "2", "3"},
		},
		{
			name: "INT range_expr result",
			typ:  "INT", rangeExpr: "{{ range(1, 4) }}",
			want: []string{"1", "2", "3"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &openjd.JobTemplate{
				Extensions:           []string{"EXPR"},
				ParameterDefinitions: jobParamDefsOfType(tc.jobParams, openjd.JobParamType(tc.typ)),
			}
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "P",
						Type:      openjd.TaskParamType(tc.typ),
						RangeExpr: ptr(tc.rangeExpr),
					},
				},
			}

			got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, tc.jobParams)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if got == nil {
				t.Fatal("expected non-nil output")
			}

			def := got.TaskParameterDefinitions[0]
			if def.RangeExpr != nil {
				t.Errorf("RangeExpr = %q, want nil (cleared once RangeList is populated)", *def.RangeExpr)
			}
			if len(def.RangeList) != len(tc.want) {
				t.Fatalf("RangeList = %v, want %v", def.RangeList, tc.want)
			}
			for i, want := range tc.want {
				if def.RangeList[i] != want {
					t.Errorf("RangeList[%d] = %q, want %q", i, def.RangeList[i], want)
				}
			}
		})
	}
}

// TestResolveParameterSpaceParams_BaseSpecUnchanged is this task's proof for
// the base-spec guarantee (task-1-brief.md's Step 4): a template that does
// NOT declare EXPR must take fmtstring.Resolve's exact path, byte for byte —
// not a new path that happens to produce the same answer.
//
// The range body below ("[Param.A, Param.B]") is valid EXPR syntax — it is
// exactly the STRING case's body from
// TestResolveParameterSpaceParams_WholeFieldListExpression above — but it is
// NOT a valid base-spec {{...}} reference: fmtstring's grammar requires a
// dotted identifier ([A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*) and
// rejects brackets, commas and spaces outright. If resolveRangeExprField (or
// its caller) were ever rerouted to the EXPR-aware branch regardless of
// tmpl's declared extensions, this test would wrongly SUCCEED with
// RangeList == []string{"x", "y"} instead of failing here — the same
// detector shape EXPR sub-project E4a's Task 6 used at both worker call
// sites (internal/worker/executor/resolve_expr_test.go's
// TestExecutor_Dispatch_BaseSpec_ExpressionSyntaxStaysMalformed).
//
// tmpl is nil, the common case (a step's parameter space is resolved without
// re-parsing its own template), AND its own sibling case, a non-nil tmpl that
// simply does not list "EXPR", is covered too — both must reject identically.
func TestResolveParameterSpaceParams_BaseSpecUnchanged(t *testing.T) {
	jobParams := map[string]string{"A": "x", "B": "y"}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeString,
				RangeExpr: ptr("{{ [Param.A, Param.B] }}"),
			},
		},
	}

	for _, tmpl := range []*openjd.JobTemplate{
		nil,
		{Extensions: []string{}},
		{Extensions: []string{"TASK_CHUNKING"}},
	} {
		got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, jobParams)
		if got != nil {
			t.Errorf("tmpl=%v: expected nil output on error, got %v", tmpl, got)
		}
		if len(errs) != 1 {
			t.Fatalf("tmpl=%v: expected 1 error, got %d: %v", tmpl, len(errs), errs)
		}
		if !strings.Contains(errs[0].Message, "not a valid dotted identifier") {
			t.Errorf("tmpl=%v: errs[0].Message = %q, want it to report a malformed dotted-identifier reference (proof the base-spec fmtstring.Resolve path ran, not EXPR evaluation)", tmpl, errs[0].Message)
		}
		if !strings.Contains(errs[0].Pointer, "/parameterSpace/taskParameterDefinitions/0/range") {
			t.Errorf("tmpl=%v: errs[0].Pointer = %q, want it to contain the range pointer", tmpl, errs[0].Pointer)
		}
	}
}

// TestResolveParameterSpaceParams_NonLoneRefStaysBaseSpecEvenWithEXPR proves
// the other half of resolveRangeExprField's contract: declaring EXPR does
// NOT change how an ordinary (non-whole-field-expression) RangeExpr
// resolves. A literal <IntRangeExpr> with a substitution embedded in
// surrounding text ("{{Param.Start}}-{{Param.End}}") is not a LONE
// reference, so it keeps taking fmtstring.Resolve exactly as
// TestResolveParameterSpaceParams's "RangeExpr resolved with Param" case
// (above) already pins for a non-EXPR template — this test pins the SAME
// input/output pair again with EXPR declared, proving the two branches agree
// on this shape rather than one silently reinterpreting it.
func TestResolveParameterSpaceParams_NonLoneRefStaysBaseSpecEvenWithEXPR(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		Extensions: []string{"EXPR"},
		ParameterDefinitions: []openjd.JobParameter{
			{Name: "Start", Type: openjd.JobParamTypeInt},
			{Name: "End", Type: openjd.JobParamTypeInt},
		},
	}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "Frame",
				Type:      openjd.TaskParamTypeInt,
				RangeExpr: ptr("{{Param.Start}}-{{Param.End}}"),
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, map[string]string{"Start": "1", "End": "5"})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	def := got.TaskParameterDefinitions[0]
	if def.RangeExpr == nil || *def.RangeExpr != "1-5" {
		t.Errorf("RangeExpr = %v, want %q", def.RangeExpr, "1-5")
	}
	if len(def.RangeList) != 0 {
		t.Errorf("RangeList = %v, want empty (this shape resolves through RangeExpr, not RangeList)", def.RangeList)
	}
}

// TestResolveParameterSpaceParams_WholeFieldExpressionError proves an
// evaluation error in a whole-field range expression is reported as a
// [openjd.ValidationError] at the field's own pointer, exactly like a
// base-spec malformed/unknown reference — not a panic, and not silently
// swallowed.
func TestResolveParameterSpaceParams_WholeFieldExpressionError(t *testing.T) {
	tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeInt,
				RangeExpr: ptr("{{ [Param.Missing] }}"),
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, map[string]string{})
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Pointer, "/parameterSpace/taskParameterDefinitions/0/range") {
		t.Errorf("errs[0].Pointer = %q, want it to contain the range pointer", errs[0].Pointer)
	}
}
