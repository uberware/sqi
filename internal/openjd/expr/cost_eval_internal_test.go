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
// RFC 0005's own AST-to-call transform (dunder-transform table, items 4 and
// 5) settles what this means: "Subscript(value, index) ... becomes
// Call(Name("__getitem__"), [value, index])" and "Subscript(value,
// Slice(lower, upper, step)) becomes Call(Name("__getitem__"), [value,
// lower_or_none, upper_or_none, step_or_none])". BOTH forms are explicitly
// ONE __getitem__ call -- the slice bounds are extra ARGUMENTS to that one
// call, not a second call. So rule 1 (section 1.3.10) charges exactly 1 for
// each, and __getitem__ is not named anywhere in rule 2's own enumeration
// (shape.go's specNamedIteratingFunctions) -- unlike list/range equality,
// concatenation and repetition, which rule 2 names explicitly. A plain
// subscript touches exactly ONE element of the receiver, never "every
// element of a list", so rule 2 cannot apply to it on the rule's own words
// regardless of whether its named-function list is exhaustive.
//
// The reference's constant "+1" beyond rule 1 on EVERY row above -- present
// even on the subscript rows, which touch one element, and even on the empty
// slice [3:3], which touches none -- has no textual home in either rule.
// Rule 1 is fully accounted for by a single dunder call per the RFC's own
// transform (never 2). Rule 2 does not name __getitem__, and even read as
// non-exhaustive ("iterates through every element of A list"), neither a
// single-index subscript nor a partial slice touches every element of the
// RECEIVER -- sqi's own indexValue/sliceValue (list.go, slice.go) are
// direct-index operations for exactly this reason, touching only what is
// selected. The reference's extra unit is an uncredited implementation
// artifact (plausibly an internal slice-object construction step the RFC's
// transform table explicitly declines to make a second call), not a
// consequence of either rule's text. Per the standing method -- run the
// probe, decide against the spec text, adopt the reference's number only
// where the text agrees with it -- sqi does not adopt it: both subscript and
// slice charge rule 1 alone.
func TestOperationCount_SubscriptAndSlice(t *testing.T) {
	tests := []struct {
		src  string
		want int64
	}{
		// Not Shape-dispatched -- evalIndex calls indexValue directly and
		// evalSlice calls sliceValue directly -- so both charge in the
		// evaluator rather than in callShape. Each is exactly ONE
		// __getitem__ call per RFC 0005's dunder-transform table (items 4
		// and 5), and __getitem__ is not one of rule 2's named iterating
		// functions, so rule 1 is the whole charge for both: see the PROBE
		// above for why this diverges from the reference's measured 2 and 4.
		{"[1,2,3][0]", 1},
		{"[1,2,3][0:2]", 1},
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
	// the shape of expr1.3.10--operation-limit-exceeded.
	_, err := Eval("[[[x for x in range(300)] for y in range(300)] for z in range(300)]",
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
