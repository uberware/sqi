// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// bigNonOverlappingIntRangeExpr returns a range expression of n
// non-overlapping single-value sub-ranges ("0-0,1-1,2-2,..."), large
// enough that [intRangeHasOverlap]'s parse and sort (range.go) show in
// allocation volume, and worth exactly n values so the package's count
// caps never fire on their own.
func bigNonOverlappingIntRangeExpr(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d-%d", i, i)
	}
	return b.String()
}

// TestParameterSpaceOverCaps_DoesNotRunOverlapScan is a white-box test that
// [parameterSpaceOverCaps] does not call [validateParameterSpaceLimits]
// wholesale, which also runs [intRangeHasOverlap]: a parse and sort of an
// INT range's sub-ranges, with no early exit once a count cap has fired. A
// caller that reaches both parameterSpaceOverCaps (gating the expression
// walk, regardless of EnforceLimits) and validateLimits (under EnforceLimits:
// true) would pay that work twice, and under EnforceLimits: false it would
// pay it where nothing else runs it.
//
// It asserts an allocation ratio rather than a wall-clock threshold, so it
// does not flake on shared CI. parseIntRangeElements allocates a slice
// proportional to the sub-range count on every call, so running the parse and
// scan twice (the wholesale shape) costs roughly double the parse allocation
// that validateParameterSpaceLimits pays on its own; a ratio well under that
// shows parameterSpaceOverCaps never reaches intRangeHasOverlap.
func TestParameterSpaceOverCaps_DoesNotRunOverlapScan(t *testing.T) {
	// Exactly at maxTaskParamValues (each sub-range is worth one value), so
	// the count checks parameterSpaceOverCaps does run never fire on their
	// own: the test measures whether the overlap scan runs, not the count caps.
	const n = maxTaskParamValues
	expr := bigNonOverlappingIntRangeExpr(n)
	ps := StepParameterSpace{
		TaskParameterDefinitions: []TaskParamDefinition{
			{Name: "F", Type: TaskParamTypeInt, RangeExpr: &expr},
		},
	}
	tmpl := &JobTemplate{Steps: []StepTemplate{{ParameterSpace: &ps}}}

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	overCap := parameterSpaceOverCaps(tmpl)
	runtime.ReadMemStats(&after)
	overCapAlloc := after.TotalAlloc - before.TotalAlloc

	runtime.ReadMemStats(&before)
	errs := validateParameterSpaceLimits(ps, "/steps/0/parameterSpace")
	runtime.ReadMemStats(&after)
	fullAlloc := after.TotalAlloc - before.TotalAlloc

	if overCap {
		t.Fatalf("parameterSpaceOverCaps must not flag %d non-overlapping single-value sub-ranges as over-cap", n)
	}
	if len(errs) != 0 {
		t.Fatalf("validateParameterSpaceLimits must not flag %d non-overlapping single-value sub-ranges either, got %v", n, errs)
	}

	// If parameterSpaceOverCaps called validateParameterSpaceLimits
	// wholesale, paying the parse and scan twice per caller, overCapAlloc
	// would be close to fullAlloc. Its count checks never parse a range
	// expression (listFormValueCount), so it should cost a small fraction of
	// validateParameterSpaceLimits' allocation (one parse for the same count
	// check, plus a second parse and scan for intRangeHasOverlap).
	ratio := float64(overCapAlloc) / float64(fullAlloc)
	t.Logf("parameterSpaceOverCaps alloc=%d bytes, validateParameterSpaceLimits alloc=%d bytes, ratio=%.3f",
		overCapAlloc, fullAlloc, ratio)
	if ratio > 0.65 {
		t.Fatalf("parameterSpaceOverCaps appears to run the overlap scan: "+
			"its own allocation (%d bytes) is %.0f%% of validateParameterSpaceLimits' (%d bytes); "+
			"want it well under half, evidence the parse-and-scan is not paid twice",
			overCapAlloc, ratio*100, fullAlloc)
	}
}
