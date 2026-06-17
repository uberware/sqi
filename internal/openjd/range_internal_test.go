// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"testing"
	"time"
)

// TestParseIntRangeExpr_HugeRange is the resource-exhaustion regression guard:
// a range whose arithmetic value count exceeds maxRangeValues must return an
// error WITHOUT materializing the slice (no OOM), and must do so quickly.
func TestParseIntRangeExpr_HugeRange(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"contiguous huge", "1-2000000000"},
		{"stepped huge", "1-2000000000:2"},
		{"summed across elements huge", "1-9000000,1-9000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				if _, err := parseIntRangeExpr(tc.expr); err == nil {
					t.Errorf("parseIntRangeExpr(%q): expected error, got nil", tc.expr)
				}
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("parseIntRangeExpr(%q) did not return promptly (possible OOM/unbounded allocation)", tc.expr)
			}
		})
	}
}

// TestIntRangeCount checks the allocation-free arithmetic count.
func TestIntRangeCount(t *testing.T) {
	cases := []struct {
		r    intRange
		want int
	}{
		{intRange{Start: 1, End: 1, Step: 1}, 1},
		{intRange{Start: 1, End: 5, Step: 1}, 5},
		{intRange{Start: 1, End: 10, Step: 2}, 5},
		{intRange{Start: 1, End: 10, Step: 3}, 4},
		{intRange{Start: 0, End: 0, Step: 0}, 1},   // step normalized to 1
		{intRange{Start: 5, End: 1, Step: 1}, 0},   // start > end
		{intRange{Start: -5, End: 5, Step: 1}, 11}, // negatives
	}
	for _, tc := range cases {
		if got := tc.r.count(); got != tc.want {
			t.Errorf("intRange%+v.count() = %d, want %d", tc.r, got, tc.want)
		}
		// count() must match the materialized length for in-bounds ranges.
		if got := len(tc.r.iterate()); tc.want != got {
			t.Errorf("intRange%+v: count()=%d but len(iterate())=%d", tc.r, tc.want, got)
		}
	}
}

// TestParseIntRangeExpr_InBoundsUnchanged confirms normal ranges still expand
// exactly (dedupe + ordering preserved).
func TestParseIntRangeExpr_InBoundsUnchanged(t *testing.T) {
	cases := []struct {
		expr string
		want []int
	}{
		{"1-5", []int{1, 2, 3, 4, 5}},
		{"1-10:2", []int{1, 3, 5, 7, 9}},
		{"1,5,10", []int{1, 5, 10}},
		{"1-5,3-7", []int{1, 2, 3, 4, 5, 6, 7}}, // dedupe across overlap
		{"-2-2", []int{-2, -1, 0, 1, 2}},
	}
	for _, tc := range cases {
		got, err := parseIntRangeExpr(tc.expr)
		if err != nil {
			t.Fatalf("parseIntRangeExpr(%q): unexpected error %v", tc.expr, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseIntRangeExpr(%q) = %v, want %v", tc.expr, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parseIntRangeExpr(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		}
	}
}

// TestValidateIntRangeExpr_MatchesParse locks in the equivalence that lets the
// submit path use validateIntRangeExpr instead of parseIntRangeExpr: for any
// expression cheap enough to fully expand, the two must agree on success/failure
// and produce byte-identical error strings. (Large in-bounds ranges, where
// parseIntRangeExpr would materialize, are covered separately below.)
func TestValidateIntRangeExpr_MatchesParse(t *testing.T) {
	exprs := []string{
		"1-5",
		"1-10:2",
		"1,5,10",
		"1-5,3-7",      // dedupe across overlap
		"-2-2",         // negatives
		"bad-",         // parse error
		"5-1",          // start > end parse error
		",,",           // all-empty elements -> "produced no values"
		"",             // empty expression
		"1-2000000000", // too large -> errRangeTooLarge (guarded before allocation)
	}
	for _, expr := range exprs {
		t.Run(expr, func(t *testing.T) {
			_, perr := parseIntRangeExpr(expr)
			verr := validateIntRangeExpr(expr)
			switch {
			case perr == nil && verr != nil:
				t.Fatalf("validateIntRangeExpr(%q) = %v, want nil (parseIntRangeExpr succeeded)", expr, verr)
			case perr != nil && verr == nil:
				t.Fatalf("validateIntRangeExpr(%q) = nil, want error %q", expr, perr.Error())
			case perr != nil && verr != nil && perr.Error() != verr.Error():
				t.Fatalf("validateIntRangeExpr(%q) error = %q, want %q (identical to parseIntRangeExpr)", expr, verr.Error(), perr.Error())
			}
		})
	}

	// The too-large failure must wrap errRangeTooLarge so callers can match it.
	if err := validateIntRangeExpr("1-2000000000"); !errors.Is(err, errRangeTooLarge) {
		t.Errorf("validateIntRangeExpr huge: error = %v, want wrapping errRangeTooLarge", err)
	}
}

// TestValidateIntRangeExpr_LargeInBoundsNoMaterialize is the efficiency
// regression guard: a range whose value count equals maxRangeValues is valid but
// must validate WITHOUT materializing the ~maxRangeValues-element slice (the
// reason the submit path calls validateIntRangeExpr rather than
// parseIntRangeExpr). It must return nil promptly.
func TestValidateIntRangeExpr_LargeInBoundsNoMaterialize(t *testing.T) {
	const expr = "1-10000000" // exactly maxRangeValues values: in bounds, valid
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = validateIntRangeExpr(expr)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("validateIntRangeExpr(%q) did not return promptly (slice materialized?)", expr)
	}
	if err != nil {
		t.Fatalf("validateIntRangeExpr(%q) = %v, want nil", expr, err)
	}
}

// TestIntRangeHasOverlap_NonMaterializing covers the overlap detector,
// including stepped ranges that must NOT be reported as overlapping, and a
// huge range that must be evaluated without OOM.
func TestIntRangeHasOverlap_Internal(t *testing.T) {
	cases := []struct {
		expr        string
		wantOverlap bool
		wantErr     bool
	}{
		{"1-5,3-7", true, false},  // overlapping sub-ranges
		{"1-5,5-10", true, false}, // share endpoint 5
		{"1-5,6-10", false, false},
		{"1,2,3,2", true, false},        // duplicate single value
		{"1-10:2,2-10:2", false, false}, // stepped, disjoint despite overlapping intervals
		{"1-10:2,1-10:2", true, false},  // identical stepped ranges
		{"1-10:2,3-12:2", true, false},  // stepped, same step but different alignment, shares 3,5,7,9
		{"{{Param.N}}", false, false},   // format ref skipped
		{"1-2000000000", false, false},  // huge but disjoint single element: no overlap, no OOM
		{"bad-", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			done := make(chan struct{})
			var overlap bool
			var err error
			go func() {
				defer close(done)
				overlap, err = intRangeHasOverlap(tc.expr)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("intRangeHasOverlap(%q) did not return promptly", tc.expr)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("intRangeHasOverlap(%q): expected error", tc.expr)
				}
				return
			}
			if err != nil {
				t.Fatalf("intRangeHasOverlap(%q): unexpected error %v", tc.expr, err)
			}
			if overlap != tc.wantOverlap {
				t.Fatalf("intRangeHasOverlap(%q) = %v, want %v", tc.expr, overlap, tc.wantOverlap)
			}
		})
	}
}
