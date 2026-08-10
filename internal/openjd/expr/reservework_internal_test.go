// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"runtime"
	"testing"
)

// This file pins the property no operation count can see: how much WORK an
// evaluation does.
//
// Both defects it exists to catch were found by measurement and were invisible
// to every test in the suite, including the differential oracle, because the
// operation counts were byte-identical either side of them (EXPR sub-project
// E4c, whole-branch re-review, IMPORTANT A and B):
//
//   - A range_expr with TWO OR MORE sub-ranges cannot be counted
//     arithmetically -- rangeExprCount expands it to count it (rangeexpr.go).
//     So the reservation added to bound expansions was itself performing the
//     expansion it existed to refuse: len(range_expr(
//     "1-5000000,6000000-9000000")) answered after 645 ms and 687 MB while
//     charging TWO operations, and list(), a subscript and a comprehension
//     each paid the same 687 MB before refusing.
//
//   - On the SUCCESS path the same expression was then expanded a SECOND
//     time by the operation that had just been reserved for -- +73 MB and
//     +38 ms per call, a pure regression with no observable effect on any
//     count.
//
// Allocation deltas, not wall clock: this package's established technique for
// a cost property that must not flake on shared CI (see
// internal/openjd/validate_paramspace_internal_test.go's
// TestParameterSpaceOverCaps_DoesNotRunOverlapScan and
// validate_exprgate_test.go's totalAllocDelta).

// multiSubRangeRefused is an eight-million-value range_expr in two
// NON-OVERLAPPING sub-ranges. Non-overlap is what makes
// rangeExprCountBounds' lower bound (the largest single sub-range, 5,000,000)
// decisive on its own, so the refusal below needs no expansion to be exact.
const multiSubRangeRefused = `"1-5000000,6000000-9000000"`

// multiSubRangeAccepted is the same shape an order of magnitude smaller, so
// that it SUCCEEDS under the default budget and the success path's own
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

// TestReserveRangeExpr_RefusalDoesNotExpand pins finding A: a multi-sub-range
// range_expr that cannot fit the budget must be refused ARITHMETICALLY, with
// no expansion at all.
//
// The ceiling is 1 MB against a defect that allocated 687 MB, so it is nearly
// three orders of magnitude below the regression and comfortably above the
// parse-and-bounds arithmetic the fixed path actually does.
func TestReserveRangeExpr_RefusalDoesNotExpand(t *testing.T) {
	const ceiling = 1 << 20

	tests := []struct {
		name string
		src  string
	}{
		// len() is first because it is the worst case: section 1.3.10 exempts
		// len() from charging, so this expression cost 687 MB while reporting
		// TWO operations -- no budget in the package could see it.
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

// TestReserveRangeExpr_SuccessExpandsOnce pins finding B: an accepted
// multi-sub-range expression must not be expanded twice.
//
// It asserts a RATIO against a single-sub-range control of the same value
// count rather than an absolute byte figure. The control's reservation is
// exact arithmetic and has never expanded, so it measures the one expansion
// the operation genuinely needs; the multi-sub-range case must stay in the
// same ballpark. A regression to reserving via rangeExprCount doubles the
// expansion and pushes the ratio well past 2.
func TestReserveRangeExpr_SuccessExpandsOnce(t *testing.T) {
	// 900,001 values in two sub-ranges, against 900,000 in one. Sized to be
	// within the default operation limit so both SUCCEED -- the success path
	// is the whole point of this test.
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
	// single-sub-range path does not (expandOneRange's own doc comment). The
	// measured figures are ~175 MB against ~144 MB, a ratio near 1.2; the
	// double-expansion regression measured ~249 MB, a ratio near 1.7. 1.45
	// sits between them with room on both sides.
	if ratio > 1.45 {
		t.Errorf("the multi-sub-range success path allocated %.0f%% of the single-sub-range "+
			"control's (%d vs %d bytes); want well under double.\n"+
			"This is what a reservation that counts by EXPANDING looks like: the range is "+
			"expanded once to size the reservation and again to produce the value. The "+
			"reservation must come from rangeExprCountBounds' arithmetic instead.",
			ratio*100, multiAlloc, singleAlloc)
	}
}

// TestReserveRangeExprCount_SingleSubRangeStaysFree pins the property that
// separating reserveRangeExprCount from reserveRangeExprExpansion exists to
// protect, and that a first draft of this fix broke.
//
// len() over a SINGLE sub-range is pure arithmetic on the parsed bounds: it
// allocates nothing and expands nothing, so there is no work for a
// reservation to refuse. Reserving the COUNT as though it were the expansion
// made len(range_expr('1-20000000')) fail under any budget below 20,000,000 --
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
