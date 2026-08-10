// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"runtime"
	"testing"
)

// This file pins meter.reserve: the rule that a BULK operation must be
// refused BEFORE it allocates, not billed after it has.
//
// The defect it exists to prevent (EXPR sub-project E4c, whole-branch review,
// Critical 1): every Cost.ResultElements/Cost.ResultBytes charge is levied by
// chargeResult (ops.go) once the produced value exists, so under the server's
// submission budget of 10,000 operations, "[0] * 10000000" materialized ten
// million elements -- 1.1 GB of allocator traffic, 108 ms -- and only THEN
// reported "10000001 operations exceeds the limit of 10000". The limit
// overshot by a factor of a THOUSAND, and the only thing that actually stopped
// the expression was limits.go's fixed maxElements floor three orders of
// magnitude above it. The template-wide budget (internal/openjd/exprcheck.go)
// derives its cumulative operation ceiling from that per-evaluation limit, so
// the derived ceiling was false by the same factor.
//
// TWO PROPERTIES ARE PINNED, and they are separate tests on purpose:
//
//   - a refusal costs no allocation (TestReserve_BulkOperationsRefuseBeforeAllocating)
//   - a SUCCESS is charged exactly what it was charged before reserve existed
//     (TestReserve_DoesNotChangeSuccessfulCounts)
//
// The second is the one that keeps this change invisible to the differential
// oracle (test/oracle), whose operation counts are compared only on cases
// whose values agree -- i.e. only on successful evaluations. reserve adds
// nothing to the running total; it only refuses when a later charge of the
// same figure would have refused anyway.

// reserveOpLimit and reserveMemLimit mirror internal/openjd's submissionLimits
// (submissionOperationLimit, submissionMemoryLimit) -- the budget the server
// actually applies at validate and submit, and the one the overshoot was
// measured against. Restated as literals rather than imported: internal/openjd
// imports this package, not the other way round.
const (
	reserveOpLimit  int64 = 10_000
	reserveMemLimit int64 = 1_000_000
)

// allocCeilingBytes is the per-expression allocation budget the refusal test
// allows. The pre-fix figures for these expressions were 1.1-1.6 GB; the
// post-fix figures are under 1 MB. 16 MB sits two orders of magnitude below
// the defect and one above the fix, so this asserts the DEFECT is gone
// without pinning an exact allocator figure that a Go release could move.
const allocCeilingBytes = 16 << 20

func TestReserve_BulkOperationsRefuseBeforeAllocating(t *testing.T) {
	// Every expression here amplifies: a handful of source bytes asks for up
	// to maxElements elements or maxStringBytes bytes, far past the 10,000
	// operations the budget allows. Each must be refused on arithmetic alone.
	tests := []struct {
		name string
		src  string
	}{
		{"list repetition", `[0] * 10000000`},
		{"range", `range(0, 10000000)`},
		{"list of a range_expr", `list(range_expr("1-10000000"))`},
		{"comprehension over a range_expr", `[x for x in range_expr("1-10000000")]`},
		{"string repetition", `"x" * 10000000`},
		{"ljust", `"".ljust(10000000)`},
		{"center", `"".center(10000000)`},
		{"zfill", `(1).zfill(10000000)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := Eval(tt.src, MapSymbols(nil), TAny,
				WithOperationLimit(reserveOpLimit), WithMemoryLimit(reserveMemLimit))
			runtime.ReadMemStats(&after)

			if !errors.Is(err, errOperationLimit) {
				t.Fatalf("%s = %v; want errOperationLimit", tt.src, err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > allocCeilingBytes {
				t.Errorf("%s allocated %d bytes before being refused; want under %d.\n"+
					"The refusal must come from the ARITHMETIC count, before the value is built -- "+
					"see meter.reserve.", tt.src, allocated, allocCeilingBytes)
			}
		})
	}
}

func TestReserve_DoesNotChangeSuccessfulCounts(t *testing.T) {
	// The operation counts here are the ones the differential oracle already
	// agrees with the reference implementation on (see
	// cost_ops_internal_test.go and cost_eval_internal_test.go). Restated in
	// this file so that a reserve() that wrongly ACCUMULATED, or that was
	// placed where it double-charges, fails here as well as in the oracle --
	// which needs python3 and does not run in `make test`.
	tests := []struct {
		src  string
		want int64
	}{
		{`[1, 2] * 3`, 7},                  // 1 call + 6 produced elements
		{`range(10)`, 11},                  // 1 call + 10 produced elements
		{`range(0, 10, 2)`, 6},             // 1 call + 5 produced elements
		{`list(range_expr("1-100"))`, 102}, // 1 (range_expr) + 1 (list call) + 100 elements
		// 1 (range call) + 5 (range's produced elements) + 5 (the
		// comprehension iterating them). Each of the three figures below was
		// read off the PRE-FIX evaluator and is reproduced here unchanged.
		{`[x for x in range(0, 5)]`, 11},
		{`"ab" * 3`, 2},       // 1 call + ceil(6/256)
		{`"a".ljust(300)`, 3}, // 1 call + ceil(300/256) + the receiver's own byte charge
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			_, ops, err := EvalWithMetrics(tt.src, MapSymbols(nil), TAny)
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tt.src, err)
			}
			if ops != tt.want {
				t.Errorf("%s charged %d operations; want %d. reserve() must never ADD to the "+
					"running total -- if this moved, every oracle count moved with it.", tt.src, ops, tt.want)
			}
		})
	}
}

// TestReserve_DoesNotTightenTheLanguage pins the other half of the bargain:
// the point of reserve is to stop the OVERSHOOT, not to make the language
// stricter. An expression that genuinely needs millions of operations must
// still succeed under the specification's own default limit (10,000,000
// operations, section 1.3.10), exactly as it did before.
func TestReserve_DoesNotTightenTheLanguage(t *testing.T) {
	v, ops, err := EvalWithMetrics(`sum(range(0, 1000000))`, MapSymbols(nil), TAny)
	if err != nil {
		t.Fatalf("a million-element sum under the DEFAULT operation limit must succeed; got %v", err)
	}
	if got := v.AsInt(); got != 499999500000 {
		t.Errorf("sum(range(0, 1000000)) = %d; want 499999500000", got)
	}
	if ops <= 1_000_000 {
		t.Errorf("charged %d operations; expected well over a million -- if this is small, the "+
			"expression is not exercising the headroom this test exists to protect", ops)
	}
}
