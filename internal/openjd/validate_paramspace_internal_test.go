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
// enough that [intRangeHasOverlap]'s O(n^2) pairwise scan (range.go) is
// visible in allocation volume, deliberately worth exactly n values so the
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

// TestParameterSpaceOverCaps_DoesNotRunOverlapScan is a white-box regression
// test for a cost defect a review caught in E4c Task 1: an earlier revision
// of [parameterSpaceOverCaps] called [validateParameterSpaceLimits]
// wholesale, which also runs [intRangeHasOverlap] -- an O(n^2) pairwise scan
// over an INT range's sub-ranges, with no early exit once a count cap has
// already fired. A caller that reaches both parameterSpaceOverCaps (gating
// the expression walk, unconditionally) AND validateLimits (under
// EnforceLimits: true) paid that scan TWICE. Measured with the bug present:
// 8,000 non-overlapping sub-ranges went from 84ms to 148ms under
// EnforceLimits: true (roughly 2x), and 32,000 sub-ranges (186 KB) went from
// ~0 to ~1.97s under EnforceLimits: false, where nothing used to run here at
// all.
//
// Asserts an ALLOCATION ratio, not a wall-clock threshold -- this package's
// own established technique for a cost property that must not flake on
// shared CI (see validate_exprgate_test.go's totalAllocDelta).
// parseIntRangeElements allocates a slice proportional to the sub-range
// count on every call it services, so running the parse-and-scan TWICE
// (the pre-narrowing shape) costs roughly double the parse allocation that
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

	// A regression to the pre-narrowing shape (parameterSpaceOverCaps calling
	// validateParameterSpaceLimits wholesale, paying the parse-and-scan
	// twice per caller) would make overCapAlloc close to fullAlloc. The
	// narrowed shape -- two O(n) count checks, one parse via
	// taskParamValueCount -- should cost a small fraction of
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
