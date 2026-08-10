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
			// NOT a range_expr-typed value: range() (funcslist.go) returns
			// list[int] DIRECTLY, so this case exercises the same list[int]
			// coercion path "INT list expression" above does, just reached via
			// a function call instead of a literal. A GENUINE range_expr value
			// (from range_expr(), which the language's own list(range_expr)
			// conversion and this package's evalRangeExprList both handle via
			// coerce.go's range_expr -> list[int] rule) is what section 2.1's
			// trap is actually about, and it is NOT exercised by this table at
			// all — see TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy,
			// below, which pins policy-divergent range_expr shapes this rename
			// exists to stop misrepresenting.
			name: "INT list[int] result (range() returns list[int] directly)",
			typ:  "INT", rangeExpr: "{{ range(1, 4) }}",
			want: []string{"1", "2", "3"},
		},
		{
			name: "PATH list expression",
			typ:  "PATH", rangeExpr: `{{ ["a//b", "c/./d", "e\\f"] }}`,
			// POSIX is expr.Eval's default path_format (see resolve.go's
			// package-level discipline: a server-side template must expand
			// identically whatever submitted it, never the host's native
			// flavor), and a string -> path coercion (coerceScalar's CodePath
			// case) is a bare re-tag with no normalization at all — so each
			// entry survives byte for byte: no "//" collapsing, no "."
			// resolution, and the backslash stays a literal character rather
			// than becoming a POSIX separator. Confirmed empirically before
			// writing this case.
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

// TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy is the
// design spec's section 2.1 trap, pinned directly rather than argued in
// prose: a GENUINE range_expr-typed value (from range_expr(), which — unlike
// range(), the "INT list[int] result" case above — really does produce
// expr.CodeRangeExpr) must expand under internal/openjd/expr's OWN policy
// (the zero intrange.Policy: start may exceed end, a step may be negative,
// and section 3.4.1.1.1's own worked table orders the result), never
// internal/openjd's stricter one (Policy{PositiveStepOnly, AscendingOnly},
// range.go's openjdRangePolicy) — see evalRangeExprList's doc comment for the
// full argument.
//
// All three rows discriminate the two policies: each is a case
// internal/openjd's own parseIntRangeExpr rejects or reorders differently —
// see the sibling test below,
// TestResolveParameterSpaceParams_LiteralIntRangeKeepsOpenJDPolicy, which
// pins the SAME three range texts, written as literal <IntRangeExpr>
// strings, still doing exactly that. If this function's production code were
// ever "simplified" to render the evaluated value with .String() and hand
// the resulting <IntRangeExpr> text back through RangeExpr — the exact
// mistake section 2.1 exists to prevent — every OTHER test in this file
// keeps passing (range(1,4)'s literal round-trip, "1-3", expands to [1,2,3]
// either way), but this test starts failing: "5-1" would be rejected instead
// of expanding to [5], "10-1:-2" would be rejected instead of expanding to
// [2,4,6,8,10], and "10-15:2,1-5" would come back first-seen
// ([10,12,14,1,2,3,4,5]) instead of increasing ([1,2,3,4,5,10,12,14].
// Verified by performing exactly that mutation against this task's own code,
// confirming this test fails, and reverting — see task-1-report.md's "Fix
// round 1" section.
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

			got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, nil)
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
// TestResolveParameterSpaceParams_RangeExprKeepsExpressionPolicy's sibling:
// the SAME three range texts, written as LITERAL <IntRangeExpr> strings (no
// {{...}} at all, so resolveRangeExprField's base-spec branch runs even
// though EXPR is declared on tmpl) must keep failing or reordering under
// internal/openjd's OWN stricter policy (range.go's parseIntRangeExpr,
// invoked from expand.go's expandTaskParam — NOT from this package's
// resolve.go, which never parses a literal range's semantics, only
// substitutes {{...}} references in its text). This is the proof that the
// two policies are genuinely two different code paths reached by two
// genuinely different inputs, not the same code path merely described twice.
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

			resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, nil)
			if len(errs) != 0 {
				t.Fatalf("unexpected resolve errors: %v", errs)
			}
			// The literal text must pass through resolve.go untouched — proof
			// EXPR being declared did not reroute it: RangeExpr still set to
			// the exact literal, RangeList still empty. The policy divergence
			// itself is enforced downstream, in ExpandParameterSpace.
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

// TestResolveParameterSpaceParams_WholeFieldExpressionMetered proves a
// whole-field range expression is evaluated under the same submission-time
// budget (submissionLimits(), exprcheck.go) as every other submit-time
// expression position, not expr.Eval's own looser unconfigured defaults.
// Dropping the metering options from evalRangeExprList's expr.Eval call
// leaves every OTHER test in this file passing (none of them evaluates
// anything expensive enough to trip a 10,000-operation/1MB budget) — this is
// the one test that would catch it, the same gap EXPR sub-project E3's own
// review found and fixed for checkLetBindings. Verified by dropping
// submissionLimits()... from the call, confirming this test fails, and
// restoring it — see task-1-report.md's "Fix round 1" section.
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

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, nil)
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
// NOT change the VALUE an ordinary (non-whole-field-expression) RangeExpr
// resolves to. A literal <IntRangeExpr> with a substitution embedded in
// surrounding text ("{{Param.Start}}-{{Param.End}}") is not a LONE
// reference, so it resolves to the SAME text
// TestResolveParameterSpaceParams's "RangeExpr resolved with Param" case
// (above) already pins for a non-EXPR template — this test pins the SAME
// input/output pair again with EXPR declared, proving the two branches agree
// on this shape's VALUE.
//
// As of EXPR sub-project E4b Task 2's checker/resolver-drift fix, this is NO
// LONGER the identical CODE PATH as the non-EXPR case: an EXPR-declared
// template now resolves this shape through resolveFormatStringExpr (each
// embedded reference evaluated as an expression, expr.TAny, rendered with
// Value.String()), not fmtstring.Resolve (each reference read only as a bare
// dotted-identifier lookup). Both a plain "Param.Start" lookup and an
// expr.TAny evaluation of the expression "Param.Start" produce the same text
// for a concrete int value, which is exactly what this test demonstrates —
// see TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedExpression,
// below, for a case (an actual operator, not a bare reference) the base-spec
// path cannot resolve at all, which is the whole point of the fix.
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
	if !strings.Contains(errs[0].Message, `"Param.Missing"`) {
		t.Errorf("errs[0].Message = %q, want it to name the unknown symbol %q", errs[0].Message, "Param.Missing")
	}
}

// TestResolveParameterSpaceParams_RangeListEntryExpressions is EXPR
// sub-project E4b Task 2's Step 1: an entry that is a lone {{expr}}, one with
// an expression embedded in surrounding text, and one per element type
// (FLOAT/STRING/PATH keep list[... | FormatString] per §1.3.12; INT/CHUNK[INT]
// may also carry a RangeList per TaskParamDefinition's own doc comment).
// Asserts each resolves to the right VALUE and the right RENDERING for its
// type — the FLOAT computed case ("5.0") pins the same shortest-round-trip
// rendering rule evalRangeExprList's doc comment establishes for the
// whole-field form, now proven for a per-entry expression too.
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
			// Bare re-tag, no normalization (coerceScalar's CodePath case,
			// same as evalRangeExprList's PATH case) — "a/b" survives byte
			// for byte, and the embedded form concatenates as plain text.
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

			got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, tc.jobParams)
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

// TestResolveParameterSpaceParams_RangeListEntryBaseSpecUnchanged is
// TestResolveParameterSpaceParams_BaseSpecUnchanged's sibling for the
// per-entry position: a template that does NOT declare EXPR must keep taking
// fmtstring.Resolve's exact path for a RangeList entry too, not a new path
// that happens to produce the same answer for simple references. The entry
// below ("{{ Param.Scale * 2 }}") is valid EXPR syntax but not a valid
// base-spec dotted-identifier reference.
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
		got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, jobParams)
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

// TestResolveParameterSpaceParams_RangeListEntryError proves an evaluation
// error in a RangeList entry expression is reported as a [openjd.ValidationError]
// at the entry's own pointer, matching the base-spec "unknown variable in
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

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, map[string]string{})
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

// TestResolveParameterSpaceParams_RangeListEntryMetered proves a per-entry
// expression is evaluated under the same submission-time budget
// (submissionLimits(), exprcheck.go) as every other submit-time expression
// position — the same gap TestResolveParameterSpaceParams_
// WholeFieldExpressionMetered (above) closed for the whole-field form,
// now closed for resolveFormatStringExpr's own new Eval call sites too.
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

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, nil)
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

// TestResolveParameterSpaceParams_NonLoneRangeExprEmbeddedExpression is EXPR
// sub-project E4b Task 2's plan amendment: with extensions: [EXPR], a
// non-lone whole-field RangeString may now contain an embedded expression,
// per §1.3.12 ("it is a format string, it can now contain an expression").
// Before this task, checkParameterSpaceExpressions (exprcheck.go) accepted
// this shape at both phase 1 and phase 2 (checkFormatString evaluates every
// segment as an expression, lone or not), while resolveRangeExprField routed
// it to base-spec fmtstring.Resolve, which rejects "Param.End * 2" outright
// as "not a valid dotted identifier" — a template that validated twice and
// then failed at submit naming a construct the extension itself allows.
//
// This test proves the checker and resolver now agree: the SAME template
// that passes checkParameterSpaceExpressions (proven indirectly — this is
// resolve.go's own package, not a submission end-to-end test, but
// checkFormatString's non-lone branch and this function's new
// resolveFormatStringExpr call are, by construction, the identical
// expr.TAny/Segments walk) now also resolves successfully here.
//
// It also proves the §2.1-adjacent claim in resolveRangeExprField's doc
// comment: the resolved text ("1-10") is handed to ExpandParameterSpace,
// which takes it through expand.go's parseIntRangeExpr — internal/openjd's
// OWN <IntRangeExpr> grammar reader — and it expands correctly. That is
// legitimate, not section 2.1's trap: the user wrote range SYNTAX with an
// expression embedded in one piece of it, not an expression that PRODUCES a
// range_expr value, so there is no range_expr VALUE here to mis-stringify
// and re-parse under a mismatched policy — only ordinary text, exactly as if
// the author had typed "1-10" by hand.
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

	resolved, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, map[string]string{"End": "5"})
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

// TestResolveParameterSpaceParams_NonLoneRangeExprError proves an evaluation
// error in a non-lone whole-field RangeExpr's embedded expression is
// reported as a [openjd.ValidationError] at the field's own pointer, matching
// the lone-expression case (TestResolveParameterSpaceParams_
// WholeFieldExpressionError, above) and the base-spec "unknown variable in
// RangeExpr" case.
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
	if !strings.Contains(errs[0].Message, `"Param.Missing"`) {
		t.Errorf("errs[0].Message = %q, want it to name the unknown symbol %q", errs[0].Message, "Param.Missing")
	}
}

// TestResolveParameterSpaceParams_NonLoneRangeExprMetered proves the new
// non-lone whole-field embedded-expression path is evaluated under
// submissionLimits() too — the third new expr.Eval call site this task
// introduces (per-entry lone, per-entry embedded, and this one all funnel
// through resolveFormatStringExpr, so this one test covers the shared
// function's metering, exercised via the whole-field position).
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

	got, errs := openjd.ResolveParameterSpaceParams(tmpl, ps, nil)
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
