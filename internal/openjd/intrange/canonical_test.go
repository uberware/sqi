// SPDX-License-Identifier: AGPL-3.0-or-later

package intrange

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCanonical_SpecificationTable pins Template Schemas §3.4.1.1.1's
// canonical form: the list of values is split, from the left, into runs of a
// constant difference; a run of three or more values is written x-y (step 1)
// or x-y:s, anything shorter one value at a time, joined by "," with no white
// space. Every row of the specification's own table is here, written in a form
// other than its canonical one where the table allows it.
func TestCanonical_SpecificationTable(t *testing.T) {
	tests := []struct{ in, want string }{
		{"5", "5"},
		{"1,2", "1,2"},
		{"1,2,3", "1-3"},
		{"1,3,5,7,9", "1-9:2"},
		{"3,7", "3,7"},
		{"1,3,4,5", "1,3-5"},
		{"1,2,4", "1,2,4"},
		{"1,2,3,5,7,9", "1-3,5-9:2"},
		{"2,4,6,8,9,10", "2-8:2,9,10"},
		{"13,14,15,16,17,18,19", "13-19"},
		{"-3,-2,-1", "-3--1"},
		{"-2,-1,3,4,5,7,8,9,12,100", "-2,-1,3-5,7-9,12,100"},
		// The specification's prose examples.
		{"1-10:4", "1-9:4"},
		{"5-1:-1", "1-5"},
		{"19-23:5,13-18", "13-19"},
		{"13-18,19", "13-19"},
		// Element order, how values are split into elements, and white space are
		// not part of the value.
		{"10-15:2,1-5", "1-5,10-14:2"},
		{" 1 - 3 , 5 ", "1-3,5"},
		{"1,2,3,10-15:2", "1-3,10-14:2"},
		{"1,3,5-7", "1-5:2,6,7"}, // runs are taken greedily from the left
		{"1,5-7", "1,5-7"},
		{"1-5:2,7-9", "1-7:2,8,9"},
		{"1-5:2,7", "1-7:2"},
		{"1,2,3,4,5,6", "1-6"},
		{"1-2,3-4", "1-4"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			rs, err := Parse(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := Canonical(rs)
			if !ok || got != tc.want {
				t.Fatalf("Canonical(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
			}
			// The canonical form is a valid <IntRangeExpr> naming the same
			// values, and canonicalizing it again changes nothing.
			again, err := Parse(got)
			if err != nil {
				t.Fatalf("Parse(canonical %q): %v", got, err)
			}
			if got2, ok := Canonical(again); !ok || got2 != got {
				t.Errorf("Canonical(Canonical(%q)) = %q, %v; want %q", tc.in, got2, ok, got)
			}
		})
	}
}

// TestCanonical_DoesNotListTheValues pins that the work is proportional to the
// number of elements, never to the number of values: a billion-value element,
// and many elements that each name a long run, return promptly.
func TestCanonical_DoesNotListTheValues(t *testing.T) {
	var b strings.Builder
	for i := range 100_000 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d-%d:3", i*1_000_000, i*1_000_000+999_999)
	}
	for _, tc := range []struct{ in, want string }{
		{"1-1000000000", "1-1000000000"},
		{"1-1000000000:2,1000000001", "1-1000000001:2"},
		{"-9223372036854775807-9223372036854775807", "-9223372036854775807-9223372036854775807"},
		{b.String(), ""},
	} {
		done := make(chan struct{})
		var got string
		var ok bool
		go func() {
			defer close(done)
			rs, err := Parse(tc.in)
			if err != nil {
				return
			}
			got, ok = Canonical(rs)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Canonical(%.40q...) did not return promptly", tc.in)
		}
		if !ok || (tc.want != "" && got != tc.want) {
			t.Errorf("Canonical(%.40q...) = %.60q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
}

// TestCanonical_DifferenceOverflowIsNotARun pins the specification's note that
// the difference between two consecutive values may not fit in 64 bits: such a
// pair can never start a run of three, so each value is written alone.
func TestCanonical_DifferenceOverflowIsNotARun(t *testing.T) {
	rs, err := Parse("-9223372036854775807,0,9223372036854775807")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := Canonical(rs); !ok || got != "-9223372036854775807-9223372036854775807:9223372036854775807" {
		t.Errorf("Canonical = %q, %v", got, ok)
	}
	rs, err = Parse("-9223372036854775808,9223372036854775807")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := Canonical(rs); !ok || got != "-9223372036854775808,9223372036854775807" {
		t.Errorf("Canonical = %q, %v", got, ok)
	}
}

// TestCanonical_OverlappingElementsAreReported pins the one shape this
// arithmetic cannot settle without listing the values: elements whose spans
// overlap. The caller decides what to do (expand within its own bound).
func TestCanonical_OverlappingElementsAreReported(t *testing.T) {
	for _, in := range []string{"1-5,3-7", "1-10:2,2-10:2", "1,1"} {
		rs, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := Canonical(rs); ok {
			t.Errorf("Canonical(%q) ok = true, want false", in)
		}
	}
}

// TestCanonical_MatchesTheDefinitionOnRandomInputs compares the arithmetic walk
// with the specification's definition applied directly to the listed values,
// over random expressions of stepped, reversed and single-value elements.
func TestCanonical_MatchesTheDefinitionOnRandomInputs(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20_000 {
		var parts []string
		for range 1 + rng.IntN(5) {
			a, n, s := rng.IntN(60)-20, rng.IntN(6), 1+rng.IntN(4)
			switch rng.IntN(3) {
			case 0:
				parts = append(parts, strconv.Itoa(a))
			case 1:
				parts = append(parts, fmt.Sprintf("%d-%d:%d", a, a+n*s+rng.IntN(s), s))
			default:
				parts = append(parts, fmt.Sprintf("%d-%d:-%d", a+n*s, a, s))
			}
		}
		expr := strings.Join(parts, ",")
		rs, err := Parse(expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", expr, err)
		}
		got, ok := Canonical(rs)
		if !ok {
			continue // overlapping spans: the caller's case, pinned above
		}
		if want := canonicalByDefinition(rs); got != want {
			t.Fatalf("Canonical(%q) = %q, want %q", expr, got, want)
		}
	}
}

// canonicalByDefinition lists the values and applies §3.4.1.1.1's run rule to
// them literally.
func canonicalByDefinition(rs []Range) string {
	var vals []int
	for _, r := range rs {
		vals = append(vals, r.Iterate()...)
	}
	slices.Sort(vals)
	vals = slices.Compact(vals)
	var out []string
	for i := 0; i < len(vals); {
		if i == len(vals)-1 {
			out = append(out, strconv.Itoa(vals[i]))
			break
		}
		s, j := vals[i+1]-vals[i], i+1
		for j+1 < len(vals) && vals[j+1]-vals[j] == s {
			j++
		}
		if j < i+2 {
			out = append(out, strconv.Itoa(vals[i]))
			i++
			continue
		}
		run := fmt.Sprintf("%d-%d", vals[i], vals[j])
		if s != 1 {
			run += ":" + strconv.Itoa(s)
		}
		out = append(out, run)
		i = j + 1
	}
	return strings.Join(out, ",")
}
