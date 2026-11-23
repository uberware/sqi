// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

// TestOperationCount_Operators pins section 1.3.10's charges for the
// operators. The reasoning for each is beside its row in ops.go; in brief:
//
//   - String concatenation, and path "/" and "+", charge ResultBytes, not
//     ArgBytes on both operands: ceil(len/256) is not additive, and the
//     reference charges the produced length once. Unequal, over-256-byte
//     operands tell the two apart ('a'*300 + 'a'*10 costs 1 + ceil(310/256),
//     not 1 + 2 + 1).
//   - List concatenation charges ArgElements on both operands; element counts
//     are additive, so this equals the produced length. A range_expr operand
//     counts through rangeExprCount (range_expr("1-3") + range_expr("4-5")
//     costs 1 + 3 + 2 for the operator).
//   - List repetition charges ResultElements ([1,2] * 3 costs 1 + 6) and
//     string repetition ResultBytes.
//   - Scalar arithmetic, comparison, unary minus and "not" charge rule 1
//     only. The reference charges ordering and "not" one more, a flat +1
//     baselined in test/oracle/baseline-ops.txt. The list/list ordering row
//     charges the left operand's elements, as equality does, because
//     compareLists walks every element as listsEqual does; doc.go reads rule
//     2's enumeration as open.
//   - "in"/"not in" charge the container: rule 2's "contains()" is this
//     operator, named by its dunder (RFC 0005 lines 1445-1446). The range_expr
//     row charges too, because containsRangeInt expands the range in full.
//     See TestOperationCount_InOperator.
func TestOperationCount_Operators(t *testing.T) {
	tests := []struct {
		src  string
		want int64
		why  string
	}{
		{"1 + 2", 1, "rule 1 only: scalar arithmetic processes no list and no string"},
		{"[1,2] + [3]", 4, "rule 1 + rule 2 over both operands (2 + 1 elements)"},
		{"[1,2] * 3", 7, "rule 1 + rule 2 over the 6 produced elements"},
		{
			"'ab' + 'cd'", 2,
			"rule 1 + rule 3 over the 4-byte PRODUCED string only (ceil(4/256)=1); " +
				"not ArgBytes{0,1} (which would be 3) -- the reference charges the result's length, " +
				"not each operand's, whenever that differs from doing both",
		},
		{"'a' * 1000", 5, "rule 1 + rule 3 over the produced 1000 bytes = ceil(1000/256) = 4"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%q) = %d; want %d (%s)", tt.src, got, tt.want, tt.why)
			}
		})
	}
}

// TestOperationCount_PathOperators isolates joinPaths ("/") and
// appendToPath ("+") from any path()-construction charge by building the
// path Value directly and calling applyBinary, so the asserted count is the
// operator row's OWN Cost and nothing else's.
//
// Both rows charge ResultBytes, like string concatenation -- see
// TestOperationCount_Operators, and RFC 0006's own framing of a path as a
// string value under rule 3.
func TestOperationCount_PathOperators(t *testing.T) {
	t.Run("join, path/string", func(t *testing.T) {
		ec := testCtx()
		// "/a" (2 bytes) / "b"*300 (300 bytes) -> "/a/bbb...b" (303 bytes).
		// ceil(303/256) = 2. 1 (call) + 2 = 3.
		if _, err := applyBinary(ec, OpDiv, Path("/a", PathPOSIX), String(strings.Repeat("b", 300))); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		if ec.m.ops != 3 {
			t.Errorf("ops = %d; want 3 (1 call + ceil(303/256)=2 over the produced path)", ec.m.ops)
		}
	})

	t.Run("join, path/path", func(t *testing.T) {
		ec := testCtx()
		// "/a" / path("b"*300) -> same joined result, same 303 bytes.
		if _, err := applyBinary(ec, OpDiv, Path("/a", PathPOSIX), Path(strings.Repeat("b", 300), PathPOSIX)); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		if ec.m.ops != 3 {
			t.Errorf("ops = %d; want 3 (1 call + ceil(303/256)=2 over the produced path)", ec.m.ops)
		}
	})

	t.Run("append, path+string", func(t *testing.T) {
		ec := testCtx()
		// "/a" (2 bytes) + "x"*300 (300 bytes, no separator) -> 302 bytes.
		// ceil(302/256) = 2. 1 (call) + 2 = 3.
		if _, err := applyBinary(ec, OpAdd, Path("/a", PathPOSIX), String(strings.Repeat("x", 300))); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		if ec.m.ops != 3 {
			t.Errorf("ops = %d; want 3 (1 call + ceil(302/256)=2 over the produced path)", ec.m.ops)
		}
	})
}

// TestOperationCount_StringConcatenationBytes cross-checks the ResultBytes
// resolution against a bigger, unequal-length pair directly through
// applyBinary, independent of the parser round-trip TestOperationCount_Operators
// uses. 300 + 10 = 310 produced bytes, ceil(310/256) = 2.
func TestOperationCount_StringConcatenationBytes(t *testing.T) {
	ec := testCtx()
	if _, err := applyBinary(ec, OpAdd, String(strings.Repeat("a", 300)), String(strings.Repeat("b", 10))); err != nil {
		t.Fatalf("applyBinary: %v", err)
	}
	if ec.m.ops != 3 {
		t.Errorf("ops = %d; want 3 (1 call + ceil(310/256)=2 over the produced string)", ec.m.ops)
	}
}

// TestOperationCount_RangeExprConcatenation covers OpAdd's two other
// list/range-producing rows, both the same "list concatenation" operator
// section 1.3.10 rule 2 names,
// under signatures the generic list[T]+list[T1] row cannot admit (a bare
// range_expr operand). See TestOperationCount_Operators.
func TestOperationCount_RangeExprConcatenation(t *testing.T) {
	t.Run("range_expr + range_expr", func(t *testing.T) {
		ec := testCtx()
		l, err := RangeExpr("1-3")
		if err != nil {
			t.Fatalf("RangeExpr: %v", err)
		}
		r, err := RangeExpr("4-5")
		if err != nil {
			t.Fatalf("RangeExpr: %v", err)
		}
		if _, err := applyBinary(ec, OpAdd, l, r); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		// 1 (call) + 3 (range "1-3" has 3 elements) + 2 (range "4-5" has 2).
		if ec.m.ops != 6 {
			t.Errorf("ops = %d; want 6 (1 call + 3 + 2 elements)", ec.m.ops)
		}
	})

	t.Run("range_expr + list[int]", func(t *testing.T) {
		ec := testCtx()
		l, err := RangeExpr("1-3")
		if err != nil {
			t.Fatalf("RangeExpr: %v", err)
		}
		list := List(TInt, []Value{Int(4), Int(5)})
		if _, err := applyBinary(ec, OpAdd, l, list); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		// A bare range_expr operand cannot reach the generic list[T]+list[T1]
		// row, so this hits the concrete list[int]+list[int] row instead.
		// 1 (call) + 3 (range "1-3", coerced) + 2 (the list literal).
		if ec.m.ops != 6 {
			t.Errorf("ops = %d; want 6 (1 call + 3 + 2 elements)", ec.m.ops)
		}
	})
}

// TestOperationCount_InOperator pins that OpIn/OpNotIn's string/string and
// elem/list rows charge the container (Params[1] -- the
// item is Params[0], see the comment on OpIn in ops.go), because rule 2's
// "contains()" and rule 3's implicit coverage of "in" are the __contains__/
// __not_contains__ dunder this operator transforms to (RFC 0005 lines
// 1445-1446), not a functionShapes registry entry.
//
// The list and string rows charge Cost{ArgElements: []int{1}} /
// Cost{ArgBytes: []int{1}} on the container only, plus sqi's single rule-1
// call (callShape); the range_expr row charges its element count because
// containsRangeInt expands the range. The reference's totals run higher (its
// own +1, and for strings a charge on the combined needle and haystack); rules
// 2 and 3 scope the charge to what is iterated or scanned, the container, so
// the tests assert sqi's formula, not the reference's numbers.
func TestOperationCount_InOperator(t *testing.T) {
	t.Run("list container, 3 vs 10 elements discriminates from uncharged", func(t *testing.T) {
		ec := testCtx()
		small := List(TInt, []Value{Int(1), Int(2), Int(3)})
		if _, err := applyBinary(ec, OpIn, Int(1), small); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		// 1 (call) + 3 (container elements). An uncharged row would read 1.
		if ec.m.ops != 4 {
			t.Errorf("ops = %d; want 4 (1 call + 3 container elements)", ec.m.ops)
		}

		ec2 := testCtx()
		big := List(TInt, []Value{Int(1), Int(2), Int(3), Int(4), Int(5), Int(6), Int(7), Int(8), Int(9), Int(10)})
		if _, err := applyBinary(ec2, OpIn, Int(1), big); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		// 1 (call) + 10 (container elements): proves the charge scales, not
		// just that it's nonzero.
		if ec2.m.ops != 11 {
			t.Errorf("ops = %d; want 11 (1 call + 10 container elements)", ec2.m.ops)
		}
	})

	t.Run("not in, list container", func(t *testing.T) {
		ec := testCtx()
		list := List(TInt, []Value{Int(1), Int(2), Int(3), Int(4), Int(5)})
		if _, err := applyBinary(ec, OpNotIn, Int(9), list); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		if ec.m.ops != 6 {
			t.Errorf("ops = %d; want 6 (1 call + 5 container elements)", ec.m.ops)
		}
	})

	t.Run("string container, 10 vs 300 bytes crosses the 256 boundary", func(t *testing.T) {
		ec := testCtx()
		if _, err := applyBinary(ec, OpIn, String("z"), String(strings.Repeat("a", 10))); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		// 1 (call) + ceil(10/256) = 1. An uncharged row would also read 1 here
		// -- this case alone cannot discriminate, which is why the 300-byte
		// case below is required too (a <=256-byte-only test cannot tell a
		// charged row from an uncharged one).
		if ec.m.ops != 2 {
			t.Errorf("ops = %d; want 2 (1 call + ceil(10/256)=1 container byte unit)", ec.m.ops)
		}

		ec2 := testCtx()
		if _, err := applyBinary(ec2, OpIn, String("z"), String(strings.Repeat("a", 300))); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		// 1 (call) + ceil(300/256) = 2: proves the charge scales with the
		// container's byte length, crossing the 256-byte boundary.
		if ec2.m.ops != 3 {
			t.Errorf("ops = %d; want 3 (1 call + ceil(300/256)=2 container byte units)", ec2.m.ops)
		}
	})

	t.Run("not in, string container", func(t *testing.T) {
		ec := testCtx()
		if _, err := applyBinary(ec, OpNotIn, String("z"), String(strings.Repeat("a", 300))); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		if ec.m.ops != 3 {
			t.Errorf("ops = %d; want 3 (1 call + ceil(300/256)=2 container byte units)", ec.m.ops)
		}
	})

	// containsRangeInt calls rangeInts, which expands the range in full, so
	// the row scales with the container exactly as the string and list rows
	// above do. The reference's own flatness here stays baselined.
	t.Run("int/range_expr charges the container's element count", func(t *testing.T) {
		small, err := RangeExpr("1-3")
		if err != nil {
			t.Fatalf("RangeExpr: %v", err)
		}
		big, err := RangeExpr("1-1000")
		if err != nil {
			t.Fatalf("RangeExpr: %v", err)
		}
		for _, tt := range []struct {
			name string
			rng  Value
			want int64
		}{{"small", small, 4}, {"big", big, 1001}} {
			ec := testCtx()
			if _, err := applyBinary(ec, OpIn, Int(1), tt.rng); err != nil {
				t.Fatalf("applyBinary: %v", err)
			}
			if ec.m.ops != tt.want {
				t.Errorf("%s range: ops = %d; want %d (1 call + the range's element count)", tt.name, ec.m.ops, tt.want)
			}
		}
	})

	// "not in" shares the row's Cost, and the negation cannot short-circuit
	// the expansion either.
	t.Run("not in, int/range_expr charges the container's element count", func(t *testing.T) {
		r, err := RangeExpr("1-1000")
		if err != nil {
			t.Fatalf("RangeExpr: %v", err)
		}
		ec := testCtx()
		if _, err := applyBinary(ec, OpNotIn, Int(5000), r); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		if ec.m.ops != 1001 {
			t.Errorf("ops = %d; want 1001 (1 call + 1000 elements)", ec.m.ops)
		}
	})
}

// TestOperationCount_ListOrderingChargesItsWalk pins that an ordering
// comparison of two lists charges its elementwise walk, exactly as the
// equality comparison of the same operands does.
//
// Section 1.3.10 rule 2 names "list/range equality comparisons" by token and
// does not name ordering, but doc.go settles the enumeration as open: "<" and
// "==" run the same walk over the same operands (compareLists and valuesEqual
// are both elementwise), and charging only one would price the identical
// expression shape three orders of magnitude apart. Both are charged on the
// left operand.
func TestOperationCount_ListOrderingChargesItsWalk(t *testing.T) {
	three := func() Value { return List(TInt, []Value{Int(1), Int(2), Int(3)}) }
	for _, op := range []Op{OpLt, OpGt, OpLe, OpGe} {
		ec := testCtx()
		if _, err := applyBinary(ec, op, three(), three()); err != nil {
			t.Fatalf("applyBinary(%s): %v", op, err)
		}
		if ec.m.ops != 4 {
			t.Errorf("ops(%s) = %d; want 4 (1 call + 3 elements)", op, ec.m.ops)
		}
	}
	// The equality form of the same comparison agrees by construction, which
	// is the point.
	ec := testCtx()
	if _, err := applyBinary(ec, OpEq, three(), three()); err != nil {
		t.Fatalf("applyBinary(==): %v", err)
	}
	if ec.m.ops != 4 {
		t.Errorf("ops(==) = %d; want 4, matching the ordering rows exactly", ec.m.ops)
	}
	// Scalar ordering still charges rule 1 only: it iterates nothing.
	ec = testCtx()
	if _, err := applyBinary(ec, OpLt, Int(1), Int(2)); err != nil {
		t.Fatalf("applyBinary(<, scalars): %v", err)
	}
	if ec.m.ops != 1 {
		t.Errorf("ops(scalar <) = %d; want 1 (rule 1 only)", ec.m.ops)
	}
}

// TestOperationCount_ScalarRowsChargeNothing pins the negative space: a row
// whose operands are all scalars iterates no list and processes no string, so
// it charges rule 1 and nothing else, regardless of what the reference
// happens to report for it.
//
// Membership is by operand type: "not named by section 1.3.10 rule 2 or rule
// 3" is not a criterion, since doc.go settles the enumeration as open. The
// list/list ordering row and both OpIn/OpNotIn range_expr rows are unnamed
// but do O(receiver) work, so they are not scalar rows and they charge (see
// TestOperationCount_ListOrderingChargesItsWalk and
// TestOperationCount_InOperator).
func TestOperationCount_ScalarRowsChargeNothing(t *testing.T) {
	tests := []struct {
		name string
		run  func(ec evalCtx) error
	}{
		{"int arithmetic (+)", func(ec evalCtx) error { _, err := applyBinary(ec, OpAdd, Int(1), Int(2)); return err }},
		{"float arithmetic (-)", func(ec evalCtx) error { _, err := applyBinary(ec, OpSub, Float(1), Float(2)); return err }},
		{"int division", func(ec evalCtx) error { _, err := applyBinary(ec, OpDiv, Int(4), Int(2)); return err }},
		{"modulo", func(ec evalCtx) error { _, err := applyBinary(ec, OpMod, Int(4), Int(2)); return err }},
		{"power", func(ec evalCtx) error { _, err := applyBinary(ec, OpPow, Int(2), Int(3)); return err }},
		{"ordering, scalar int", func(ec evalCtx) error { _, err := applyBinary(ec, OpLt, Int(1), Int(2)); return err }},
		{"ordering, scalar string", func(ec evalCtx) error {
			_, err := applyBinary(ec, OpLt, String("a"), String("b"))
			return err
		}},
		{"ordering, scalar bool", func(ec evalCtx) error {
			_, err := applyBinary(ec, OpLt, Bool(false), Bool(true))
			return err
		}},
		{"unary negation", func(ec evalCtx) error { _, err := applyUnary(ec, OpNeg, Int(5)); return err }},
		{"unary plus", func(ec evalCtx) error { _, err := applyUnary(ec, OpPos, Int(5)); return err }},
		{"boolean not", func(ec evalCtx) error { _, err := applyUnary(ec, OpNot, Bool(true)); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ec := testCtx()
			if err := tt.run(ec); err != nil {
				t.Fatalf("op: %v", err)
			}
			if ec.m.ops != 1 {
				t.Errorf("ops = %d; want 1 (rule 1 only)", ec.m.ops)
			}
		})
	}
}
