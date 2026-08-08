// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

// PROBE (sub-project E1, Task 6), .venv-oracle/bin/python -c "..." against
// openjd-model 0.11.1 -- the brief's own Step 1 command, plus follow-ups run
// to resolve every row this task owns (funcsconv.go's 19 rows, funcslist.go's
// 13). Pasted verbatim as evidence, per "run something first, decide against
// the spec text second."
//
// Brief's own command:
//
//	  1  len([1,2,3])
//	  1  len("abc")
//	  2  len(range_expr("1-100"))
//	 11  range(10)
//	102  list(range_expr("1-100"))
//	  4  sum([1,2,3])
//	  4  sorted([3,1,2])
//	  5  flatten([[1],[2]])
//	  4  unique([1,1,2])
//	  2  any([True])
//	  1  string([1,2,3])
//	  1  int("5")
//	  1  float("1.5")
//	  1  range_expr("1-100")
//
// sum/min/max are funcsmath.go rows (Task 8's), out of scope here despite
// appearing in the brief's probe command; the other twelve resolve rows this
// task owns.
//
// Follow-ups, len() and the other exemption-shaped conversions -- confirming
// the exemption does not depend on WHAT is inside, and that int/float/bool
// are flat regardless of input size (see the divergence-free rows below):
//
//	3  len(path("/a/b"))                (path()'s own construction cost
//	                                      is Task 8's, not yet declared --
//	                                      see the len(path(...)) case in
//	                                      TestOperationCount_ConversionAndListFunctions
//	                                      for what sqi computes instead)
//	1  int(5)
//	1  bool(True)  /  bool(1)  /  bool("true")
//	7  float("1"+"2"*300)                isolates to 1: the 301-byte
//	                                      string's own build cost (a
//	                                      already-charged "*" then "+",
//	                                      Task 5) is 6, leaving float()
//	                                      itself at 7-6=1
//	5  int("0"*250+"5")                  isolates to 1 the same way
//	6  string("a"*1000)                  isolates to 1 the same way
//	                                      ("a"*1000 alone is 5)
//	9  string(path("/a"*300))            isolates to 1 (path("/a"*300)
//	                                      alone was not isolated further;
//	                                      see the smaller pair below)
//	3  string(path("/a"))                isolates to 1 (path("/a") alone
//	                                      is 2, per path("/a/b")'s
//	                                      sibling measurement above)
//	2  string(range_expr("1-1000"))      isolates to 1 (range_expr(...)
//	                                      alone is 1, regardless of the
//	                                      range's span)
//
// list() and range_expr(list[int]) -- confirmed to charge, the second NOT in
// the brief's own "rows that must charge" list, found only by probing this
// row per the task's instruction to probe every row rather than trust the
// brief's starting point:
//
//	 4  range_expr([1,2,3])               1 (call) + 3 (elements)
//	11  range_expr([1,2,3,4,5,6,7,8,9,10]) 1 (call) + 10 (elements):
//	                                       proves it SCALES
//
// string(list) -- discriminating on size to prove the reference does NOT
// scale, unlike every genuinely-charged row above:
//
//	1  string([1,2,3])
//	1  string([1,2,3,4,5,6,7,8,9,10])     same count at 3x the length --
//	                                      the reference does not charge
//	                                      this row at all. sqi diverges
//	                                      on purpose; see the Cost
//	                                      comment on this row in
//	                                      funcsconv.go and
//	                                      TestOperationCount_StringOverListDivergesFromReference.
//
// flatten -- the three rows discriminated by making the outer count and the
// flattened result count DIFFER, which a single-charge hypothesis cannot
// explain but ArgElements{0}+ResultElements can:
//
//	 5  flatten([[1],[2]])          1 + 2 (outer) + 2 (result)
//	 8  flatten([[1,2,3],[4,5]])    1 + 2 (outer) + 5 (result) -- outer
//	                                and result now DIFFER, which is what
//	                                proves both charges are real
//	 4  flatten([[],[],[]])         1 + 3 (outer) + 0 (result)
//	 4  flatten([1,2,3])            1 + 3 -- the FLAT row, a single
//	                                charge despite Fn being a plain
//	                                identity that iterates nothing itself
//	11  flatten([1,2,3,4,5,6,7,8,9,10])  1 + 10, confirming the flat
//	                                row's charge scales too
//	 1  flatten([])                 the empty-list tie documented on the
//	                                flatten table in funcslist.go always
//	                                resolves to the FLAT row, never the
//	                                list[nulltype] row -- see that row's
//	                                own comment for why it is Cost{}
//
// sorted/reversed/unique -- confirmed linear, matching the reference exactly
// (no short-circuit, unlike any/all below):
//
//	 4  sorted([3,1,2])                    1 + 3
//	11  sorted([5,4,3,2,1,9,8,7,6,0])       1 + 10
//	 6  reversed([1,2,3,4,5])               1 + 5
//	 3  reversed([1,2])                     1 + 2
//	 4  unique([1,1,2])                     1 + 3
//	11  unique([1,1,1,1,1,1,1,1,1,1])       1 + 10
//
// unique's own two numbers above are the REFERENCE's, unchanged by Task 12
// and reproducible today by rerunning the same probe -- the reference charges
// unique() by input length alone, with no accounting for its comparison
// count. sqi's own unique() deliberately no longer matches them: Task 12
// found, by RUNNING TestUnique_IsBoundedByTheOperationLimit before making any
// change, that a linear charge lets unique()'s real O(n^2) valuesEqual scan
// run uncounted underneath it -- unique(range(20000)) returned successfully
// in under 4 seconds with 4*10^8 comparisons actually performed and only
// ~20001 operations charged, nowhere near the 10-million default limit. sqi
// now charges one operation per valuesEqual call instead (uniqueList,
// funcslist.go, FnCtx), which is what makes the SAME call fail fast with
// errOperationLimit. See TestOperationCount_ConversionAndListFunctions's
// unique() row below for sqi's own resulting count, and
// test/oracle/baseline-ops.txt for the adjudication of this divergence
// against the oracle.
//
// any/all -- the reference SHORT-CIRCUITS, so its own count does not equal
// "1 + list length" once the answer is decided early; this is the one
// divergence in this task that is NOT "the reference failed to charge
// something", but "the reference charges LESS than a flat per-call Cost can
// express" -- see the Cost comment on the any/all list[bool] rows in
// funcslist.go and TestOperationCount_AnyAllDivergeOnShortCircuit. All six
// lines below are the COMMA-SEPARATED LITERAL form, copy-paste-runnable as
// printed. Deliberately NOT written with "*" list-repetition syntax --
// "[True]*10" is itself a rule-2-charged operation (Task 5's OpMul row,
// ArgElements/ResultElements over the repeated-out list), so a probe of
// "any([True]*10)" measures any() PLUS the repetition's own charge baked
// in (13, not 2) and "all([True]*10)" measures 22, not 11. A prior revision
// of this comment printed those inflated totals next to the isolated
// any()/all()-only numbers below, which does not reproduce by re-running it
// -- caught in review. The literal form has no such extra charge to strip
// out, which is also why TestOperationCount_AnyAllDivergeOnShortCircuit
// builds its lists directly via a boolList helper rather than parsing a
// source string with "*" in it.
//
//	 1  any([])                             list[nulltype] row, trivial
//	 6  any([False,False,False,False,False])          1 + 5, forced to
//	                                                   scan all of them
//	 2  any([True,True,True,True,True,True,True,True,True,True])
//	                                                   1 + 1, stops at
//	                                                   the FIRST element
//	 1  all([])                             list[nulltype] row, trivial
//	11  all([True,True,True,True,True,True,True,True,True,True])
//	                                                   1 + 10, forced to
//	                                                   scan all of them
//	 2  all([False,False,False,False,False])          1 + 1, stops at
//	                                                   the FIRST element
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
		// len(). len itself charges nothing, and in particular does NOT
		// expand the range to count it -- see Task 12.
		{"len(range_expr('1-100'))", 2, "range_expr() call + len() call, no expansion"},
		// UPDATED by Task 8, as anticipated above: path(string) now declares
		// Cost{ArgBytes: {0}} (funcspath.go), so path('/a/b') itself costs
		// 1 call + ceil(4/256)=1 = 2, matching the reference's own 3 exactly
		// once len()'s own call (1, still adding nothing -- len's exemption is
		// unchanged) is added: 2 + 1 = 3.
		{"len(path('/a/b'))", 3, "path() call (now ArgBytes-charged by Task 8) + len() call, len itself still adds nothing"},

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
		// range_expr(list[int]) DOES charge: it must scan the whole input
		// list to sort and de-duplicate it. Not in the brief's own "rows
		// that must charge" list -- found by probing this row anyway.
		{"range_expr([1,2,3])", 4, "range_expr(list) scans every input element (1 call + 3 elements)"},

		// list() over a range_expr materializes every value: charged by the
		// PRODUCED list's size.
		{"list(range_expr('1-3'))", 5, "list() over range_expr: 1 call + 1 (range_expr() itself) + 3 produced elements"},

		// range()'s three arities all charge by the produced list.
		{"range(5)", 6, "range(stop): 1 call + 5 produced elements"},
		{"range(0, 5)", 6, "range(start, stop): 1 call + 5 produced elements"},
		{"range(0, 10, 2)", 6, "range(start, stop, step): 1 call + 5 produced elements (0,2,4,6,8)"},

		// sorted/reversed/unique all charge the input list's length, and the
		// reference confirms the charge is exact (no short-circuit, unlike
		// any/all).
		{"sorted([3,1,2])", 4, "sorted(): 1 call + 3 elements"},
		{"reversed([1,2,3,4,5])", 6, "reversed(): 1 call + 5 elements"},
		// Task 12: unique() no longer declares Cost{ArgElements: {0}} at all --
		// it charges itself, per valuesEqual comparison, via FnCtx. For
		// [1,1,2]: comparing the second 1 against the kept [1] costs 1
		// comparison and finds a duplicate; comparing 2 against the kept [1]
		// costs 1 more and finds none. 1 call + 2 comparisons = 3. This is a
		// deliberate divergence from the reference's own linear count (4, see
		// the probe comment above) -- baselined in test/oracle/baseline-ops.txt.
		{"unique([1,1,2])", 3, "unique(): 1 call + 2 comparisons (quadratic scan, charged per comparison, not per input element)"},

		// flatten's three rows, discriminated in the probe comment above.
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
// and string()'s own charge from the ALREADY-CHARGED cost of building a
// large operand (string repetition/concatenation, Task 5), using callFunction
// directly so no other operator's charge is mixed in. A sub-256-byte probe
// cannot discriminate "charges nothing" from "charges nothing because the
// input happened to be small" -- see the brief's own warning about this
// exact trap with string "+". These use inputs over 256 bytes and confirm
// the count does not move.
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
// not its "such as" enumeration, is what covers it) and the probe comment at
// the top of this file for the reference's own flat measurements.
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

// TestOperationCount_AnyAllDivergeOnShortCircuit pins the OTHER kind of
// divergence in this task: unlike every other row, the reference charges
// LESS than sqi's flat "1 + list length" here, because it short-circuits.
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
		// The reference short-circuits here and measures 2 (1 call + 1
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
// standing ruling that binds this task: "an unresolved operand charges rule
// 1 only -- no element or byte charges, since no values were processed."
// chargeArgs (ops.go) reads elementCount off a real Value, and an unresolved
// placeholder is never a CodeList value, so elementCount already returns 0
// for it structurally; this test exists to pin that behavior for one of this
// task's own charging rows rather than assume it holds.
func TestOperationCount_UnresolvedListArgumentChargesRuleOneOnly(t *testing.T) {
	ec := testCtx()
	if _, err := callFunction(ec, "sorted", []Value{Unresolved(ListOf(TInt))}, false); err != nil {
		t.Fatalf("callFunction(sorted, unresolved): %v", err)
	}
	if ec.m.ops != 1 {
		t.Errorf("ops = %d; want 1 (rule 1 only, no element charge for an unresolved operand)", ec.m.ops)
	}
}
