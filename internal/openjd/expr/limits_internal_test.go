// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"math"
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
