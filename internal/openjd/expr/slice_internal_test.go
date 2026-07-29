// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func sliceSyms(t *testing.T) MapSymbols {
	t.Helper()
	rng, err := RangeExpr("10-50:10")
	if err != nil {
		t.Fatalf("RangeExpr: %v", err)
	}
	return MapSymbols{
		"Param.Items": List(TInt, []Value{Int(10), Int(20), Int(30), Int(40), Int(50)}),
		"Param.Name":  String("hello"),
		"Param.Range": rng,
	}
}

func TestEvalSlice_Values(t *testing.T) {
	syms := sliceSyms(t)
	tests := []struct {
		src      string
		want     string
		wantType string
	}{
		{"Param.Items[1:4]", "[20, 30, 40]", "list[int]"},
		{"Param.Items[:3]", "[10, 20, 30]", "list[int]"},
		{"Param.Items[2:]", "[30, 40, 50]", "list[int]"},
		{"Param.Items[:]", "[10, 20, 30, 40, 50]", "list[int]"},
		{"Param.Items[::2]", "[10, 30, 50]", "list[int]"},
		{"Param.Items[::-1]", "[50, 40, 30, 20, 10]", "list[int]"},
		{"Param.Items[0:5:2]", "[10, 30, 50]", "list[int]"},
		{"Param.Items[-3:]", "[30, 40, 50]", "list[int]"},
		{"Param.Items[:-2]", "[10, 20, 30]", "list[int]"},
		{"Param.Items[1:99]", "[20, 30, 40, 50]", "list[int]"},
		{"Param.Items[99:]", "[]", "list[int]"},
		{"Param.Items[3:1]", "[]", "list[int]"},
		{"Param.Name[1:3]", "el", "string"},
		{"Param.Name[:3]", "hel", "string"},
		{"Param.Name[2:]", "llo", "string"},
		{"Param.Name[::-1]", "olleh", "string"},
		{"[1, 2, 3][1:]", "[2, 3]", "list[int]"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestEvalSlice_RangeExpr pins section 2.1.8's result-type rule: a positive step
// yields a range_expr, a negative one a list[int], "because range_expr cannot
// represent descending sequences".
func TestEvalSlice_RangeExpr(t *testing.T) {
	syms := sliceSyms(t)
	tests := []struct {
		src      string
		want     string
		wantType string
	}{
		// Two picked values never collapse to a run: canonicalRange's own table
		// (rangeexpr_internal_test.go, "pair is two singles") renders exactly
		// two integers as a comma list, not a "start-end:step" run, because a
		// pair alone cannot distinguish a real step from coincidence.
		{"Param.Range[1:3]", "20,30", "range_expr"},
		{"Param.Range[:]", "10-50:10", "range_expr"},
		{"Param.Range[::2]", "10-50:20", "range_expr"},
		{"Param.Range[::-1]", "[50, 40, 30, 20, 10]", "list[int]"},
		{"Param.Range[3:1]", "[]", "list[int]"}, // empty: a range_expr cannot be empty
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

func TestEvalSlice_Errors(t *testing.T) {
	syms := sliceSyms(t)
	syms["Param.Dir"] = Value{Type: TPath, s: "/tmp"}
	tests := []struct {
		src      string
		wantSubs string
	}{
		{"Param.Items[::0]", "step"},
		{"Param.Dir[1:2]", "parts"},
		{"5[1:2]", "cannot be sliced"},
		{"Param.Items['a':]", "must be an int"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := Eval(tc.src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) = nil error, want one mentioning %q", tc.src, tc.wantSubs)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Fatalf("Eval(%q) error = %q, want it to mention %q", tc.src, err.Error(), tc.wantSubs)
			}
		})
	}
}

func TestEvalSlice_Unresolved(t *testing.T) {
	syms := MapSymbols{
		"Param.Items": Unresolved(ListOf(TInt)),
		"Param.Name":  Unresolved(TString),
		"Param.Range": Unresolved(TRangeExpr),
	}
	tests := []struct {
		src      string
		wantType string
	}{
		{"Param.Items[1:3]", "unresolved[list[int]]"},
		{"Param.Name[1:3]", "unresolved[string]"},
		{"Param.Range[1:3]", "unresolved[list[int] | range_expr]"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Fatalf("Eval(%q) type = %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}
