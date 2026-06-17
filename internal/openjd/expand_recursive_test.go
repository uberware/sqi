// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for the recursive OpenJD combination grammar:
//
//	<CombinationExpr> ::= <Element> | <CombinationExpr> '*' <Element>
//	<Element>         ::= <Identifier> | '(' <ExprList> ')'
//	<ExprList>        ::= <CombinationExpr> | <ExprList> ',' <CombinationExpr>
//
// i.e. an association group may contain a comma-separated list of full
// sub-expressions, which may themselves be products and nested groups.

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// strParam is a tiny helper to declare a STRING task parameter.
func strParam(name string, vals ...string) openjd.TaskParamDefinition {
	return openjd.TaskParamDefinition{
		Name:      name,
		Type:      openjd.TaskParamTypeString,
		RangeList: vals,
	}
}

func TestExpand_ProductInAssociation(t *testing.T) {
	// (A*B, C): A*B yields 4 rows; C must yield 4 to zip.
	comb := "(A*B, C)"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			strParam("A", "a1", "a2"),
			strParam("B", "b1", "b2"),
			strParam("C", "c0", "c1", "c2", "c3"),
		},
		Combination: &comb,
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []openjd.TaskParams{
		{"A": "a1", "B": "b1", "C": "c0"},
		{"A": "a1", "B": "b2", "C": "c1"},
		{"A": "a2", "B": "b1", "C": "c2"},
		{"A": "a2", "B": "b2", "C": "c3"},
	}
	assertRows(t, want, rows)
}

func TestExpand_NestedAssociation(t *testing.T) {
	// ((A,B), C): inner (A,B) zips to 2 rows, then zips with C (2 rows).
	comb := "((A,B), C)"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			strParam("A", "a1", "a2"),
			strParam("B", "b1", "b2"),
			strParam("C", "c1", "c2"),
		},
		Combination: &comb,
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []openjd.TaskParams{
		{"A": "a1", "B": "b1", "C": "c1"},
		{"A": "a2", "B": "b2", "C": "c2"},
	}
	assertRows(t, want, rows)
}

func TestExpand_AssociationOfProductTimesProduct(t *testing.T) {
	// (A, B*C) * D
	comb := "(A, B*C) * D"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			strParam("A", "a1", "a2"),
			strParam("B", "b1"),
			strParam("C", "c1", "c2"),
			strParam("D", "d1", "d2"),
		},
		Combination: &comb,
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// (A, B*C): A=[a1,a2] zipped with B*C=[{b1,c1},{b1,c2}] → 2 rows.
	// Then * D → 4 rows.
	want := []openjd.TaskParams{
		{"A": "a1", "B": "b1", "C": "c1", "D": "d1"},
		{"A": "a1", "B": "b1", "C": "c1", "D": "d2"},
		{"A": "a2", "B": "b1", "C": "c2", "D": "d1"},
		{"A": "a2", "B": "b1", "C": "c2", "D": "d2"},
	}
	assertRows(t, want, rows)
}

func TestExpand_ProductInAssociationLengthMismatch(t *testing.T) {
	// (A*B, C) where A*B yields 4 rows but C yields 3 → error.
	comb := "(A*B, C)"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			strParam("A", "a1", "a2"),
			strParam("B", "b1", "b2"),
			strParam("C", "c0", "c1", "c2"),
		},
		Combination: &comb,
	}
	if _, err := openjd.ExpandParameterSpace(ps); err == nil {
		t.Fatal("expected length-mismatch error for (A*B, C) with unequal row counts")
	}
}

func TestExpand_MalformedRecursiveExpressions(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"double star", "A**B"},
		{"trailing comma", "(A,)"},
		{"leading comma", "(,A)"},
		{"empty group", "()"},
		{"single element group", "(A)"},
		{"unclosed open paren", "(A"},
		{"stray close paren", "A)"},
		{"unclosed group with comma", "(A,B"},
		{"nested unbalanced", "((A,B), C"},
		{"leading star", "*A"},
		{"trailing star", "A*"},
		{"empty comma item product", "(A*, B)"},
	}
	defs := []openjd.TaskParamDefinition{
		strParam("A", "a1", "a2"),
		strParam("B", "b1", "b2"),
		strParam("C", "c1", "c2"),
		strParam("D", "d1", "d2"),
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr := tc.expr
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: defs,
				Combination:              &expr,
			}
			if _, err := openjd.ExpandParameterSpace(ps); err == nil {
				t.Fatalf("expected error for malformed expression %q, got nil", tc.expr)
			}
		})
	}
}

func TestExpand_AssocFlatLeftFactor(t *testing.T) {
	// (A,B)*C: flat association as the left factor of a product.
	// (A,B) zips A=[a1,a2] with B=[b1,b2] → 2 rows.
	// Then * C=[c1,c2] → 4 rows (Cartesian product).
	comb := "(A,B)*C"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			strParam("A", "a1", "a2"),
			strParam("B", "b1", "b2"),
			strParam("C", "c1", "c2"),
		},
		Combination: &comb,
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []openjd.TaskParams{
		{"A": "a1", "B": "b1", "C": "c1"},
		{"A": "a1", "B": "b1", "C": "c2"},
		{"A": "a2", "B": "b2", "C": "c1"},
		{"A": "a2", "B": "b2", "C": "c2"},
	}
	assertRows(t, want, rows)
}

// nestExpr builds a left-recursive association expression nested to depth N.
// Each level wraps the inner expression: ((…(A,B)…),B) × N.
//
//	depth 1 → "(A,B)"
//	depth 2 → "((A,B),B)"
//	depth 3 → "(((A,B),B),B)"
//
// Only two distinct parameters are needed: A and B.
func nestExpr(depth int) string {
	s := "(A,B)"
	for range depth - 1 {
		s = "(" + s + ",B)"
	}
	return s
}

func TestExpand_NestingDepthAtLimit(t *testing.T) {
	// At exactly depth 64 the parser must succeed (limit is > 64, so 64 is ok).
	comb := nestExpr(64)
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			strParam("A", "a1", "a2"),
			strParam("B", "b1", "b2"),
		},
		Combination: &comb,
	}
	if _, err := openjd.ExpandParameterSpace(ps); err != nil {
		t.Fatalf("expected success at depth 64, got error: %v", err)
	}
}

func TestExpand_NestingDepthBeyondLimit(t *testing.T) {
	// At depth 65 the parser must return the depth-limit error.
	comb := nestExpr(65)
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			strParam("A", "a1", "a2"),
			strParam("B", "b1", "b2"),
		},
		Combination: &comb,
	}
	_, err := openjd.ExpandParameterSpace(ps)
	if err == nil {
		t.Fatal("expected depth-limit error for 65-level nesting, got nil")
	}
	const wantMsg = "nested too deeply"
	if !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("expected error containing %q, got: %v", wantMsg, err)
	}
}

// assertRows compares two TaskParams slices for exact equality (order-sensitive).
func assertRows(t *testing.T, want, got []openjd.TaskParams) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row count: got %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("row %d key count: got %v, want %v", i, got[i], want[i])
		}
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Fatalf("row %d key %q: got %q, want %q\n full got: %v", i, k, got[i][k], v, got[i])
			}
		}
	}
}
