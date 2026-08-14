// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"math"
	"runtime"
	"strings"
	"testing"
)

func TestCheckElementCount(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{"zero", 0, false},
		{"one", 1, false},
		{"at the limit", maxElements, false},
		{"one over", maxElements + 1, true},
		{"negative means overflow", -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkElementCount(tc.n)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkElementCount(%d) = %v, wantErr %v", tc.n, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, errTooLarge) {
				t.Fatalf("checkElementCount(%d) = %v, want it to wrap errTooLarge", tc.n, err)
			}
		})
	}
}

func TestCheckStringBytes(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{"zero", 0, false},
		{"one", 1, false},
		{"at the limit", maxStringBytes, false},
		{"one over", maxStringBytes + 1, true},
		{"negative means overflow", -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStringBytes(tc.n)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkStringBytes(%d) = %v, wantErr %v", tc.n, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, errTooLarge) {
				t.Fatalf("checkStringBytes(%d) = %v, want it to wrap errTooLarge", tc.n, err)
			}
		})
	}
}

// TestCheckRepeat_OverflowSafe pins the class of bug a Critical review found
// in repeatList/repeatString: computing unitSize*n and checking the PRODUCT,
// rather than checking the OPERANDS first, lets a large enough unitSize and n
// overflow int64 and wrap — sometimes to a negative number (an allocation
// would then panic), sometimes to a small positive number (the bound would
// then silently pass and a caller looping on the raw, un-wrapped n would hang
// or exhaust memory). Every case here uses operands large enough that the
// naive "multiply first" check would get the wrong answer, to prove
// checkRepeat itself never forms the unsafe product.
func TestCheckRepeat_OverflowSafe(t *testing.T) {
	tests := []struct {
		name      string
		unitSize  int
		n         int64
		max       int
		wantErr   bool
		wantTotal int64 // only meaningful when wantErr is false
	}{
		{"zero unit size never overflows and needs no bound", 0, math.MaxInt64, 10, false, 0},
		{"zero n never overflows and needs no bound", 10, 0, 10, false, 0},
		{"negative n needs no bound", 10, -1, 10, false, 0},
		{"at the limit exactly", 5, 2, 10, false, 10},
		{"one over the limit", 5, 3, 14, true, 0},
		// 2 * math.MaxInt64 overflows int64 and wraps to -2 — a naive
		// "product > max" check would see -2 > maxElements as false and
		// wrongly admit it.
		{"wraps to negative", 2, math.MaxInt64, 10_000_000, true, 0},
		// 1_048_576 (2^20) * 17_592_186_044_416 (2^44) is exactly 2^64, which
		// wraps to 0 in int64 — a naive check would see 0 > maxElements as
		// false and wrongly admit it, and a caller that then looped on the
		// raw n would iterate 2^44 times.
		{"wraps to zero", 1_048_576, 17_592_186_044_416, 10_000_000, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total, err := checkRepeat(tc.unitSize, tc.n, tc.max)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkRepeat(%d, %d, %d) = (%d, %v), wantErr %v",
					tc.unitSize, tc.n, tc.max, total, err, tc.wantErr)
			}
			if err != nil {
				if !errors.Is(err, errTooLarge) {
					t.Fatalf("checkRepeat(%d, %d, %d) = %v, want it to wrap errTooLarge", tc.unitSize, tc.n, tc.max, err)
				}
				return
			}
			if total != tc.wantTotal {
				t.Errorf("checkRepeat(%d, %d, %d) total = %d, want %d", tc.unitSize, tc.n, tc.max, total, tc.wantTotal)
			}
		})
	}
}

// TestParse_RejectsAnOversizedSourceBeforeItCosts is the regression test for
// the parse-time denial of service maxSourceBytes exists to close: every other
// bound in this package and its callers lives DOWNSTREAM of Parse, so before
// this limit a single expression could allocate hundreds of megabytes with no
// budget of any kind consulted.
//
// The construction is the exact shape that defeats maxParseDepth — a flat,
// left-associative chain, which parseBinaryLevel reads in a LOOP and so costs
// no recursion at all. Measured on the machine this was written on with the
// bound removed: 4,000,001 bytes of "1+1+1+…" parsed SUCCESSFULLY in 544 ms,
// holding 427.6 MB of live heap and churning 1,403.3 MB in total.
//
// THE ASSERTION ON ALLOCATION IS THE POINT, not the error. A regression that
// moved this check after tokenize, or dropped it and let some later bound
// report a different failure, would still return an error here — it would just
// spend a gigabyte first. Allocation is asserted rather than elapsed time
// because it is machine-independent: the 1 MiB budget below is three orders of
// magnitude under the measured cost of the parse this test forbids, so no
// plausible machine makes it flaky in either direction.
func TestParse_RejectsAnOversizedSourceBeforeItCosts(t *testing.T) {
	src := "1" + strings.Repeat("+1", 2_000_000)
	if len(src) <= maxSourceBytes {
		t.Fatalf("test construction is %d bytes, which is not over the %d-byte limit", len(src), maxSourceBytes)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := Parse(src)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	if err == nil {
		t.Fatalf("Parse accepted a %d-byte expression", len(src))
	}
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("Parse error = %v, want it to wrap errTooLarge", err)
	}
	const budget = 1 << 20 // 1 MiB
	if allocated > budget {
		t.Errorf("rejecting a %d-byte expression allocated %d bytes, want at most %d: "+
			"the bound is no longer applying BEFORE the source is tokenized",
			len(src), allocated, budget)
	}
}

// TestParse_AcceptsTheLargestRealisticExpressions is maxSourceBytes' other
// half: a bound that rejects real templates is worse than the hazard it
// closes.
//
// Every case is a VERBATIM expression from a vendored conformance fixture, an
// official sample or one of sqi's own reference presets — the largest found by
// scanning every {{ }} body and every let: binding in
// third_party/openjd-specifications/{conformance-tests,samples} and presets/,
// 1,401 expressions in all. The longest is 99 bytes.
func TestParse_AcceptsTheLargestRealisticExpressions(t *testing.T) {
	// The four longest, and the longest single-line one.
	realistic := []string{
		// EXPR/jobs/expr1.1.7--multiline.test.yaml, 99 bytes.
		"[\n                x * 2\n                for x in [1, 2, 3]\n                if x > 1\n            ]",
		// EXPR/job_templates/expr1.1--multiline-expr.yaml, 93 bytes.
		"[\n              x * 2\n              for x in Param.Items\n              if x > 2\n          ]",
		// EXPR/job_templates/expr1.1--multiline-expr.yaml, 89 bytes.
		"\"high\"\n                     if Param.Quality == \"final\"\n                     else \"low\"",
		// samples/expr-test-job.yaml, 80 bytes.
		"Param.OutputDir / Param.InputFile.stem + '_' + string(Task.Param.Frame) + '.exr'",
		// EXPR/job_templates/expr1.3.10--operation-limit-exceeded.invalid.yaml, 79 bytes.
		// Rejected at EVALUATION for exceeding the operation limit, which is
		// exactly the point: it must still PARSE.
		"[len([i for i in [len(range(300)) for j in range(300)]]) for k in range(300)]",
	}

	longest := 0
	for _, src := range realistic {
		if _, err := Parse(src); err != nil {
			t.Errorf("Parse(%q) = %v, want it accepted", src, err)
		}
		if len(src) > longest {
			longest = len(src)
		}
	}

	// The headroom is the sizing argument, so it is asserted rather than left
	// in a comment: shrinking maxSourceBytes toward what real templates
	// actually contain fails here long before it starts rejecting them.
	if want := 50 * longest; maxSourceBytes < want {
		t.Errorf("maxSourceBytes = %d, want at least %d (50x the largest real expression, %d bytes)",
			maxSourceBytes, want, longest)
	}
}

// TestParse_SourceBoundIsInclusive pins the boundary itself, in both
// directions, on a source that is otherwise perfectly valid.
func TestParse_SourceBoundIsInclusive(t *testing.T) {
	// "1+1+1…" is 1 byte plus 2 per operator, so it lands on an odd length;
	// trailing spaces (which the lexer skips) make up the difference when
	// maxSourceBytes is even.
	atLimit := "1" + strings.Repeat("+1", (maxSourceBytes-1)/2)
	atLimit += strings.Repeat(" ", maxSourceBytes-len(atLimit))
	if len(atLimit) != maxSourceBytes {
		t.Fatalf("construction is %d bytes, want exactly %d", len(atLimit), maxSourceBytes)
	}
	if _, err := Parse(atLimit); err != nil {
		t.Errorf("Parse of a %d-byte expression = %v, want it accepted at the limit", len(atLimit), err)
	}
	if _, err := Parse(atLimit + "+1"); !errors.Is(err, errTooLarge) {
		t.Errorf("Parse of a %d-byte expression = %v, want errTooLarge", len(atLimit)+2, err)
	}
}
