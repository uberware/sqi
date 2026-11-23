// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

// TestOperationCount_ConversionAndListFunctions pins section 1.3.10's charges
// for funcsconv.go's and funcslist.go's rows. The reasoning for each Cost is
// beside the row; in brief:
//
//   - len() is exempt (rule 3's "simple lookups like len()"), whatever it is
//     given; int(), float(), bool() and string() of a scalar cost a flat 1
//     however long the input.
//   - list(range_expr) and range_expr(list[int]) charge one per element.
//   - string(list) charges the argument list's length (rule 2's general
//     clause); see TestOperationCount_StringOverListDivergesFromReference.
//   - flatten's nested row charges the outer length plus the result's
//     (flatten([[1,2,3],[4,5]]) costs 1 + 2 + 5 = 8); its flat row charges the
//     list's length once; flatten([]) resolves to the flat row.
//   - sorted and reversed are linear in the input.
//   - unique charges one operation per valuesEqual comparison (uniqueList,
//     FnCtx), so its quadratic scan is bounded
//     (TestUnique_IsBoundedByTheOperationLimit); the reference charges the
//     input length only.
//   - any and all charge the full list length, where the reference stops at
//     the deciding element; see TestOperationCount_AnyAllDivergeOnShortCircuit.
//     A list here is written as a literal: "[True]*10" would add the
//     repetition's own rule-2 charge.
func TestOperationCount_ConversionAndListFunctions(t *testing.T) {
	tests := []struct {
		src  string
		want int64
		why  string
	}{
		// Section 1.3.10 exempts "simple lookups like len() that do not
		// process the string content". All four len rows charge rule 1 only.
		// Pinned because an exemption and an oversight look identical
		// otherwise.
		{"len([1,2,3])", 1, "len is exempt from rule 2"},
		{"len('abc')", 1, "len is exempt from rule 3"},
		// len(range_expr(...)) is 2: one call for range_expr(), one for
		// len(). len itself charges nothing, and in particular does not
		// expand the range to count it -- see TestLenRangeExpr_DoesNotMaterialize.
		{"len(range_expr('1-100'))", 2, "range_expr() call + len() call, no expansion"},
		// path(string) declares Cost{ArgBytes: {0}} (funcspath.go), so
		// path('/a/b') itself costs 1 call + ceil(4/256)=1 = 2, matching the
		// reference's own 3 exactly once len()'s own call (1, adding nothing
		// further -- len stays exempt) is added: 2 + 1 = 3.
		{"len(path('/a/b'))", 3, "path() call (ArgBytes-charged) + len() call, len itself adds nothing"},

		// bool()/int()/float() carry no Cost anywhere -- neither rule names
		// them, and probing shows their own work does not scale with input
		// size (see the discriminating isolation tests below for the
		// over-256-byte proof; these are the flat baseline cases).
		{"bool('true')", 1, "bool() is a closed eight-spelling membership test, not length-proportional"},
		{"bool(true)", 1, "bool(bool) is an identity read"},
		{"bool(1)", 1, "bool(int) is a zero-test"},
		{"int('5')", 1, "int() from string charges nothing beyond the call"},
		{"int(5)", 1, "int(int) is an identity read"},
		{"float('1.5')", 1, "float() from string charges nothing beyond the call"},
		{"float(1)", 1, "float(int) is a widening read"},

		// string() over a scalar charges nothing -- Value.String() does no
		// length-proportional work for a scalar the way writeJSONValue does
		// for a list (see the divergence test below for the list row).
		{"string(1)", 1, "string(int) formats a fixed-size number"},
		{"string('a')", 1, "string(string) is a flat baseline"},

		// range_expr(string) stays uncharged, matching len(range_expr(...))'s
		// exemption -- parsing compact range text is not proportional to the
		// expanded count.
		{"range_expr('1-100')", 1, "range_expr(string) parses compact text, not proportional to the expanded count"},
		// range_expr(list[int]) does charge: it must scan the whole input
		// list to sort and de-duplicate it.
		{"range_expr([1,2,3])", 4, "range_expr(list) scans every input element (1 call + 3 elements)"},

		// list() over a range_expr materializes every value: charged by the
		// produced list's size.
		{"list(range_expr('1-3'))", 5, "list() over range_expr: 1 call + 1 (range_expr() itself) + 3 produced elements"},

		// range()'s three arities all charge by the produced list.
		{"range(5)", 6, "range(stop): 1 call + 5 produced elements"},
		{"range(0, 5)", 6, "range(start, stop): 1 call + 5 produced elements"},
		{"range(0, 10, 2)", 6, "range(start, stop, step): 1 call + 5 produced elements (0,2,4,6,8)"},

		// sorted/reversed charge the input list's length, as the reference
		// does (no short-circuit, unlike any/all).
		{"sorted([3,1,2])", 4, "sorted(): 1 call + 3 elements"},
		{"reversed([1,2,3,4,5])", 6, "reversed(): 1 call + 5 elements"},
		// unique() declares no Cost{ArgElements: {0}} at all -- it charges
		// itself, per valuesEqual comparison, via FnCtx. For
		// [1,1,2]: comparing the second 1 against the kept [1] costs 1
		// comparison and finds a duplicate; comparing 2 against the kept [1]
		// costs 1 more and finds none. 1 call + 2 comparisons = 3. This
		// diverges from the reference's linear count (4), as baselined in
		// test/oracle/baseline-ops.txt.
		{"unique([1,1,2])", 3, "unique(): 1 call + 2 comparisons (quadratic scan, charged per comparison, not per input element)"},

		// flatten's three rows; the second case's outer and result counts
		// differ.
		{"flatten([[1],[2]])", 5, "flatten(nested): 1 call + 2 outer + 2 result"},
		{"flatten([[1,2,3],[4,5]])", 8, "flatten(nested), outer and result now DIFFER (2 vs 5), proving both charges are real"},
		{"flatten([[],[],[]])", 4, "flatten(nested), empty inners: 1 call + 3 outer + 0 result"},
		{"flatten([1,2,3])", 4, "flatten(flat): a single charge of the list length even though Fn is a plain identity"},
		{"flatten([])", 1, "flatten([]) always resolves to the flat row's tie-break, charging 0 elements"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := opsFor(t, tt.src); got != tt.want {
				t.Errorf("ops(%q) = %d; want %d (%s)", tt.src, got, tt.want, tt.why)
			}
		})
	}
}

// TestOperationCount_IntFloatBoolStayFlatOverBigInput isolates int(), float()
// and string()'s own charge from the already-charged cost of building a
// large operand (string repetition/concatenation), using callFunction
// directly so no other operator's charge is mixed in. A sub-256-byte input
// cannot discriminate "charges nothing" from "charges nothing because the
// input happened to be small" (the same trap applies to string "+"). These
// use inputs over 256 bytes and confirm the count does not move.
func TestOperationCount_IntFloatBoolStayFlatOverBigInput(t *testing.T) {
	t.Run("float() over a 301-byte numeric string", func(t *testing.T) {
		ec := testCtx()
		big := "1" + strings.Repeat("2", 300)
		if _, err := callFunction(ec, "float", []Value{String(big)}, false); err != nil {
			t.Fatalf("callFunction(float): %v", err)
		}
		if ec.m.ops != 1 {
			t.Errorf("ops = %d; want 1 (rule 1 only, no ArgBytes charge)", ec.m.ops)
		}
	})

	t.Run("int() over a 255-byte numeric string", func(t *testing.T) {
		ec := testCtx()
		big := strings.Repeat("0", 250) + "5"
		if _, err := callFunction(ec, "int", []Value{String(big)}, false); err != nil {
			t.Fatalf("callFunction(int): %v", err)
		}
		if ec.m.ops != 1 {
			t.Errorf("ops = %d; want 1 (rule 1 only, no ArgBytes charge)", ec.m.ops)
		}
	})

	t.Run("string() over a 1000-byte string", func(t *testing.T) {
		ec := testCtx()
		big := strings.Repeat("a", 1000)
		if _, err := callFunction(ec, "string", []Value{String(big)}, false); err != nil {
			t.Fatalf("callFunction(string): %v", err)
		}
		if ec.m.ops != 1 {
			t.Errorf("ops = %d; want 1 (rule 1 only, no ArgBytes charge)", ec.m.ops)
		}
	})

	t.Run("bool() is unaffected by input length (closed spelling set)", func(t *testing.T) {
		ec := testCtx()
		if _, err := callFunction(ec, "bool", []Value{String("true")}, false); err != nil {
			t.Fatalf("callFunction(bool): %v", err)
		}
		if ec.m.ops != 1 {
			t.Errorf("ops = %d; want 1 (rule 1 only)", ec.m.ops)
		}
	})
}

// TestOperationCount_StringOverListDivergesFromReference pins the deliberate
// divergence: sqi charges string() over a list by its element count, the
// reference does not, at any size. See the Cost comment on this row in
// funcsconv.go for the spec-text justification (rule 2's general sentence,
// not its "such as" enumeration, is what covers it) and
// test/oracle/baseline-ops.txt for how the reference's count differs.
func TestOperationCount_StringOverListDivergesFromReference(t *testing.T) {
	small := opsFor(t, "string([1,2,3])")
	if small != 4 {
		t.Errorf("ops(string([1,2,3])) = %d; want 4 (1 call + 3 elements) -- the reference measures 1 here", small)
	}
	big := opsFor(t, "string([1,2,3,4,5,6,7,8,9,10])")
	if big != 11 {
		t.Errorf("ops(string(<10 elements>)) = %d; want 11 (1 call + 10 elements) -- the reference measures 1 here too, never scaling", big)
	}
	if big-small != 7 {
		t.Errorf("charge did not scale with list length: small=%d big=%d", small, big)
	}
}

// TestOperationCount_AnyAllDivergeOnShortCircuit pins the other kind of
// divergence: unlike every other row, the reference charges
// less than sqi's flat "1 + list length" here, because it short-circuits.
// sqi's declarative Cost mechanism charges arguments before Fn ever runs
// (chargeArgs in callShape, ops.go), so it cannot know how many elements a
// short-circuiting Fn will actually visit -- charging the full length is a
// safe over-approximation, never an under-count. See the Cost comment on the
// any/all list[bool] rows in funcslist.go.
func TestOperationCount_AnyAllDivergeOnShortCircuit(t *testing.T) {
	// boolList builds a list[bool] of n copies of v, via direct construction
	// rather than a source literal: a source string repeating "true, true,
	// ..." trips golangci-lint's dupword check for no benefit, and this is
	// also more direct for stating "n copies of the same bool" than typing
	// them out.
	boolList := func(v bool, n int) Value {
		vals := make([]Value, n)
		for i := range vals {
			vals[i] = Bool(v)
		}
		return List(TBool, vals)
	}

	t.Run("any() charges the full length even when the first element is true", func(t *testing.T) {
		// The reference short-circuits here and charges 2 (1 call + 1
		// element visited). sqi's declarative Cost cannot see that the loop
		// would stop after one element, so it charges the full list.
		ec := testCtx()
		if _, err := callFunction(ec, "any", []Value{boolList(true, 10)}, false); err != nil {
			t.Fatalf("callFunction(any): %v", err)
		}
		if ec.m.ops != 11 {
			t.Errorf("ops = %d; want 11 (1 call + 10 elements, a safe over-approximation of the reference's short-circuited 2)", ec.m.ops)
		}
	})

	t.Run("any() over all-false matches the reference exactly (nothing to short-circuit)", func(t *testing.T) {
		ec := testCtx()
		if _, err := callFunction(ec, "any", []Value{boolList(false, 5)}, false); err != nil {
			t.Fatalf("callFunction(any): %v", err)
		}
		if ec.m.ops != 6 {
			t.Errorf("ops = %d; want 6 (1 call + 5 elements) -- matches the reference here since it must scan everything too", ec.m.ops)
		}
	})

	t.Run("all() charges the full length even when the first element is false", func(t *testing.T) {
		ec := testCtx()
		if _, err := callFunction(ec, "all", []Value{boolList(false, 5)}, false); err != nil {
			t.Fatalf("callFunction(all): %v", err)
		}
		if ec.m.ops != 6 {
			t.Errorf("ops = %d; want 6 (1 call + 5 elements, a safe over-approximation of the reference's short-circuited 2)", ec.m.ops)
		}
	})

	t.Run("all() over all-true matches the reference exactly", func(t *testing.T) {
		ec := testCtx()
		if _, err := callFunction(ec, "all", []Value{boolList(true, 10)}, false); err != nil {
			t.Fatalf("callFunction(all): %v", err)
		}
		if ec.m.ops != 11 {
			t.Errorf("ops = %d; want 11 (1 call + 10 elements)", ec.m.ops)
		}
	})
}

// TestOperationCount_AnyAllListNullRowChargesNothing pins the list[nulltype]
// rows: an empty list is empty by type, so there is nothing to iterate
// regardless of Cost, and both rows are declared with the zero Cost value
// rather than a copy of the list[bool] row's ArgElements{0} to say so
// explicitly.
func TestOperationCount_AnyAllListNullRowChargesNothing(t *testing.T) {
	if got := opsFor(t, "any([])"); got != 1 {
		t.Errorf("ops(any([])) = %d; want 1 (rule 1 only)", got)
	}
	if got := opsFor(t, "all([])"); got != 1 {
		t.Errorf("ops(all([])) = %d; want 1 (rule 1 only)", got)
	}
}

// TestOperationCount_UnresolvedListArgumentChargesRuleOneOnly covers the
// ruling "an unresolved operand charges rule 1 only -- no element or byte
// charges, since no values were processed." chargeArgs (ops.go) reads
// elementCount off a real Value, and an unresolved placeholder is never a
// CodeList value, so elementCount already returns 0 for it structurally;
// this test pins that behavior for one of the list-function charging rows
// rather than assume it holds.
func TestOperationCount_UnresolvedListArgumentChargesRuleOneOnly(t *testing.T) {
	ec := testCtx()
	if _, err := callFunction(ec, "sorted", []Value{Unresolved(ListOf(TInt))}, false); err != nil {
		t.Fatalf("callFunction(sorted, unresolved): %v", err)
	}
	if ec.m.ops != 1 {
		t.Errorf("ops = %d; want 1 (rule 1 only, no element charge for an unresolved operand)", ec.m.ops)
	}
}
