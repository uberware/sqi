// SPDX-License-Identifier: AGPL-3.0-or-later

package intrange_test

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd/intrange"
)

func expand(t *testing.T, s string) []int {
	t.Helper()
	ranges, err := intrange.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error %v", s, err)
	}
	var out []int
	for _, r := range ranges {
		out = append(out, r.Iterate()...)
	}
	return out
}

func TestParse_SpecTable(t *testing.T) {
	tests := []struct {
		expr string
		want []int
	}{
		{"1 - 5", []int{1, 2, 3, 4, 5}},
		{"1 - -1", []int{1}},
		{"-1 - 1", []int{-1, 0, 1}},
		{"1-5:2", []int{1, 3, 5}},
		{"10-15:2,1-5", []int{10, 12, 14, 1, 2, 3, 4, 5}},
		{"1-10:4", []int{1, 5, 9}},
		{"7", []int{7}},
		{"1,5,10", []int{1, 5, 10}},
		{"10-2:-3", []int{10, 7, 4}},
		{"1-10:-2", []int{1}},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got := expand(t, tc.expr)
			if len(got) != len(tc.want) {
				t.Fatalf("expand(%q) = %v, want %v", tc.expr, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("expand(%q) = %v, want %v", tc.expr, got, tc.want)
				}
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []string{"", "   ", "bad-", "1-x", "1-10:0", "1-10:x", "7:2"}
	for _, expr := range tests {
		t.Run(expr, func(t *testing.T) {
			if _, err := intrange.Parse(expr); err == nil {
				t.Fatalf("Parse(%q) = nil error, want an error", expr)
			}
		})
	}
}

func TestParseWithPolicy_ReproducesOpenJDStrictness(t *testing.T) {
	tests := []struct {
		expr    string
		wantErr string
	}{
		{"5-1", `range start (5) must be ≤ end (1)`},
		{"1-10:-2", `invalid step "-2": must be a positive integer`},
		{"1-10: -2 ", `invalid step " -2": must be a positive integer`},
	}
	policy := intrange.Policy{PositiveStepOnly: true, AscendingOnly: true}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := intrange.ParseWithPolicy(tc.expr, policy)
			if err == nil {
				t.Fatalf("ParseWithPolicy(%q) = nil error, want %q", tc.expr, tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("ParseWithPolicy(%q) error = %q, want %q", tc.expr, err.Error(), tc.wantErr)
			}
		})
	}
	// The same inputs are accepted without the policy.
	for _, tc := range tests {
		if _, err := intrange.Parse(tc.expr); err != nil {
			t.Fatalf("Parse(%q) = %v, want nil (the spec permits it)", tc.expr, err)
		}
	}
}

func TestRange_CountMatchesIterate(t *testing.T) {
	tests := []intrange.Range{
		{Start: 1, End: 5, Step: 1},
		{Start: -5, End: 5, Step: 1},
		{Start: 1, End: 10, Step: 4},
		{Start: 1, End: -1, Step: 1},  // start > end: the set is {start}
		{Start: 10, End: 2, Step: -3}, // descending
		{Start: 1, End: 10, Step: -2}, // negative step, ascending bounds: {start}
	}
	for _, r := range tests {
		t.Run(r.String(), func(t *testing.T) {
			if got, want := r.Count(), len(r.Iterate()); got != want {
				t.Fatalf("%v: Count() = %d, len(Iterate()) = %d", r, got, want)
			}
		})
	}
}

func TestRange_CountSaturatesOnOverflow(t *testing.T) {
	r := intrange.Range{Start: -(1 << 62), End: 1 << 62, Step: 1}
	if got := r.Count(); got <= 0 {
		t.Fatalf("Count() = %d, want a large positive saturated value", got)
	}
}
