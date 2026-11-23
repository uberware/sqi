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
	// arithmetic error in the spec: ceil(100000/256) is 391, not 392, because
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

// TestOperationCount_SubscriptAndSlice pins the charges for subscript and
// slice, neither of which is Shape-dispatched.
//
// Subscript of a list costs 1 (rule 1 only). RFC 0005's dunder-transform
// table (item 4) makes "Subscript(value, index)" one __getitem__ call, and
// rule 2 has nothing to charge: AsList returns the backing slice with no copy
// (value.go), so the subscript touches one element and walks no list. The
// reference charges a constant 2, which no rule accounts for.
//
// Subscript of a string or range_expr is charged by receiver size: indexValue
// (list.go) decodes the whole string ([]rune) or expands the whole range
// (rangeInts) whatever the index, so evalIndex charges rule 3 on the string's
// bytes and rule 2 on the range's element count, as sliceValue does. A flat 1
// would charge a 500,000-byte string 1 where its slice costs 1,954. See
// TestOperationCount_SubscriptReceiverSizedForStringAndRangeExpr.
//
// Slice of a list costs rule 1 plus the produced element count:
// [1,2,3][0:2] is 1 + 2 = 3. It is one __getitem__ call (item 5), but one
// call does not exclude rule 2 (join() and list repetition charge rule 1 and
// rule 2 on one call), and being unnamed in rule 2's enumeration does not
// exempt it (string(list) is unnamed and charged; shape.go: "rule 2 covers
// generators as well as consumers"). The question is whether the work is
// element-count-dominated: sliceValue copies every selected element into a
// new list, as repetition does, while split() is byte-scan-dominated and
// charges no elements. The reference charges 2 + len(result).
//
// Slice of a string or range_expr is charged by receiver size, not by the
// selection: []rune(recv.AsStr()) touches every byte and rangeInts expands
// the whole range before any selection happens, so a selection-based charge
// would read a huge receiver sliced to nothing as nearly free. sliceValue
// charges the string on ceil(receiver-bytes/256) (rule 3, as strip/find
// charge the full receiver) and the range_expr on rangeExprCount(receiver)
// (rule 2, computed arithmetically). See sliceValue's and sliceRangeExpr's
// doc comments.
func TestOperationCount_SubscriptAndSlice(t *testing.T) {
	tests := []struct {
		src  string
		want int64
	}{
		// Not Shape-dispatched -- evalIndex calls indexValue directly and
		// evalSlice calls sliceValue directly -- so both charge in the
		// evaluator rather than in callShape.
		//
		// Subscript of a list: exactly one __getitem__ call (RFC 0005
		// item 4), and rule 2 has no list to apply to (AsList is O(1), one
		// element read, no list produced) -- flat 1 regardless of index or
		// receiver length. Two cases here (first element, last element of a
		// longer list) pin that flatness. A string or range_expr
		// receiver is not flat and is covered separately by
		// TestOperationCount_SubscriptReceiverSizedForStringAndRangeExpr.
		{"[1,2,3][0]", 1},
		{"[1,2,3,4,5,6,7,8,9,10][9]", 1},
		// Slice: rule 1 (1) plus rule 2's element count on the produced
		// list (sliceValue's copy loop walks and writes exactly this many
		// elements, the same shape list repetition's ResultElements
		// charges). Two cases at different result lengths (2 and 9) make
		// the charge visibly proportional rather than merely nonzero.
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
// selection (see that test), but string and range_expr slicing charge on
// the receiver instead, because that is what sqi's own []rune conversion
// and rangeInts expansion actually do regardless of slice bounds -- see the
// string and range_expr slices comment above and sliceValue's own doc comment.
// Values computed with opsFor itself and cross-checked by hand against each
// construct's own declared cost: 'hello'[1:3] is 1 (rule 1) + ceil(5/256)=1 = 2;
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
		// same 2 (1 call + ceil(5/256)=1) despite selecting 2 characters
		// vs. all 5.
		{"'hello'[1:3]", 2},
		{"'hello'[0:5]", 2},
		// A larger receiver crossing a 256-byte boundary, to confirm the
		// charge tracks the receiver's byte count and not the selection:
		// both a 1-character and a full 300-character selection off the
		// same 300-byte receiver charge identically.
		{"('a'*300)[0:1]", 6},
		{"('a'*300)[0:300]", 6},
		// range_expr: flat regardless of the selected span, same shape as
		// string above -- both charge the same 12 despite selecting 3
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

// TestOperationCount_SubscriptReceiverSizedForStringAndRangeExpr is the
// subscript counterpart of the slice test above: a flat 1 for all three
// receiver kinds would under-charge the two for which indexValue does
// O(receiver) work, and every other subscript case uses a list receiver.
//
// The expected values are the slice test's own values one for one --
// deliberately, since the two constructs charge the same receiver-sized
// amount for a string and a range_expr receiver. Only the list receiver
// differs, and it stays flat at 1 (TestOperationCount_SubscriptAndSlice pins
// that).
func TestOperationCount_SubscriptReceiverSizedForStringAndRangeExpr(t *testing.T) {
	tests := []struct {
		src  string
		want int64
	}{
		// String: 1 (rule 1) + ceil(receiver bytes/256), flat regardless of
		// which index is read -- []rune decodes the whole receiver either
		// way.
		{"'hello'[0]", 2},
		{"'hello'[4]", 2},
		// A receiver crossing a 256-byte boundary: repetition's own 3
		// (1 + ceil(300/256)=2) plus the subscript's 1 + 2 = 3, total 6 --
		// identical to ('a'*300)[0:1] above, and identical at either end of
		// the receiver.
		{"('a'*300)[0]", 6},
		{"('a'*300)[299]", 6},
		// range_expr: range_expr()'s own construction charge of 1, plus the
		// subscript's 1 + rangeExprCount(10) = 11, total 12 -- identical to
		// range_expr('1-10')[2:5] above, and flat regardless of index.
		{"range_expr('1-10')[0]", 12},
		{"range_expr('1-10')[9]", 12},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%q) = %d; want %d", tt.src, got, tt.want)
			}
		})
	}
}

// TestOperationLimit_CatchesRepeatedSubscriptOfALargeReceiver is the exploit
// the receiver-sized subscript charge exists to stop, rather than a count
// assertion about it. Charged a flat 1 per subscript, this expression would
// expand a million values 500 times while counting 2,002 operations against a
// 10,000,000 budget, and scaling the outer range would keep it under a fifth of
// the budget while running for a very long time. It must trip the limit.
func TestOperationLimit_CatchesRepeatedSubscriptOfALargeReceiver(t *testing.T) {
	const src = "len([range_expr('1-1000000')[0] for x in range(500)])"
	_, _, err := EvalWithMetrics(src, MapSymbols(nil), TInt)
	if err == nil {
		t.Fatalf("Eval(%q) succeeded; want the operation limit to stop it", src)
	}
	if !errors.Is(err, errOperationLimit) {
		t.Errorf("err = %v; want errOperationLimit", err)
	}
}

func TestOperationCount_ListEquality(t *testing.T) {
	// Section 1.3.10 rule 2 names "list/range equality comparisons" explicitly,
	// and this path returns from applyBinary's valuesEqual fast path before
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
	// the shape of expr1.3.10--operation-limit-exceeded, and with section
	// 1.3.9 memory accounting in place it must be the real fixture's own
	// shape, not a simplified stand-in.
	//
	// A stand-in such as "[[[x for x in range(300)] for y in range(300)] for
	// z in range(300)]" is not equivalent in shape: its outer levels
	// accumulate whole nested lists as their produced elements (a 300-int list
	// at the middle level, a list of 300 of those at the outer level), so its
	// live memory hits the default 100MB bound after only ~18 outer iterations
	// -- long before 10 million operations, failing with errMemoryLimit
	// instead of errOperationLimit and falsifying this test's own "bounded
	// live memory" premise. The real conformance fixture avoids this by
	// wrapping every level's accumulation in len(), which collapses each
	// produced element back down to a single int before it becomes part of
	// the next level's list -- keeping live memory in the tens of kilobytes
	// throughout while operations still climb past the limit through the
	// cubic iteration pattern.
	//
	// This is a fixture-shape test, not an isolation test for the
	// comprehension charge specifically: the other operations charged inside
	// the triple-nested body (range() and the comprehensions' element counts
	// at every level) trip the default 10-million-operation limit on their
	// own. TestComprehension_SharesTheCallersMeter below isolates the
	// comprehension charge's presence with an exact count on a body too small
	// for anything else to trip the limit first.
	_, err := Eval("[len([i for i in [len(range(300)) for j in range(300)]]) for k in range(300)]",
		nil, TAny)
	if !errors.Is(err, errOperationLimit) {
		t.Fatalf("a triple-nested comprehension over range(300) = %v; want errOperationLimit", err)
	}
}

func TestComprehension_SharesTheCallersMeter(t *testing.T) {
	// runComp builds a derived evalCtx for the comprehension scope. The derived
	// context must carry the same meter pointer, or a comprehension's work --
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
