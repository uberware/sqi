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
// text second. Every count below is for the expression EXACTLY as written,
// with any "*" evaluated IN-EXPRESSION and therefore carrying its own rule-3
// charge (so "z" in ("a"*10) is 2 for the repetition plus 3 for the "in",
// not 3 in total). That is the opposite convention from
// cost_misc_internal_test.go's probe, which pre-expands its strings in
// Python; read the two blocks accordingly:
//
//	 5  1 in [1,2,3]                        3  1 in [1]
//	12  1 in [1,2,3,4,5,6,7,8,9,10]          5  4 in [1,2,3]  (full length, no early exit)
//	 3  "a" in "a"                           5  "z" in ("a"*10)
//	 7  "z" in ("a"*300)                     2  "" in ""
//	 3  1 in range_expr("1-3")               3  1 in range_expr("1-100")
//
// CORRECTION (final whole-branch review, sub-project E1): the "z" in ("a"*10)
// entry was transcribed as 3 and is 5, re-measured live. It sat on the same
// printed line as an entry that DOES use the in-expression reading, so a
// reader comparing 3 -> 7 across the two would have inferred the wrong
// scaling shape from two different notations. No `want` value in this file
// was ever computed from the transcribed number -- they come from sqi's own
// formula -- so no assertion moved; the probe block exists to be auditable,
// which is reason enough to correct it.
//
// The list and string rows clearly SCALE with the container's size (3, 5, 12
// track container lengths 1, 3, 10 exactly; 5 vs 7 tracks byte length
// crossing a 256 boundary once the repetition's own charge is subtracted).
// The reference's range_expr row does NOT scale (3 regardless of range size)
// -- and sqi charges it anyway. That was the one row this probe originally
// left uncharged on the strength of the reference's flatness; the final
// whole-branch review overturned it, because sqi's own containsRangeInt
// expands the range in full via rangeInts and rule 2's text is therefore
// satisfied whatever the reference does. See ops.go's OpIn rows.
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

	// This subtest previously asserted the range_expr row "stays uncharged
	// regardless of range size", at a flat 1. The final whole-branch review
	// overturned that: containsRangeInt calls rangeInts, which expands the
	// range in full, so the row scales with the container exactly as the
	// string and list rows above do. The reference's own flatness here is
	// unchanged and stays baselined.
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
// The list/list ordering row was uncharged until the final whole-branch
// review, on the sole ground that section 1.3.10 rule 2 names "list/range
// equality comparisons" by token and does not name ordering. doc.go settles
// the enumeration as OPEN, which removes that ground: "<" and "==" run the
// same walk over the same operands (compareLists and valuesEqual are both
// elementwise), and charging only one produced a measured 1,666x discrepancy
// on the identical expression shape. Both are charged on the LEFT operand.
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
	// is the whole point of the correction.
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
// whose operands are all SCALARS iterates no list and processes no string, so
// it charges rule 1 and nothing else, regardless of what the reference
// happens to report for it.
//
// The membership test is what "scalar" means here, and it is the one the
// final whole-branch review substituted for the old one. This table
// previously admitted any row "NOT named by section 1.3.10 rule 2 or rule 3"
// and so also held the list/list ordering row and both OpIn/OpNotIn
// range_expr rows -- three rows that do O(receiver) work. All three now
// charge (see TestOperationCount_ListOrderingChargesItsWalk and
// TestOperationCount_InOperator), and "unnamed by the enumeration" is no
// longer a criterion for anything: doc.go settles the enumeration as OPEN.
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
