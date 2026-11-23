// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// bigNonOverlappingIntRangeExpr returns a range expression of n
// non-overlapping single-value sub-ranges ("0-0,1-1,2-2,...") -- large
// enough that [intRangeHasOverlap]'s parse and sort (range.go) are
// visible in allocation volume, worth exactly n values so the
// package's count caps never fire on their own.
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
// wholesale, which also runs [intRangeHasOverlap] -- a parse and sort of an
// INT range's sub-ranges, with no early exit once a count cap has already
// fired. A caller that reaches both parameterSpaceOverCaps (gating the
// expression walk, unconditionally) and validateLimits (under EnforceLimits:
// true) would pay that work twice, and under EnforceLimits: false it would
// pay it where nothing else runs here at all.
//
// Asserts an ALLOCATION ratio, not a wall-clock threshold -- this package's
// own established technique for a cost property that must not flake on
// shared CI. parseIntRangeElements allocates a slice proportional to the
// sub-range count on every call it services, so running the parse-and-scan
// TWICE (the wholesale shape) costs roughly double the parse allocation that
// validateParameterSpaceLimits pays on its own; a ratio well under that is
// direct evidence the narrowed parameterSpaceOverCaps never reaches
// intRangeHasOverlap at all.
func TestParameterSpaceOverCaps_DoesNotRunOverlapScan(t *testing.T) {
	// Exactly at maxTaskParamValues (each sub-range is worth one value), so
	// the count checks parameterSpaceOverCaps DOES run never fire on their
	// own -- the only thing under measurement is whether the overlap scan
	// runs, not whether the count caps do.
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

	// A regression to the wholesale shape (parameterSpaceOverCaps calling
	// validateParameterSpaceLimits wholesale, paying the parse-and-scan
	// twice per caller) would make overCapAlloc close to fullAlloc. The
	// narrowed shape -- count checks that never parse a range expression
	// (listFormValueCount) -- should cost a small fraction of
	// validateParameterSpaceLimits' own allocation (one parse for the same
	// count check, PLUS a second parse and scan for intRangeHasOverlap),
	// since it never reaches the overlap scan's own parse at all.
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
