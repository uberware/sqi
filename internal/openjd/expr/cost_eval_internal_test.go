// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"testing"
)

func TestOperationCount_Comprehension(t *testing.T) {
	// Section 1.3.10's own worked example, and the reference agrees exactly:
	//   range(100) is 1 call + 100 iterations = 101
	//   the comprehension adds 100 iterations
	//   each x * 2 is 1 call             = 100
	//   total                            = 301
	if got := opsFor(t, "[x * 2 for x in range(100)]"); got != 301 {
		t.Errorf("ops = %d; want 301 (section 1.3.10's worked example)", got)
	}
	// An identity comprehension over a literal: 3 iterations, no body calls,
	// and a list literal charges nothing. The reference reports 3.
	if got := opsFor(t, "[x for x in [1,2,3]]"); got != 3 {
		t.Errorf("ops = %d; want 3", got)
	}
}

func TestOperationCount_SumRange(t *testing.T) {
	// Section 1.3.10's other worked example: sum(range(1000)) is 2002.
	if got := opsFor(t, "sum(range(1000))"); got != 2002 {
		t.Errorf("ops = %d; want 2002 (section 1.3.10's worked example)", got)
	}
}

func TestOperationCount_StringRepetition(t *testing.T) {
	// Section 1.3.10's third worked example. Its stated total of 393 is an
	// ARITHMETIC ERROR in the spec: ceil(100000/256) is 391, not 392, because
	// 256*390 = 99840 leaves 160. The rule is right and the total is wrong; the
	// reference reports 392 (1 call + 391) and so does sqi.
	if got := opsFor(t, "'a' * 100000"); got != 392 {
		t.Errorf("ops = %d; want 392 (1 call + ceil(100000/256) = 391)", got)
	}
}

func TestOperationCount_ListLiteralChargesNothing(t *testing.T) {
	// Pinned because "charges nothing" and "was forgotten" are
	// indistinguishable without a test. A literal is not a call and iterates no
	// existing list. The reference reports 0.
	if got := opsFor(t, "[1,2,3]"); got != 0 {
		t.Errorf("ops([1,2,3]) = %d; want 0", got)
	}
}

// PROBE (sub-project E1, Task 9), .venv-oracle/bin/python3 against
// openjd-model 0.11.1, binary-searching each expression's minimal
// operation_limit that still succeeds (evaluate_expression(expr,
// operation_limit=n) -- a limit that is too low fails with "operation limit
// exceeded", so bisecting on that finds the reference's exact count without
// reading any internal counter):
//
//	2  [1,2,3][0]
//	2  [1,2,3,4,5][0]
//	2  [1,2,3,4,5][4]
//	2  [1,2,3,4,5][2]
//	2  [1,2,3,4,5,6,7,8,9,10][0]
//	2  [1,2,3,4,5,6,7,8,9,10][9]
//	4  [1,2,3,4,5,6,7,8,9,10][0:2]
//	11 [1,2,3,4,5,6,7,8,9,10][0:9]
//	6  [1,2,3,4,5,6,7,8,9,10][5:9]
//	12 [1,2,3,4,5,6,7,8,9,10][:]
//	2  [1,2,3,4,5,6,7,8,9,10][3:3]
//
// Every list above is a flat pre-expanded literal -- ",".join(str(i) for i in
// range(1,11)) -- exactly the notation Task 7's postmortem requires, so the
// printed numbers reproduce verbatim with no shorthand to re-expand.
//
// Subscript is a CONSTANT 2 regardless of index or list length: not "iterate
// to the index" (which would grow with the index) and not "iterate the whole
// list" (which would grow with its length). Slice varies as
// 2 + len(result): the [0:2] row is 2+2=4, [0:9] is 2+9=11, [5:9] is 2+4=6,
// [:] is 2+10=12, and the empty [3:3] is 2+0=2.
//
// RULING, subscript: 1 (rule 1 only), diverging from the reference's
// constant 2. RFC 0005's own AST-to-call transform (dunder-transform table,
// item 4) settles rule 1's count: "Subscript(value, index) ... becomes
// Call(Name("__getitem__"), [value, index])" -- exactly one call. Rule 2
// does not apply: a plain subscript touches exactly ONE element of the
// receiver and produces a SCALAR, never "every element of a list" and never
// a produced list to iterate either -- there is no list on either side of
// the operation for rule 2 to have a claim on. The reference's constant
// "+1" -- present even at index 0, 2, 4 and 9 across two list lengths -- has
// no textual home: rule 1 is fully accounted for by the RFC's single dunder
// call, and rule 2 has no element or list to charge. This is an uncredited
// reference implementation artifact, not a consequence of either rule's
// text, so sqi does not adopt it. This ruling was independently reviewed
// and confirmed correct; it is unchanged from the first version of this
// comment.
//
// RULING, slice: rule 1 (1) PLUS the produced element count -- e.g.
// [1,2,3][0:2] is 1 + 2 = 3. THIS CORRECTS AN EARLIER, WRONG RULING kept
// here (rather than silently replaced) because a falsified claim in this
// package stays auditable. The first version of this comment argued slice
// should charge rule 1 alone, on two grounds, and BOTH were wrong:
//
//  1. "It is one __getitem__ call per RFC 0005's dunder-transform table
//     (item 5), so rule 1 is the whole charge." TRUE that it is one call,
//     but this package's own precedent already rejects "one call means no
//     rule 2": join() (funcsstrsplit.go) charges rule 2's element count AND
//     rule 3's byte count on its single call, and list repetition
//     (ops.go's OpMul, ResultElements) charges rule 1 via callShape AND a
//     rule-2 element count on ITS single call. Rules 1 and 2 are additive
//     wherever they both apply, not alternatives.
//  2. "__getitem__ is not named in rule 2's own enumeration
//     (specNamedIteratingFunctions), so rule 2 cannot apply." This
//     package's own precedent already rejects "not named" as sufficient
//     too: string(list) (funcsconv.go) charges ArgElements despite being
//     absent from rule 2's list, because it provably walks every element
//     to build its output; shape.go's own comment on ResultElements
//     states the ruling this package has held since Task 5: "rule 2 covers
//     generators as well as consumers".
//
// The line this package actually draws -- restated here because it is what
// the corrected ruling rests on -- is whether the work is
// element-count-dominated. split() (funcsstrsplit.go) is unnamed and
// correctly NOT charged elements, because its cost is byte-scan-dominated.
// Slice is the opposite: sliceValue (slice.go) does an O(1) bounds
// computation (sliceIndices) followed by an O(len(idx)) copy loop that
// walks and copies every SELECTED element into a newly produced list --
// exactly list repetition's shape, and exactly what rule 2 charges there.
// A subscript has no such loop (it reads one element and returns), which is
// why the subscript ruling above is untouched by this correction: the
// distinguishing question -- is a list actually walked and copied? -- has
// two different answers for the two constructs, not one shared answer.
//
// The reference's reported totals (2 for subscript, [4,11,6,12,2] for the
// five slice rows) were independently re-measured during review and
// confirmed accurate. What was wrong was treating the reference's
// discrepancy as entirely an artifact: rule 1's "+1" component IS an
// artifact (RFC 0005 confirms one call, never two, so there is no textual
// basis for the reference's flat 2 on a plain subscript, which never
// touches a list at all) -- but the reference's SCALING component
// (2+len(result) for slice, growing 4/11/6/12 with the selection while
// staying flat at 2 for subscript, which never selects more than one
// element) is exactly rule 2's element charge, correctly present in the
// reference and previously misread here as part of the same artifact.
//
// FOLLOW-UP CORRECTION, same fix round: the list-only fix above is not the
// whole story. sliceValue (slice.go) also handles STRING and RANGE_EXPR
// receivers, through the SAME sliceIndices machinery, and an identical
// probe run against those two receiver kinds shows they do NOT scale with
// the selection the way list does -- they are FLAT regardless of what is
// selected, and instead scale with the RECEIVER:
//
//	3    'hello'[0:0]                        3     'hello'[0:1]
//	11   ('a'*1000)[0:1]                     11    ('a'*1000)[0:1000]
//	785  ('a'*100000)[0:1]                   785   ('a'*100000)[0:100000]
//	3    range_expr('1-1000')[0:1]           3     range_expr('1-1000')[0:1000]
//	3    range_expr('1-10000')[0:1]          3     range_expr('1-10000')[0:10000]
//
// This is not a reference quirk to reject -- it matches what sqi's OWN
// sliceValue genuinely does: recv.AsList() is O(1) (returns the backing
// slice, no copy), so a list slice's only real work is the O(len(idx))
// copy loop, proportional to the SELECTION -- but []rune(recv.AsStr())
// touches every byte of the WHOLE string to find rune boundaries regardless
// of slice bounds, and rangeInts (sliceRangeExpr's first call) fully
// EXPANDS a range_expr to a concrete []int64 regardless of slice bounds
// too. Both do real O(receiver-size) work before any selection happens, so
// charging on the selection for these two receivers would UNDER-charge
// exactly the gap this whole fix round exists to close: a huge string or
// range_expr sliced down to nothing would read as nearly free while sqi's
// own implementation did receiver-sized work regardless. sliceValue now
// charges the STRING branch on ceil(receiver-bytes/256) (rule 3, matching
// funcsstrfind.go's strip/find precedent: ArgBytes on the full receiver
// "even though a real implementation only needs" less) and the
// RANGE_EXPR branch on rangeExprCount(receiver) (rule 2, computed
// arithmetically so the charge itself does not require the expansion it is
// guarding). See sliceValue's and sliceRangeExpr's own doc comments.
func TestOperationCount_SubscriptAndSlice(t *testing.T) {
	tests := []struct {
		src  string
		want int64
	}{
		// Not Shape-dispatched -- evalIndex calls indexValue directly and
		// evalSlice calls sliceValue directly -- so both charge in the
		// evaluator rather than in callShape.
		//
		// Subscript: exactly ONE __getitem__ call (RFC 0005 item 4), and
		// rule 2 has no list to apply to (one element read, no list
		// produced) -- flat 1 regardless of index or receiver length. Two
		// cases here (first element, last element of a longer list) pin
		// that flatness in the suite rather than leaving it only in the
		// PROBE comment above.
		{"[1,2,3][0]", 1},
		{"[1,2,3,4,5,6,7,8,9,10][9]", 1},
		// Slice: rule 1 (1) plus rule 2's element count on the PRODUCED
		// list (sliceValue's copy loop walks and writes exactly this many
		// elements, the same shape list repetition's ResultElements
		// charges). Two cases at different result lengths (2 and 9) make
		// the charge visibly PROPORTIONAL rather than merely nonzero.
		{"[1,2,3][0:2]", 3},                 // 1 + 2
		{"[1,2,3,4,5,6,7,8,9,10][0:9]", 10}, // 1 + 9
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%q) = %d; want %d", tt.src, got, tt.want)
			}
		})
	}
}

// TestOperationCount_SliceReceiverSizedForStringAndRangeExpr covers
// sliceValue's other two receiver kinds, deliberately not folded into
// TestOperationCount_SubscriptAndSlice above: list slicing charges on the
// SELECTION (see that test), but string and range_expr slicing charge on
// the RECEIVER instead, because that is what sqi's own []rune conversion
// and rangeInts expansion actually do regardless of slice bounds -- see the
// FOLLOW-UP CORRECTION comment above and sliceValue's own doc comment.
// Values computed with opsFor itself (t.Logf against the probe expressions
// below during the fix, cross-checked by hand against each construct's own
// already-declared cost): 'hello'[1:3] is 1 (rule 1) + ceil(5/256)=1 = 2;
// ('a'*300)[0:1] is repetition's own 3 (1 + ceil(300/256)=2, per
// TestOperationCount_StringRepetition's formula) PLUS the slice's 1 +
// ceil(300/256)=2 = 3, total 6; range_expr('1-10')'s own construction is 1
// (a bare call, no elements -- range_expr() itself declares no Cost) plus
// the slice's 1 + rangeExprCount(10) = 11, total 12.
func TestOperationCount_SliceReceiverSizedForStringAndRangeExpr(t *testing.T) {
	tests := []struct {
		src  string
		want int64
	}{
		// String: flat regardless of the selected span -- both charge the
		// SAME 2 (1 call + ceil(5/256)=1) despite selecting 2 characters
		// vs. all 5.
		{"'hello'[1:3]", 2},
		{"'hello'[0:5]", 2},
		// A larger receiver crossing a 256-byte boundary, to confirm the
		// charge tracks the RECEIVER's byte count and not the selection:
		// both a 1-character and a full 300-character selection off the
		// SAME 300-byte receiver charge identically.
		{"('a'*300)[0:1]", 6},
		{"('a'*300)[0:300]", 6},
		// range_expr: flat regardless of the selected span, same shape as
		// string above -- both charge the SAME 12 despite selecting 3
		// elements vs. all 10.
		{"range_expr('1-10')[2:5]", 12},
		{"range_expr('1-10')[0:9]", 12},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%q) = %d; want %d", tt.src, got, tt.want)
			}
		})
	}
}

func TestOperationCount_ListEquality(t *testing.T) {
	// Section 1.3.10 rule 2 names "list/range equality comparisons" explicitly,
	// and this path returns from applyBinary's valuesEqual fast path BEFORE
	// matchShapes runs, so it never reaches callShape. The reference reports 2
	// for a two-element comparison, which is 1 + 1 rather than the 1 + 2 a
	// straight reading of rule 2 gives -- adjudicated in favor of the spec
	// text: rule 2 names this comparison explicitly as an operation that
	// iterates a list's elements, so the element count is charged in full
	// (elementCount of the left operand), giving 1 (call) + 2 (elements) = 3.
	// The reference's 2 has no textual support -- it does not scale with the
	// operand length the way concatenation and repetition (rule 2's other
	// named list operators) demonstrably do elsewhere in this package -- so
	// sqi does not adopt it.
	if got := opsFor(t, "[1,2] == [1,2]"); got != 3 {
		t.Errorf("ops = %d; want 3 (1 call + 2 elements)", got)
	}
}

func TestOperationLimit_CatchesANestedComprehension(t *testing.T) {
	// The case section 1.3.10 exists for, and the one no per-operation
	// allocation bound can catch: bounded live memory, unbounded CPU. This is
	// the shape of expr1.3.10--operation-limit-exceeded, and (since
	// sub-project E1's memory accounting landed) it must be the REAL fixture's
	// own shape, not a simplified stand-in.
	//
	// CORRECTION (sub-project E1, Task 11): an earlier revision of this test
	// built its own "[[[x for x in range(300)] for y in range(300)] for z in
	// range(300)]", reasoning it was equivalent in shape. It was not: that
	// expression's outer levels accumulate whole nested LISTS as their
	// produced elements (a 300-int list at the middle level, a list of 300 of
	// those at the outer level), so once section 1.3.9 memory accounting was
	// actually wired into evalNode, its live memory hit the DEFAULT 100MB
	// bound after only ~18 outer iterations -- long before 10 million
	// operations, failing with errMemoryLimit instead of errOperationLimit and
	// falsifying this test's own "bounded live memory" premise. The real
	// conformance fixture avoids this by wrapping every level's accumulation
	// in len(), which collapses each produced element back down to a single
	// int before it becomes part of the next level's list -- keeping live
	// memory in the tens of kilobytes throughout while operations still climb
	// past the limit through the cubic iteration pattern.
	//
	// This is a FIXTURE-SHAPE regression test, not an isolation test for the
	// comprehension charge specifically: it already passed (under the old
	// fixture) before runComp's chargeElements landed, because the other
	// operations charged inside the triple-nested body (range() and the
	// comprehensions' element counts at every level) were enough to trip the
	// default 10-million-operation limit on their own. TestComprehension_
	// SharesTheCallersMeter below is the test that isolates the comprehension
	// charge's presence with an exact count on a body too small for anything
	// else to trip the limit first.
	_, err := Eval("[len([i for i in [len(range(300)) for j in range(300)]]) for k in range(300)]",
		nil, TAny)
	if !errors.Is(err, errOperationLimit) {
		t.Fatalf("a triple-nested comprehension over range(300) = %v; want errOperationLimit", err)
	}
}

func TestComprehension_SharesTheCallersMeter(t *testing.T) {
	// runComp builds a DERIVED evalCtx for the comprehension scope. The derived
	// context must carry the SAME meter pointer, or a comprehension's work --
	// precisely the unbounded case section 1.3.10 exists for -- would be charged
	// to a discarded copy and the limit above could never fire.
	e, err := Parse("[x * 2 for x in [1,2,3]]")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ec := newEvalCtx("", MapSymbols(nil), nil)
	if _, err := evalNode(e.root, ec, TAny, 0); err != nil {
		t.Fatalf("eval: %v", err)
	}
	// 3 iterations + 3 body calls. If the scoped context carried its own meter,
	// the body calls would be lost and this would read 3.
	if ec.m.ops != 6 {
		t.Errorf("ops = %d; want 6 (3 iterations + 3 body calls)", ec.m.ops)
	}
}
