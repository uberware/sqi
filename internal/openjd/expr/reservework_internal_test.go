// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"runtime"
	"testing"
)

// This file pins the property no operation count can see: how much work an
// evaluation does.
//
// Both defects it guards against are invisible to every other test in the
// suite, including the differential oracle, because the operation counts are
// byte-identical either side of them:
//
//   - A range_expr with two or more sub-ranges cannot be counted
//     arithmetically -- rangeExprCount expands it to count it (rangeexpr.go).
//     So a reservation sized through rangeExprCount performs the expansion
//     it exists to refuse: len(range_expr("1-5000000,6000000-9000000"))
//     would expand eight million values while charging two operations, and
//     list(), a subscript and a comprehension would each pay the same
//     expansion before refusing.
//
//   - On the success path the same expression would then be expanded a
//     second time by the operation that was reserved for, with no
//     observable effect on any count.
//
// Allocation deltas, not wall clock: this package's established technique for
// a cost property that must not flake on shared CI (see
// internal/openjd/validate_paramspace_internal_test.go's
// TestParameterSpaceOverCaps_DoesNotRunOverlapScan and
// validate_exprgate_test.go's totalAllocDelta).

// multiSubRangeRefused is an eight-million-value range_expr in two
// non-overlapping sub-ranges. Non-overlap is what makes
// rangeExprCountBounds' lower bound (the largest single sub-range, 5,000,000)
// decisive on its own, so the refusal below needs no expansion to be exact.
const multiSubRangeRefused = `"1-5000000,6000000-9000000"`

// multiSubRangeAccepted is the same shape an order of magnitude smaller, so
// that it succeeds under the default budget and the success path's own
// allocation can be measured.
const multiSubRangeAccepted = `"1-500000,2000000-2400000"`

// allocDelta reports the bytes allocated by f. TotalAlloc is cumulative and
// never decreases, so a GC between the two reads cannot make this negative or
// hide work that was done and freed.
func allocDelta(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestReserveRangeExpr_RefusalDoesNotExpand pins that a multi-sub-range
// range_expr that cannot fit the budget is refused arithmetically, with no
// expansion at all.
//
// The ceiling is 1 MB, hundreds of times below what the expansion allocates
// and comfortably above the parse-and-bounds arithmetic the refusal does.
func TestReserveRangeExpr_RefusalDoesNotExpand(t *testing.T) {
	const ceiling = 1 << 20

	tests := []struct {
		name string
		src  string
	}{
		// len() is first because it is the worst case: section 1.3.10 exempts
		// len() from charging, so expanding here would cost the full expansion
		// while reporting two operations -- no budget in the package can see
		// it.
		{"len", `len(range_expr(` + multiSubRangeRefused + `))`},
		{"list", `list(range_expr(` + multiSubRangeRefused + `))`},
		{"subscript", `range_expr(` + multiSubRangeRefused + `)[0]`},
		{"slice", `range_expr(` + multiSubRangeRefused + `)[0:2]`},
		{"comprehension", `[x for x in range_expr(` + multiSubRangeRefused + `)]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			allocated := allocDelta(func() {
				_, err = Eval(tt.src, MapSymbols(nil), TAny,
					WithOperationLimit(reserveOpLimit), WithMemoryLimit(reserveMemLimit))
			})
			if !errors.Is(err, errOperationLimit) {
				t.Fatalf("%s = %v; want errOperationLimit", tt.src, err)
			}
			if allocated > ceiling {
				t.Errorf("%s allocated %d bytes before being refused; want under %d.\n"+
					"A multi-sub-range range_expr must be refused from rangeExprCountBounds' "+
					"arithmetic, never by expanding to find out -- see reserveRangeExprExpansion.",
					tt.src, allocated, ceiling)
			}
		})
	}
}

// TestReserveRangeExpr_SuccessExpandsOnce pins that an accepted
// multi-sub-range expression is not expanded twice.
//
// It asserts a ratio against a single-sub-range control of the same value
// count rather than an absolute byte figure. The control's reservation is
// exact arithmetic and never expands, so it measures the one expansion
// the operation needs; the multi-sub-range case must stay in the
// same ballpark. A regression to reserving via rangeExprCount doubles the
// expansion and pushes the ratio well past 2.
func TestReserveRangeExpr_SuccessExpandsOnce(t *testing.T) {
	// 900,001 values in two sub-ranges, against 900,000 in one. Sized to be
	// within the default operation limit so both succeed -- the success path
	// is what this test measures.
	const multi = `len(list(range_expr(` + multiSubRangeAccepted + `)))`
	const single = `len(list(range_expr("1-900000")))`

	measure := func(src string, want int64) uint64 {
		// One warm-up outside the measurement: the first evaluation in a
		// process pays for lazily-built package state that has nothing to do
		// with the expansion under test.
		if _, err := Eval(src, MapSymbols(nil), TAny); err != nil {
			t.Fatalf("%s: unexpected error %v", src, err)
		}
		var v Value
		var err error
		alloc := allocDelta(func() { v, err = Eval(src, MapSymbols(nil), TAny) })
		if err != nil {
			t.Fatalf("%s: unexpected error %v", src, err)
		}
		if got := v.AsInt(); got != want {
			t.Fatalf("%s = %d; want %d", src, got, want)
		}
		return alloc
	}

	singleAlloc := measure(single, 900_000)
	multiAlloc := measure(multi, 900_001)

	ratio := float64(multiAlloc) / float64(singleAlloc)
	t.Logf("multi-sub-range alloc=%d bytes, single-sub-range control alloc=%d bytes, ratio=%.3f",
		multiAlloc, singleAlloc, ratio)

	// The multi-sub-range path legitimately costs more than the control even
	// when it expands once -- expandRanges carries a deduplication map the
	// single-sub-range path does not (expandOneRange's own doc comment): a
	// single expansion reads a ratio near 1.2 and a double expansion near
	// 1.7. 1.45 sits between them with room on both sides.
	if ratio > 1.45 {
		t.Errorf("the multi-sub-range success path allocated %.0f%% of the single-sub-range "+
			"control's (%d vs %d bytes); want well under double.\n"+
			"This is what a reservation that counts by EXPANDING looks like: the range is "+
			"expanded once to size the reservation and again to produce the value. The "+
			"reservation must come from rangeExprCountBounds' arithmetic instead.",
			ratio*100, multiAlloc, singleAlloc)
	}
}

// TestReserveRangeExpr_CoercionAndEqualityDoNotExpand pins the three
// expansion sites that no charge sits in front of, so that a reservation at a
// charge cannot cover them.
//
// Without a reservation of their own, each one runs to completion and is
// caught (if at all) only afterwards:
//
//   - equality. Section 1.3.10 charges a list/range comparison against the
//     left operand only -- an adjudicated ruling
//     (TestOperationCount_ListEquality) -- but listOrRangeEqual expands
//     whichever side is a range_expr. Unreserved, a right-hand one has nothing
//     in front of it: [1] == range_expr("1-5000000,6000000-9000000") would
//     expand eight million values, return false, and charge three operations.
//
//   - argument coercion. callShape's coercion loop runs before chargeArgs, so
//     a range_expr coerced to a list[int] parameter is expanded and only then
//     billed: unreserved, sorted([1] + range_expr("1-10000000")) would expand
//     ten million values before reporting 10,000,003 operations against a
//     limit of 10,000.
//
//   - target coercion. coerceTop expands a range_expr meeting a list[int]
//     target with no charge at all; unreserved, the result's own alloc would
//     catch it only after the list existed. evalListLit's element coercion is
//     the same rule one level down ("[range_expr('1-10000000')]" against a
//     list[list[int]] target) and is covered by the same helper.
//
// The reservations are check-only, so the left-only charging ruling and every
// operation count are untouched -- see reserveEqualityExpansion and
// reserveRangeExprCoercion (rangeexpr.go).
func TestReserveRangeExpr_CoercionAndEqualityDoNotExpand(t *testing.T) {
	const ceiling = 1 << 20

	tests := []struct {
		name   string
		src    string
		target Type
	}{
		{"equality, range on the right", `[1] == range_expr(` + multiSubRangeRefused + `)`, TAny},
		{"inequality, range on the right", `[1] != range_expr("1-10000000")`, TAny},
		{"range vs differently-spelled range", `range_expr("1-2") == range_expr(` + multiSubRangeRefused + `)`, TAny},
		{"argument coercion", `sorted([1] + range_expr("1-10000000"))`, TAny},
		{"target coercion", `range_expr(` + multiSubRangeRefused + `)`, ListOf(TInt)},
		{"list-literal element coercion", `[range_expr("1-10000000")]`, ListOf(ListOf(TInt))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			allocated := allocDelta(func() {
				_, err = Eval(tt.src, MapSymbols(nil), tt.target,
					WithOperationLimit(reserveOpLimit), WithMemoryLimit(reserveMemLimit))
			})
			if !errors.Is(err, errOperationLimit) {
				t.Fatalf("%s = %v; want errOperationLimit", tt.src, err)
			}
			if allocated > ceiling {
				t.Errorf("%s allocated %d bytes before being refused; want under %d.\n"+
					"An expansion with no charge in front of it still needs a RESERVATION in "+
					"front of it -- see reserveEqualityExpansion and reserveRangeExprCoercion.",
					tt.src, allocated, ceiling)
			}
		})
	}
}

// TestReserveEquality_IdenticalRangeTextStillCompares pins the one case
// reserveEqualityExpansion deliberately skips.
//
// listOrRangeEqual answers two range_exprs with identical text without
// expanding either side, so there is no work to reserve. Reserving it anyway
// would refuse a nine-million-value self-comparison that costs two parses and
// fits the default operation limit -- the same shape of false rejection that
// reserving len()'s single-sub-range count would have caused.
func TestReserveEquality_IdenticalRangeTextStillCompares(t *testing.T) {
	v, err := Eval(`range_expr("1-9000000") == range_expr("1-9000000")`, MapSymbols(nil), TAny)
	if err != nil {
		t.Fatalf("identical range text expands nothing, so it must still compare: %v", err)
	}
	if !v.AsBool() {
		t.Error("a range_expr must equal itself")
	}
}

// TestReserveRangeExprCount_SingleSubRangeStaysFree pins the property that
// separating reserveRangeExprCount from reserveRangeExprExpansion exists to
// protect.
//
// len() over a single sub-range is pure arithmetic on the parsed bounds: it
// allocates nothing and expands nothing, so there is no work for a
// reservation to refuse. Reserving the count as though it were the expansion
// makes len(range_expr('1-20000000')) fail under any budget below 20,000,000 --
// rejecting the exact query the arithmetic path exists to serve, and the one
// funcsconv.go's len row and TestLenRangeExpr_DoesNotMaterialize both name.
func TestReserveRangeExprCount_SingleSubRangeStaysFree(t *testing.T) {
	v, err := Eval(`len(range_expr("1-20000000"))`, MapSymbols(nil), TAny,
		WithOperationLimit(reserveOpLimit), WithMemoryLimit(reserveMemLimit))
	if err != nil {
		t.Fatalf("counting a single sub-range materializes nothing, so a %d-operation budget "+
			"must not refuse it: %v", reserveOpLimit, err)
	}
	if got := v.AsInt(); got != 20_000_000 {
		t.Errorf("len(range_expr(\"1-20000000\")) = %d; want 20000000", got)
	}
}
