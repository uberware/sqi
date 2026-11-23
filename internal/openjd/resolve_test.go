// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestResolveParameterSpaceParams_NilInput(t *testing.T) {
	got, errs := openjd.ResolveParameterSpaceParams(nil, nil, nil, map[string]string{"X": "1"})
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

			got, errs := openjd.ResolveParameterSpaceParams(nil, nil, c.ps, c.jobParams)

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

	got, errs := openjd.ResolveParameterSpaceParams(nil, nil, ps, map[string]string{"X": "1"})
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
// give symbolsFor (exprcheck.go, building the phase-2 ScopeJob symbol table)
// something to bind Param.<name>/RawParam.<name> against. Unlike the
// base-spec fmtstring.MapScope built directly from the jobParams map, the
// EXPR path binds symbols only for parameters the template declares, as in a
// real submission (Submitter.prepareTemplate's BindJobParameters output
// contains only declared names), so every EXPR test below must declare its
// parameters as well as supply values for them.
func jobParamDefsOfType(jobParams map[string]string, typ openjd.JobParamType) []openjd.JobParameter {
	defs := make([]openjd.JobParameter, 0, len(jobParams))
	for name := range jobParams {
		defs = append(defs, openjd.JobParameter{Name: name, Type: typ})
	}
	return defs
}

// TestResolveParameterSpaceParams_WholeFieldListExpression covers section
// 1.3.12's extended range field: a whole-field range that is a lone {{...}}
// expression evaluating to a list (or, for INT, a range_expr) becomes
// RangeList, with RangeExpr cleared so expand.go's expandTaskParam takes the
// list branch.
//
// See evalRangeExprField's doc comment (resolve.go) for why the FLOAT case's
// want values are "5.0" and "3.0".
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
			// Not a range_expr value: range() (funcslist.go) returns
			// list[int], so this case takes the same list[int] coercion path
			// as "INT list expression" above, through a function call. A
			// range_expr value (from range_expr(), which list(range_expr) and
			// evalRangeExprField both handle via coerce.go's range_expr ->
			// list[int] rule) is not exercised by this table; see
			// TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy
			// below, which pins policy-divergent range_expr shapes.
			name: "INT list[int] result (range() returns list[int] directly)",
			typ:  "INT", rangeExpr: "{{ range(1, 4) }}",
			want: []string{"1", "2", "3"},
		},
		{
			name: "PATH list expression",
			typ:  "PATH", rangeExpr: `{{ ["a//b", "c/./d", "e\\f"] }}`,
			// POSIX is expr.Eval's default path_format (a server-side
			// template must expand identically whatever submitted it, so the
			// host's native flavor is never used), and a string -> path
			// coercion (coerceScalar's CodePath case) re-tags the value with
			// no normalization, so each entry survives byte for byte: no "//"
			// collapsing, no "." resolution, and the backslash stays a
			// literal character.
			want: []string{"a//b", "c/./d", "e\\f"},
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

			got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, tc.jobParams)
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

// TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy pins that a
// range_expr value (from range_expr(), which, unlike range() in the "INT
// list[int] result" case above, produces expr.CodeRangeExpr) expands under
// internal/openjd/expr's policy (the zero intrange.Policy: start may exceed
// end, a step may be negative, and section 3.4.1.1.1's worked table orders
// the result), never internal/openjd's stricter one
// (Policy{PositiveStepOnly, AscendingOnly}, range.go's openjdRangePolicy).
// See evalRangeExprField's doc comment.
//
// Each row is a case internal/openjd's parseIntRangeExpr rejects or orders
// differently; TestResolveParameterSpaceParams_LiteralIntRangeKeepsOpenJDPolicy
// below pins the same three texts written as literal <IntRangeExpr> strings.
// If the resolver rendered the evaluated value with .String() and passed the
// text back through RangeExpr, the other tests in this file would still pass
// (range(1,4) round-trips as "1-3" and expands to [1,2,3] either way), but
// this one would fail: "5-1" and "10-1:-2" would be rejected instead of
// expanding to [5] and [2,4,6,8,10], and "10-15:2,1-5" would come back
// first-seen ([10,12,14,1,2,3,4,5]) instead of increasing
// ([1,2,3,4,5,10,12,14]).
func TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, call string
		want       []string
	}{
		{
			name: "start > end: legal under expr's policy, [start] per section 3.4.1.1.1",
			call: `range_expr("5-1")`,
			want: []string{"5"},
		},
		{
			name: "negative step: legal under expr's policy",
			call: `range_expr("10-1:-2")`,
			want: []string{"2", "4", "6", "8", "10"},
		},
		{
			name: "overlapping sub-ranges: expr's policy orders INCREASING",
			call: `range_expr("10-15:2,1-5")`,
			want: []string{"1", "2", "3", "4", "5", "10", "12", "14"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "P",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{ " + tc.call + " }}"),
					},
				},
			}

			got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, nil)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			def := got.TaskParameterDefinitions[0]
			if def.RangeExpr != nil {
				t.Errorf("RangeExpr = %q, want nil", *def.RangeExpr)
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

// TestResolveParameterSpaceParams_LiteralIntRangeKeepsOpenJDPolicy is
// TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy's
// counterpart: the same three range texts, written as literal <IntRangeExpr>
// strings (no {{...}}, so resolveRangeExprField's base-spec branch runs even
// though EXPR is declared on tmpl), fail or reorder under internal/openjd's
// stricter policy (range.go's parseIntRangeExpr, called from expand.go's
// expandTaskParam; resolve.go only substitutes {{...}} references in a
// literal range's text). Together the two tests show the two policies are
// separate code paths reached by different inputs.
func TestResolveParameterSpaceParams_LiteralIntRangeKeepsOpenJDPolicy(t *testing.T) {
	tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}

	for _, tc := range []struct {
		name, literal, wantErr string
		want                   []string
	}{
		{name: "start > end", literal: "5-1", wantErr: "must be ≤ end"},
		{name: "negative step", literal: "10-1:-2", wantErr: "must be a positive integer"},
		{
			name:    "overlapping sub-ranges: openjd orders FIRST-SEEN",
			literal: "10-15:2,1-5",
			want:    []string{"10", "12", "14", "1", "2", "3", "4", "5"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{Name: "P", Type: openjd.TaskParamTypeInt, RangeExpr: ptr(tc.literal)},
				},
			}

			resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, nil)
			if len(errs) != 0 {
				t.Fatalf("unexpected resolve errors: %v", errs)
			}
			// The literal text passes through resolve.go untouched although
			// EXPR is declared: RangeExpr holds the exact literal and
			// RangeList is empty. The policy difference is enforced
			// downstream, in ExpandParameterSpace.
			def := resolved.TaskParameterDefinitions[0]
			if def.RangeExpr == nil || *def.RangeExpr != tc.literal {
				t.Fatalf("RangeExpr = %v, want %q (a literal range must pass through resolve.go unresolved)", def.RangeExpr, tc.literal)
			}
			if len(def.RangeList) != 0 {
				t.Fatalf("RangeList = %v, want empty", def.RangeList)
			}

			rows, err := openjd.ExpandParameterSpace(resolved)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ExpandParameterSpace succeeded, want error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandParameterSpace: %v", err)
			}
			got := make([]string, len(rows))
			for i, row := range rows {
				got[i] = row["P"]
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expanded values = %v, want %v", got, tc.want)
			}
			for i, want := range tc.want {
				if got[i] != want {
					t.Errorf("expanded[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestResolveParameterSpaceParams_WholeFieldExpressionMetered pins that a
// whole-field range expression is evaluated under the same submission-time
// budget ([openjd.ExprLimits]'s evalOptions, exprlimits.go) as every other
// submit-time expression position, not expr.Eval's looser unconfigured
// defaults. No other test in this file evaluates anything expensive enough to
// trip a 10,000-operation/1MB budget, so this is the only one that fails if
// evalRangeExprField's expr.Eval call drops the metering options.
func TestResolveParameterSpaceParams_WholeFieldExpressionMetered(t *testing.T) {
	tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeInt,
				RangeExpr: ptr("{{ [x * 2 for x in range(1, 20000)] }}"),
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, nil)
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Message, "operation limit exceeded") {
		t.Errorf("errs[0].Message = %q, want it to contain %q", errs[0].Message, "operation limit exceeded")
	}
}

// TestResolveParameterSpaceParams_BaseSpecUnchanged pins that a template that
// does not declare EXPR takes fmtstring.Resolve's path.
//
// The range body below ("[Param.A, Param.B]") is valid EXPR syntax (the
// STRING case's body from TestResolveParameterSpaceParams_WholeFieldListExpression
// above) but not a valid base-spec {{...}} reference: fmtstring's grammar
// requires a dotted identifier
// ([A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*) and rejects brackets,
// commas and spaces. If resolveRangeExprField or its caller took the EXPR
// branch regardless of tmpl's declared extensions, this test would succeed
// with RangeList == []string{"x", "y"} instead of failing. The worker uses the
// same check at both of its call sites
// (internal/worker/executor/resolve_expr_test.go's
// TestExecutor_Dispatch_BaseSpec_ExpressionSyntaxStaysMalformed).
//
// A nil tmpl and non-nil templates that do not list "EXPR" are covered, and
// all must reject identically.
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
		got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, jobParams)
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

// TestResolveParameterSpaceParams_NonLoneRefStaysBaseSpecEvenWithEXPR pins
// the other half of resolveRangeExprField's contract: declaring EXPR does not
// change the value an ordinary (non-whole-field-expression) RangeExpr
// resolves to. A literal <IntRangeExpr> with substitutions embedded in
// surrounding text ("{{Param.Start}}-{{Param.End}}") is not a lone reference,
// so it resolves to the text TestResolveParameterSpaceParams's "RangeExpr
// resolved with Param" case pins for a non-EXPR template; this test pins the
// same input and output with EXPR declared.
//
// The code path differs from the non-EXPR case: an EXPR template resolves
// this shape through resolveFormatStringExpr (each embedded reference
// evaluated as an expression at expr.TAny and rendered with Value.String()),
// not fmtstring.Resolve (each reference read as a dotted-identifier lookup).
// Both produce the same text for a concrete int value. See
// TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedExpression below
// for a case with an operator, which the base-spec path cannot resolve.
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

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{"Start": "1", "End": "5"})
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

// TestResolveParameterSpaceParams_WholeFieldExpressionError pins that an
// evaluation error in a whole-field range expression is reported as a
// [openjd.ValidationError] at the field's pointer, like a base-spec malformed
// or unknown reference.
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

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{})
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Pointer, "/parameterSpace/taskParameterDefinitions/0/range") {
		t.Errorf("errs[0].Pointer = %q, want it to contain the range pointer", errs[0].Pointer)
	}
	if !strings.Contains(errs[0].Message, `"Param.Missing"`) {
		t.Errorf("errs[0].Message = %q, want it to name the unknown symbol %q", errs[0].Message, "Param.Missing")
	}
}

// TestResolveParameterSpaceParams_RangeListEntryExpressions covers RangeList
// entry expressions: an entry that is a lone {{expr}}, one with an
// expression embedded in surrounding text, and one per element type
// (FLOAT/STRING/PATH keep list[... | FormatString] per §1.3.12; INT/CHUNK[INT]
// may also carry a RangeList per TaskParamDefinition's doc comment). It
// asserts each entry's value and rendering in resolve.go's RangeList output,
// not what reaches a task's Parameters map: the computed FLOAT case's "5.0"
// does not survive to a worker. See
// TestResolveParameterSpaceParams_RangeListEntryFloatIsRenormalizedByExpand
// below for what the fully expanded parameter space (this function plus
// expand.go's ExpandParameterSpace) contains.
func TestResolveParameterSpaceParams_RangeListEntryExpressions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		typ       string
		jobType   openjd.JobParamType
		jobParams map[string]string
		entries   []string
		want      []string
	}{
		{
			name: "FLOAT: lone reference, lone expression, embedded, literal",
			typ:  "FLOAT", jobType: openjd.JobParamTypeFloat,
			jobParams: map[string]string{"Scale": "2.5"},
			entries:   []string{"{{ Param.Scale }}", "{{ Param.Scale * 2 }}", "x{{ Param.Scale }}y", "9.9"},
			want:      []string{"2.5", "5.0", "x2.5y", "9.9"},
		},
		{
			name: "STRING: lone reference, embedded, literal",
			typ:  "STRING", jobType: openjd.JobParamTypeString,
			jobParams: map[string]string{"Name": "bg"},
			entries:   []string{"{{ Param.Name }}", "layer-{{ Param.Name }}", "literal"},
			want:      []string{"bg", "layer-bg", "literal"},
		},
		{
			name: "PATH: lone reference, embedded",
			typ:  "PATH", jobType: openjd.JobParamTypePath,
			jobParams: map[string]string{"Dir": "a/b"},
			entries:   []string{"{{ Param.Dir }}", "{{ Param.Dir }}/file.txt"},
			// A re-tag with no normalization (coerceScalar's CodePath case,
			// as in evalRangeExprField's PATH case): "a/b" survives byte for
			// byte, and the embedded form concatenates as plain text.
			want: []string{"a/b", "a/b/file.txt"},
		},
		{
			name: "INT: lone reference, lone expression",
			typ:  "INT", jobType: openjd.JobParamTypeInt,
			jobParams: map[string]string{"N": "3"},
			entries:   []string{"{{ Param.N }}", "{{ Param.N + 1 }}"},
			want:      []string{"3", "4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &openjd.JobTemplate{
				Extensions:           []string{"EXPR"},
				ParameterDefinitions: jobParamDefsOfType(tc.jobParams, tc.jobType),
			}
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "P",
						Type:      openjd.TaskParamType(tc.typ),
						RangeList: tc.entries,
					},
				},
			}

			got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, tc.jobParams)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if got == nil {
				t.Fatal("expected non-nil output")
			}

			def := got.TaskParameterDefinitions[0]
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

// TestResolveParameterSpaceParams_RangeListEntryFloatIsRenormalizedByExpand
// pins what a worker receives for a computed FLOAT entry. The FLOAT case
// above asserts resolve.go's output ("5.0" for "{{ Param.Scale * 2 }}",
// Scale=2.5). expand.go's validateFloatList re-canonicalizes every FLOAT
// RangeList entry with strconv.FormatFloat(f, 'g', -1, 64), which drops the
// ".0" evalRangeExprField and resolveFormatStringExpr add. This test runs the
// same entry through ResolveParameterSpaceParams and ExpandParameterSpace and
// pins the value that reaches a task row: "5".
//
// So the string form of a computed float does not matter for a FLOAT range
// value, whether it comes from a whole-field list expression or a per-entry
// expression: expand.go normalizes it before it reaches a task's Parameters
// map, as it does for a literal (non-EXPR) FLOAT RangeList entry.
func TestResolveParameterSpaceParams_RangeListEntryFloatIsRenormalizedByExpand(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		Extensions:           []string{"EXPR"},
		ParameterDefinitions: []openjd.JobParameter{{Name: "Scale", Type: openjd.JobParamTypeFloat}},
	}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeFloat,
				RangeList: []string{"{{ Param.Scale * 2 }}"},
			},
		},
	}

	resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{"Scale": "2.5"})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// resolve.go's output, as the table test above pins.
	if got := resolved.TaskParameterDefinitions[0].RangeList[0]; got != "5.0" {
		t.Fatalf("resolve.go's own output = %q, want %q", got, "5.0")
	}

	rows, err := openjd.ExpandParameterSpace(resolved)
	if err != nil {
		t.Fatalf("ExpandParameterSpace: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expanded rows = %v, want exactly 1", rows)
	}
	if got := rows[0]["P"]; got != "5" {
		t.Errorf("what the worker actually receives = %q, want %q (validateFloatList's %%g re-canonicalization)", got, "5")
	}
}

// TestResolveParameterSpaceParams_RangeListEntryINTIntegralFloatAccepted pins
// that the checker and the resolver agree on an INT RangeList entry
// "{{ Param.Scale * 2 }}" with Scale=2.5. It evaluates to the float 5.0,
// which is exactly representable as an int. checkParameterSpaceExpressions
// targets TInt for this position, so the checker accepts it: float -> int is
// a legal §1.2.3 coercion and 5.0 is integral. If resolveRangeListEntry's
// loneTarget were TargetString instead of rangeExprElemType(def.Type), it
// would render "5.0", which expand.go's validateIntList rejects
// ("invalid integer \"5.0\""), so the checker would accept and expansion
// fail. resolveRangeListEntry performs the same float -> int coercion the
// checker accepted, producing the canonical int text "5", which
// ExpandParameterSpace accepts.
func TestResolveParameterSpaceParams_RangeListEntryINTIntegralFloatAccepted(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		Extensions:           []string{"EXPR"},
		ParameterDefinitions: []openjd.JobParameter{{Name: "Scale", Type: openjd.JobParamTypeFloat}},
	}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeInt,
				RangeList: []string{"{{ Param.Scale * 2 }}"},
			},
		},
	}

	resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{"Scale": "2.5"})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got := resolved.TaskParameterDefinitions[0].RangeList[0]; got != "5" {
		t.Fatalf("resolve.go's own output = %q, want %q (float->int coercion, not TargetString's float->string \"5.0\")", got, "5")
	}

	rows, err := openjd.ExpandParameterSpace(resolved)
	if err != nil {
		t.Fatalf("ExpandParameterSpace: %v -- the checker accepts this range under the TInt target, so expansion must too", err)
	}
	if len(rows) != 1 || rows[0]["P"] != "5" {
		t.Errorf("expanded rows = %v, want exactly one row with P=5", rows)
	}
}

// TestResolveParameterSpaceParams_RangeListEntryINTNonIntegralRejected is the
// other direction of
// TestResolveParameterSpaceParams_RangeListEntryINTIntegralFloatAccepted: a
// non-integral float entry is rejected by the resolver too,
// matching checkParameterSpaceExpressions's TInt target
// (TestCheckParameterSpaceExpressions_RangeTargetTypes's INT "entry rejected
// (wrong scalar type)" row, exprcheck_test.go). A TargetString target would
// accept any scalar and render "2.5", a value validateIntList rejects only
// later, at ExpandParameterSpace, which this test does not call. A range the
// checker rejects must never reach expansion; the resolver refusing it at the
// entry's pointer makes that hold whether or not a caller checks first.
func TestResolveParameterSpaceParams_RangeListEntryINTNonIntegralRejected(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		Extensions:           []string{"EXPR"},
		ParameterDefinitions: []openjd.JobParameter{{Name: "Scale", Type: openjd.JobParamTypeFloat}},
	}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeInt,
				RangeList: []string{"{{ Param.Scale }}"},
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{"Scale": "2.5"})
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Pointer, "/parameterSpace/taskParameterDefinitions/0/range/0") {
		t.Errorf("errs[0].Pointer = %q, want the entry pointer", errs[0].Pointer)
	}
	if !strings.Contains(errs[0].Message, "cannot be represented exactly as an int") {
		t.Errorf("errs[0].Message = %q, want the float->int coercion error", errs[0].Message)
	}
}

// TestResolveParameterSpaceParams_RangeListEntryElementTypeRejections is the
// resolver-side counterpart of TestCheckParameterSpaceExpressions_RangeTargetTypes'
// "entry rejected (wrong scalar type)" table (exprcheck_test.go) for FLOAT
// and PATH entries (INT/CHUNK[INT] has the pair of tests above): a value a
// string target would accept but rangeExprElemType(typ) does not is rejected
// by the resolver at the entry's pointer, so it never reaches expansion.
func TestResolveParameterSpaceParams_RangeListEntryElementTypeRejections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		typ   openjd.TaskParamType
		entry string
	}{
		// A bool coerces to string (section 1.2.3) but has no bool->float
		// rule.
		{name: "FLOAT rejects bool", typ: openjd.TaskParamTypeFloat, entry: "{{ true }}"},
		// An int coerces to string, but only a string coerces to path
		// (section 1.2.3: path <- string only).
		{name: "PATH rejects int", typ: openjd.TaskParamTypePath, entry: "{{ 5 }}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{Name: "P", Type: tc.typ, RangeList: []string{tc.entry}},
				},
			}

			got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{})
			if got != nil {
				t.Errorf("expected nil output on error, got %v", got)
			}
			if len(errs) != 1 {
				t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
			}
			if !strings.Contains(errs[0].Pointer, "/parameterSpace/taskParameterDefinitions/0/range/0") {
				t.Errorf("errs[0].Pointer = %q, want the entry pointer", errs[0].Pointer)
			}
		})
	}
}

// TestResolveParameterSpaceParams_RangeListEntryBaseSpecUnchanged is
// TestResolveParameterSpaceParams_BaseSpecUnchanged's counterpart for the
// per-entry position: a template that does not declare EXPR takes
// fmtstring.Resolve's path for a RangeList entry too. The entry below
// ("{{ Param.Scale * 2 }}") is valid EXPR syntax but not a valid base-spec
// dotted-identifier reference.
func TestResolveParameterSpaceParams_RangeListEntryBaseSpecUnchanged(t *testing.T) {
	jobParams := map[string]string{"Scale": "2.5"}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeFloat,
				RangeList: []string{"{{ Param.Scale * 2 }}"},
			},
		},
	}

	for _, tmpl := range []*openjd.JobTemplate{
		nil,
		{Extensions: []string{}},
		{Extensions: []string{"TASK_CHUNKING"}},
	} {
		got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, jobParams)
		if got != nil {
			t.Errorf("tmpl=%v: expected nil output on error, got %v", tmpl, got)
		}
		if len(errs) != 1 {
			t.Fatalf("tmpl=%v: expected 1 error, got %d: %v", tmpl, len(errs), errs)
		}
		if !strings.Contains(errs[0].Message, "not a valid dotted identifier") {
			t.Errorf("tmpl=%v: errs[0].Message = %q, want it to report a malformed dotted-identifier reference", tmpl, errs[0].Message)
		}
	}
}

// TestResolveParameterSpaceParams_RangeListEntryError pins that an evaluation
// error in a RangeList entry expression is reported as a [openjd.ValidationError]
// at the entry's pointer, matching the base-spec "unknown variable in
// RangeList entry" case above.
func TestResolveParameterSpaceParams_RangeListEntryError(t *testing.T) {
	tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeString,
				RangeList: []string{"{{ Param.Missing }}"},
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{})
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Pointer, "/parameterSpace/taskParameterDefinitions/0/range/0") {
		t.Errorf("errs[0].Pointer = %q, want it to contain the entry pointer", errs[0].Pointer)
	}
	if !strings.Contains(errs[0].Message, `"Param.Missing"`) {
		t.Errorf("errs[0].Message = %q, want it to name the unknown symbol %q", errs[0].Message, "Param.Missing")
	}
}

// TestResolveParameterSpaceParams_RangeListEntryMetered pins that a per-entry
// expression is evaluated under the same submission-time budget
// ([openjd.ExprLimits]'s evalOptions, exprlimits.go) as every other
// submit-time expression position. It covers resolveFormatStringExpr's Eval
// call sites, as TestResolveParameterSpaceParams_WholeFieldExpressionMetered
// covers the whole-field form.
func TestResolveParameterSpaceParams_RangeListEntryMetered(t *testing.T) {
	tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "P",
				Type:      openjd.TaskParamTypeInt,
				RangeList: []string{"{{ len([x * 2 for x in range(1, 20000)]) }}"},
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, nil)
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Message, "operation limit exceeded") {
		t.Errorf("errs[0].Message = %q, want it to contain %q", errs[0].Message, "operation limit exceeded")
	}
}

// TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedExpression pins
// that, with extensions: [EXPR], a non-lone whole-field RangeString may
// contain an embedded expression, per §1.3.12 ("it is a format string, it
// can now contain an expression"). checkParameterSpaceExpressions
// (exprcheck.go) accepts this shape at both phase 1 and phase 2
// (checkFormatString evaluates every segment as an expression, lone or not).
// If resolveRangeExprField routed it to base-spec fmtstring.Resolve, which
// rejects "Param.End * 2" as "not a valid dotted identifier", a template
// would validate twice and then fail at submission on a construct the
// extension allows.
//
// The template that passes checkParameterSpaceExpressions also resolves here:
// checkFormatString's non-lone branch and resolveFormatStringExpr perform the
// same expr.TAny/Segments walk.
//
// The resolved text ("1-10") is handed to ExpandParameterSpace, which parses
// it with parseIntRangeExpr, internal/openjd's <IntRangeExpr> reader, and it
// expands correctly (see resolveRangeExprField's doc comment). Parsing it
// there is correct: the author wrote range syntax with an expression embedded
// in one piece, not an expression that produces a range_expr value, so the
// result is ordinary text, as if "1-10" had been typed by hand.
func TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedExpression(t *testing.T) {
	tmpl := &openjd.JobTemplate{
		Extensions: []string{"EXPR"},
		ParameterDefinitions: []openjd.JobParameter{
			{Name: "End", Type: openjd.JobParamTypeInt},
		},
	}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "Frame",
				Type:      openjd.TaskParamTypeInt,
				RangeExpr: ptr("1-{{ Param.End * 2 }}"),
			},
		},
	}

	resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{"End": "5"})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved == nil {
		t.Fatal("expected non-nil output")
	}

	def := resolved.TaskParameterDefinitions[0]
	if def.RangeExpr == nil || *def.RangeExpr != "1-10" {
		t.Fatalf("RangeExpr = %v, want %q", def.RangeExpr, "1-10")
	}
	if len(def.RangeList) != 0 {
		t.Fatalf("RangeList = %v, want empty (this shape resolves through RangeExpr, not RangeList)", def.RangeList)
	}

	rows, err := openjd.ExpandParameterSpace(resolved)
	if err != nil {
		t.Fatalf("ExpandParameterSpace: %v", err)
	}
	got := make([]string, len(rows))
	for i, row := range rows {
		got[i] = row["Frame"]
	}
	want := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
	if len(got) != len(want) {
		t.Fatalf("expanded values = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("expanded[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestResolveParameterSpaceParams_NonLoneRangeExprError pins that an
// evaluation error in a non-lone whole-field RangeExpr's embedded expression
// is reported as a [openjd.ValidationError] at the field's pointer, matching
// the lone-expression case
// (TestResolveParameterSpaceParams_WholeFieldExpressionError) and the
// base-spec "unknown variable in RangeExpr" case.
func TestResolveParameterSpaceParams_NonLoneRangeExprError(t *testing.T) {
	tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "Frame",
				Type:      openjd.TaskParamTypeInt,
				RangeExpr: ptr("1-{{ Param.Missing }}"),
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, map[string]string{})
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Pointer, "/parameterSpace/taskParameterDefinitions/0/range") {
		t.Errorf("errs[0].Pointer = %q, want it to contain the range pointer", errs[0].Pointer)
	}
	if !strings.Contains(errs[0].Message, `"Param.Missing"`) {
		t.Errorf("errs[0].Message = %q, want it to name the unknown symbol %q", errs[0].Message, "Param.Missing")
	}
}

// TestResolveParameterSpaceParams_NonLoneRangeExprMetered pins that the
// non-lone whole-field embedded-expression path is evaluated with the same
// lim.evalOptions(). Per-entry lone, per-entry embedded and this path all go
// through resolveFormatStringExpr.
func TestResolveParameterSpaceParams_NonLoneRangeExprMetered(t *testing.T) {
	tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "Frame",
				Type:      openjd.TaskParamTypeInt,
				RangeExpr: ptr("1-{{ len([x * 2 for x in range(1, 20000)]) }}"),
			},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, nil)
	if got != nil {
		t.Errorf("expected nil output on error, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Message, "operation limit exceeded") {
		t.Errorf("errs[0].Message = %q, want it to contain %q", errs[0].Message, "operation limit exceeded")
	}
}

// TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedRangeExprValue
// pins what happens when an embedded reference in a non-lone whole-field
// RangeExpr evaluates to a range_expr value. Value.String()
// renders it in its canonical form (Template Schemas §3.4.1.1.1): ascending,
// positive steps, de-duplicated. So the composed text is ordinary base-spec
// range syntax that internal/openjd's stricter parser accepts, even when the
// expression was written with start > end or a negative step, which that
// parser would reject as literal text. Its first-seen order shows only
// through the literal that follows the embedded value.
//
// All three cases are an embedded range_expr(...) call followed by a literal
// ",<n>". fmtstring.LoneRef requires the whole field to be one reference, so
// the trailing literal makes each non-lone, and each takes the
// resolveFormatStringExpr path (which checkFormatString mirrors), not
// evalRangeExprField's lone whole-field path.
func TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedRangeExprValue(t *testing.T) {
	for _, tc := range []struct {
		name           string
		rangeExprText  string
		wantResolved   string
		wantExpandErr  string   // substring; empty means expansion must succeed
		wantExpandRows []string // checked only when wantExpandErr == ""
	}{
		{
			// range_expr("5-1") is the single value 5 (§3.4.1.1.1: the set
			// always holds its start), written "5".
			name:           "start > end: the embedded value is written as its one value",
			rangeExprText:  `{{ range_expr("5-1") }},7`,
			wantResolved:   "5,7",
			wantExpandRows: []string{"5", "7"},
		},
		{
			// A negative step names the same values as the positive one, and
			// the canonical form writes them ascending.
			name:           "negative step: the embedded value is written ascending",
			rangeExprText:  `{{ range_expr("10-1:-1") }},99`,
			wantResolved:   "1-10,99",
			wantExpandRows: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "99"},
		},
		{
			// The embedded value is written in ascending order, but the
			// composed text is still parsed in internal/openjd's first-seen
			// order, so the trailing 7 comes after 14.
			name:           "out-of-order elements: canonical within the value, first-seen across the text",
			rangeExprText:  `{{ range_expr("10-15:2,1-5") }},7`,
			wantResolved:   "1-5,10-14:2,7",
			wantExpandRows: []string{"1", "2", "3", "4", "5", "10", "12", "14", "7"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{Name: "Frame", Type: openjd.TaskParamTypeInt, RangeExpr: ptr(tc.rangeExprText)},
				},
			}

			resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, nil)
			if len(errs) != 0 {
				t.Fatalf("ResolveParameterSpaceParams: unexpected errors: %v", errs)
			}
			def := resolved.TaskParameterDefinitions[0]
			if def.RangeExpr == nil || *def.RangeExpr != tc.wantResolved {
				t.Fatalf("RangeExpr = %v, want %q", def.RangeExpr, tc.wantResolved)
			}
			if len(def.RangeList) != 0 {
				t.Fatalf("RangeList = %v, want empty", def.RangeList)
			}

			rows, err := openjd.ExpandParameterSpace(resolved)
			if tc.wantExpandErr != "" {
				if err == nil {
					t.Fatalf("ExpandParameterSpace succeeded, want error containing %q", tc.wantExpandErr)
				}
				if !strings.Contains(err.Error(), tc.wantExpandErr) {
					t.Errorf("ExpandParameterSpace error = %q, want it to contain %q", err.Error(), tc.wantExpandErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandParameterSpace: %v", err)
			}
			got := make([]string, len(rows))
			for i, row := range rows {
				got[i] = row["Frame"]
			}
			if len(got) != len(tc.wantExpandRows) {
				t.Fatalf("expanded values = %v, want %v", got, tc.wantExpandRows)
			}
			for i, want := range tc.wantExpandRows {
				if got[i] != want {
					t.Errorf("expanded[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestExpandParameterSpace_NonRangeTextIsRejected pins where a scalar that
// coerces into an INT range field but does not spell a range is rejected.
//
// The whole-field target for an INT parameter is
// "int | string | range_expr | list[int]". Under openjd-specifications#175's
// coercion rules bool's destination is string, the target offers string, and
// the checker accepts "{{ true }}", as it accepts "{{ 'abc' }}", which is not
// a range either.
//
// ResolveParameterSpaceParams substitutes the expression and leaves the text
// in RangeExpr without parsing it, so it accepts both. ExpandParameterSpace
// parses it, through range.go's parseIntRangeExpr. The checker rules on the
// type, the resolver substitutes the text, and expansion is the only place
// that parses it. This test asserts the expansion rejection for both scalars.
func TestExpandParameterSpace_NonRangeTextIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"a bool, which the coercion rules accept", "{{ true }}"},
		{"a string, which the checker has always accepted", "{{ 'abc' }}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &openjd.JobTemplate{Extensions: []string{"EXPR"}}
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{Name: "P", Type: openjd.TaskParamTypeInt, RangeExpr: ptr(tc.body)},
				},
			}
			resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, nil, ps, nil)
			if len(errs) != 0 {
				t.Fatalf("ResolveParameterSpaceParams(%q) = %v; want it to substitute and defer", tc.body, errs)
			}
			tasks, err := openjd.ExpandParameterSpace(resolved)
			if err == nil {
				t.Fatalf("ExpandParameterSpace(%q) = %v with no error; want the text rejected", tc.body, tasks)
			}
		})
	}
}
