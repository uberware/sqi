// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

// PROBE (sub-project E1, Task 5), .venv-oracle/bin/python -c "..." against
// openjd-model 0.11.1 -- see the brief's Step 1 command. Pasted verbatim as
// evidence, per "the method every one of these four tasks follows": run
// something first, decide against the spec text second, and only ADOPT the
// reference's number where the spec text agrees with it.
//
// Brief's own command:
//
//	1  1 + 2
//	4  [1,2] + [3]
//	7  [1,2] * 3
//	2  'ab' + 'cd'
//	5  'a' * 1000
//	2  [1,2] == [1,2]
//	2  1 < 2
//	1  -1
//	2  not True
//	4  path("/a") / "b"
//
// Follow-up probes run to resolve the 'ab' + 'cd' ambiguity and to check
// whether the same question applies to path "/" and "+" (the brief's top
// table states ArgBytes for both, but does not mark them ambiguous the way
// it marks the string row -- these follow-ups show that claim does not
// survive contact with the reference and must not be taken on faith):
//
//	ArgBytes{0,1} predicts 'ab'+'cd' = 1(call) + ceil(2/256) + ceil(2/256) = 3.
//	ResultBytes predicts 'ab'+'cd' = 1(call) + ceil(4/256) = 2.
//	The reference reports 2 -- ResultBytes, not ArgBytes{0,1}.
//
//	Discriminating with unequal, over-256 operand lengths (ArgBytes{0,1} and
//	ResultBytes give the SAME answer whenever both operands are small, or
//	equal, or their sum doesn't cross a 256 boundary differently than each
//	operand alone does -- these do not):
//
//	     2  'a'*100 + 'a'*100   (total 200 bytes)   ArgBytes{0,1}=1+1+1=3  ResultBytes=1+1=2
//	     3  'a'*300 + 'a'*10    (total 310 bytes)   ArgBytes{0,1}=1+2+1=4  ResultBytes=1+2=3
//	     3  'a'*10 + 'a'*300    (total 310 bytes)   ArgBytes{0,1}=1+1+2=4  ResultBytes=1+2=3
//	     4  'a'*300 + 'a'*300   (total 600 bytes)   ArgBytes{0,1}=1+2+2=5  ResultBytes=1+3=4
//
//	In every case where the two hypotheses diverge, the reference matches
//	ResultBytes and not ArgBytes{0,1}. ResultBytes wins.
//
//	The same test applied to path "/" (joinPaths) and path "+" (appendToPath),
//	isolating each operator's own contribution by subtracting the measured
//	cost of its sub-expressions (path("/a") alone = 2; 'a'*N alone follows the
//	confirmed ResultBytes repetition formula; see the arithmetic inline):
//
//	     4  (path("/a") / "b")                         join alone: 4-2-0=2 (call+ceil(4/256)=1)
//	     8  (path("/a") / ("b"*300))                    join alone: 8-2-3=3 (call+ceil(303/256)=2)  ArgBytes{0,1} would need 4
//	    12  (path("/a"*300) / "b")                      join alone: 12-8-0=4 (call+ceil(602/256)=3) ArgBytes{0,1} would need 4 too (ties here)
//	    16  (path("/a"*300) / ("b"*300))                join alone: 16-11=5 (call+ceil(901/256)=4)  ArgBytes{0,1} would need 6 -- this is the case that breaks the tie
//	     4  (path("/a") + "b")                          append alone: matches the "/" row exactly
//	     8  (path("/a") + ("b"*300))                     append alone: matches
//	    12  (path("/a"*300) + "b")                       append alone: matches
//	    16  (path("/a"*300) + ("b"*300))                  append alone: matches
//
//	Both path rows are ResultBytes too. The brief's top-table "ArgBytes{0,1}"
//	entries for string+string and for path / and + are the same kind of
//	plausible-but-wrong first guess the brief explicitly calls out for the
//	'ab'+'cd' row -- it just didn't say so for these two. Measuring settles
//	it the same way in both cases.
//
// Rule 2 (list) has no equivalent ambiguity: unlike ceil(x/256), plain
// element counts are additive, so ArgElements{0,1} and a hypothetical
// "ResultElements" always agree for concatenation (len(a)+len(b) IS
// len(result)) and cannot be told apart by any probe. ArgElements{0,1} is
// used because it is what the brief specifies and what section 1.3.10's own
// wording most directly describes ("iterates through every element of A
// list" -- each operand is a list the evaluator iterates).
//
// range_expr + range_expr (concatRanges) and the range-admitting
// list[int] + list[int] row were not in the brief's table but are the same
// operator under different signatures, so they were probed too:
//
//	1  range_expr("1-3")                (baseline: a bare call, no extra charge)
//	8  range_expr("1-3") + range_expr("4-5")   op alone: 8-1-1=6 = 1(call)+3(elems)+2(elems)
//	7  range_expr("1-3") + [4,5]               op alone: 7-1-0=6 = 1(call)+3(elems)+2(elems)
//	0  [4,5]                             (list literal: no charge, like any literal)
//
// Both match ArgElements{0,1} exactly (element counts computed by
// elementCount, which reads a range_expr's count arithmetically via
// rangeExprCount without expanding it).
//
// Ordering and "not" report MORE than rule 1 alone from the reference
// (1 < 2 = 2, not True = 2), but section 1.3.10 rules 2 and 3 each end their
// operator list with an explicit, closed enumeration ("This applies to:
// ...") that does not name ordering or "not" anywhere -- not even under a
// dunder name (RFC 0005's own transform table gives ordering __lt__ etc. and
// "not" __not__, and neither appears in rule 2 or rule 3's text). Extending
// either rule to an operator the text does not name is not supported by the
// text, so these rows charge nothing beyond rule 1 here, and the reference's
// extra charge for them is left unexplained -- consistent with doc.go's
// ruling that the reference is Beta software with known counting defects the
// spec outranks.
//
// CORRECTION (review finding on this task): "in"/"not in" were originally
// grouped with ordering/"not" above on the same "not named by rule 2/3"
// reasoning, and that was wrong. Rule 2's own text names "contains()" --
// RFC 0005's dunder-transform table (lines 1445-1446) is explicit that
// "In -> __contains__, NotIn -> __not_contains__ ... x in y becomes
// __contains__(y, x)": "contains()" in rule 2's list IS the "in" operator,
// named by its dunder rather than its surface syntax, not a functionShapes
// registry entry (there is no function named "contains" anywhere in the
// spec or sqi's registry -- confirmed by `rg '"contains"'
// internal/openjd/expr/`). So the string and list rows of OpIn/OpNotIn DO
// charge; see TestOperationCount_InOperator below. Only the range_expr row
// stays uncharged, and for a different, evidence-based reason: the
// reference's own count does not scale with the range's size (see that
// test's probe comment), so nothing supports a per-range charge there.
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
				"resolved against ArgBytes{0,1} (which would be 3) by the discriminating " +
				"probes in the comment above -- the reference charges the result's length, " +
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
// Both rows charge ResultBytes, exactly like string concatenation -- see the
// probe comment on TestOperationCount_Operators for the measurements that
// settle it, and RFC 0006's own framing of a path as a string value under
// rule 3.
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
// list/range-producing rows, neither named directly in the brief's table but
// both the same "list concatenation" operator section 1.3.10 rule 2 names,
// under signatures the generic list[T]+list[T1] row cannot admit (a bare
// range_expr operand). See the probe comment on TestOperationCount_Operators.
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

// TestOperationCount_InOperator covers the review correction: OpIn/OpNotIn's
// string/string and elem/list rows charge the CONTAINER (Params[1] -- the
// item is Params[0], see the comment on OpIn in ops.go), because rule 2's
// "contains()" and rule 3's implicit coverage of "in" are the __contains__/
// __not_contains__ dunder this operator transforms to (RFC 0005 lines
// 1445-1446), not a functionShapes registry entry.
//
// PROBE, .venv-oracle/bin/python against openjd-model 0.11.1, following the
// same method as the top-of-file probe -- run first, decide against the spec
// text second:
//
//	 5  1 in [1,2,3]                        3  1 in [1]
//	12  1 in [1,2,3,4,5,6,7,8,9,10]          5  4 in [1,2,3]  (full length, no early exit)
//	 3  "a" in "a"                           3  "z" in ("a"*10)
//	 7  "z" in ("a"*300)                     2  "" in ""
//	 3  1 in range_expr("1-3")               3  1 in range_expr("1-100")
//
// The list and string rows clearly SCALE with the container's size (3, 5, 12
// track container lengths 1, 3, 10 exactly; 3 vs 7 tracks byte length
// crossing a 256 boundary); the range_expr row does not (3 regardless of
// range size) -- this is the evidence for charging the first two and not the
// third.
//
// The reference's own absolute totals are NOT reproduced exactly and are not
// the target: they run a flat 1-2 operations higher than 1(call)+N predicts
// (e.g. "1 in [1]" measures 3, not 1+1=2; isolating the string rows further
// suggests the reference charges on the COMBINED needle+haystack length, not
// the container alone -- `("a"*300) in ("b"*300)` isolates to 5, not
// 1+ceil(300/256)=3). That combined-length or extra-call behavior is not
// what rule 2/3's text asks for (rule 2: "the number of elements is added",
// scoped to what is iterated -- the container, not the constant-size probe
// item; rule 3 likewise), and doc.go's standing ruling is that the spec text
// outranks an unexplained reference count. Cost{ArgBytes:[]int{1}} /
// Cost{ArgElements:[]int{1}} on the container only, combined with sqi's own
// fixed one-call rule 1 (never two, see callShape), is what the spec text
// supports; the tests below assert against THAT formula, not against the
// reference's raw numbers.
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
		// 1 (call) + 10 (container elements): proves the charge SCALES, not
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
		// case below is required too (a <=256-byte-only test is exactly how
		// this was missed the first time).
		if ec.m.ops != 2 {
			t.Errorf("ops = %d; want 2 (1 call + ceil(10/256)=1 container byte unit)", ec.m.ops)
		}

		ec2 := testCtx()
		if _, err := applyBinary(ec2, OpIn, String("z"), String(strings.Repeat("a", 300))); err != nil {
			t.Fatalf("applyBinary: %v", err)
		}
		// 1 (call) + ceil(300/256) = 2: proves the charge SCALES with the
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

	t.Run("int/range_expr stays uncharged regardless of range size", func(t *testing.T) {
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
		}{{"small", small}, {"big", big}} {
			ec := testCtx()
			if _, err := applyBinary(ec, OpIn, Int(1), tt.rng); err != nil {
				t.Fatalf("applyBinary: %v", err)
			}
			if ec.m.ops != 1 {
				t.Errorf("%s range: ops = %d; want 1 (rule 1 only, no scaling with range size)", tt.name, ec.m.ops)
			}
		}
	})
}

// TestOperationCount_ScalarRowsChargeNothing pins the negative space: every
// row NOT named by section 1.3.10 rule 2 or rule 3 -- including, after the
// review correction above, only the range_expr row of OpIn/OpNotIn rather
// than all of them -- charges rule 1 and nothing else, regardless of what
// the reference happens to report for it (see the comment above
// TestOperationCount_Operators for why ordering and "not" are excluded on
// purpose rather than by oversight).
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
		{"ordering, list", func(ec evalCtx) error {
			a := List(TInt, []Value{Int(1), Int(2), Int(3)})
			b := List(TInt, []Value{Int(1), Int(2), Int(4)})
			_, err := applyBinary(ec, OpLt, a, b)
			return err
		}},
		{"in, int/range_expr (uncharged: does not scale with range size)", func(ec evalCtx) error {
			r, err := RangeExpr("1-3")
			if err != nil {
				return err
			}
			_, err = applyBinary(ec, OpIn, Int(1), r)
			return err
		}},
		{"not in, int/range_expr", func(ec evalCtx) error {
			r, err := RangeExpr("1-3")
			if err != nil {
				return err
			}
			_, err = applyBinary(ec, OpNotIn, Int(5), r)
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
