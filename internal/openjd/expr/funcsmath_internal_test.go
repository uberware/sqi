// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"testing"
)

func TestRound(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"ties round to even, down", "round(0.5)", "0", "int"},
		{"ties round to even, up", "round(1.5)", "2", "int"},
		{"ties round to even, down again", "round(2.5)", "2", "int"},
		{"below the tie", "round(2.4)", "2", "int"},
		{"above the tie", "round(2.6)", "3", "int"},
		{"negative ties round to even", "round(-1.5)", "-2", "int"},
		{"positive ndigits gives a float", "round(3.14159, 2)", "3.14", "float"},
		{"positive ndigits preserves trailing zeros", "round(3.5, 2)", "3.50", "float"},
		{"positive ndigits with one place", "round(3.55, 1)", "3.6", "float"},
		{"zero ndigits gives an int", "round(3.7, 0)", "4", "int"},
		{"negative ndigits gives an int", "round(1234.5, -1)", "1230", "int"},
		{"negative ndigits, two places", "round(1234.5, -2)", "1200", "int"},
		{"an int with ndigits stays an int", "round(1234, -2)", "1200", "int"},
		{"an int with positive ndigits is unchanged", "round(1234, 2)", "1234", "int"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestRound_CarryIsRoundOnlyAndDoesNotPropagate pins all three invariants of
// the rendered-form field at once. It is the reason the field is safe to add to
// a type every other file in the package builds.
func TestRound_CarryIsRoundOnlyAndDoesNotPropagate(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"string() sees the carried form", "string(round(3.5, 2))", "3.50"},
		{"arithmetic discards it", "string(round(3.5, 2) + 0.0)", "3.5"},
		{"multiplication discards it", "string(round(3.5, 2) * 1.0)", "3.5"},
		{"a plain float has no carry", "string(3.5)", "3.5"},
		{"round without ndigits has no carry", "string(round(3.5))", "4"},
		{"a list element keeps its own carry", "string([round(3.5, 2)])", "[3.50]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.AsStr(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
		})
	}
}

// TestRound_RejectsAnUnrenderableWidth guards the one unbounded path the carry
// opens: ndigits is an arbitrary int and the rendered form is proportional to
// it.
func TestRound_RejectsAnUnrenderableWidth(t *testing.T) {
	_, err := Eval("round(3.5, 100000000)", MapSymbols{}, TAny)
	if err == nil {
		t.Fatal("round with an enormous ndigits succeeded; want a size error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want it to report the size bound", err)
	}
}

func TestAbsFloorCeil(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"abs of a negative int", "abs(-5)", "5", "int"},
		{"abs of a positive int", "abs(5)", "5", "int"},
		{"abs of zero", "abs(0)", "0", "int"},
		{"abs of a negative float", "abs(-2.5)", "2.5", "float"},
		{"abs of a positive float", "abs(2.5)", "2.5", "float"},
		{"floor of an int is the identity", "floor(3)", "3", "int"},
		{"floor of a negative int is the identity", "floor(-3)", "-3", "int"},
		{"floor of a float goes down", "floor(3.7)", "3", "int"},
		{"floor of a negative float goes down", "floor(-3.2)", "-4", "int"},
		{"floor of a whole float", "floor(3.0)", "3", "int"},
		{"ceil of an int is the identity", "ceil(3)", "3", "int"},
		{"ceil of a float goes up", "ceil(3.2)", "4", "int"},
		{"ceil of a negative float goes up", "ceil(-3.7)", "-3", "int"},
		{"ceil of a whole float", "ceil(3.0)", "3", "int"},
		{"method form on a parenthesized literal", "(-5).abs()", "5", "int"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestAbs_RejectsTheUnrepresentableMinimum pins that abs reports the same
// overflow section 2.1.1's arithmetic does, rather than returning its negative
// input the way Go's unary minus silently would.
func TestAbs_RejectsTheUnrepresentableMinimum(t *testing.T) {
	_, err := Eval("abs(-9223372036854775807 - 1)", MapSymbols{}, TAny)
	if err == nil {
		t.Fatal("abs of the minimum int64 succeeded; want an overflow error")
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Errorf("error = %v, want it to report an overflow", err)
	}
}

func TestMinMaxSum(t *testing.T) {
	syms := MapSymbols{"Param.R": mustRangeExpr(t, "1-4")}
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"min of two ints", "min(3, 7)", "3", "int"},
		{"min of two floats", "min(3.5, 7.5)", "3.5", "float"},
		{"min promotes a mixed pair", "min(1, 2.0)", "1.0", "float"},
		{"min of three ints", "min(5, 3, 7)", "3", "int"},
		{"min of three floats", "min(5.0, 3.0, 7.0)", "3.0", "float"},
		{"min of an int list", "min([3, 1, 2])", "1", "int"},
		{"min of a float list", "min([3.0, 1.0])", "1.0", "float"},
		{"min of a range_expr", "min(Param.R)", "1", "int"},
		{"max of two ints", "max(3, 7)", "7", "int"},
		{"max of three ints", "max(5, 3, 7)", "7", "int"},
		{"max of an int list", "max([3, 1, 2])", "3", "int"},
		{"max of a float list", "max([3.0, 1.0])", "3.0", "float"},
		{"max of a range_expr", "max(Param.R)", "4", "int"},
		{"sum of an empty list is zero", "sum([])", "0", "int"},
		{"sum of an int list", "sum([1, 2, 3])", "6", "int"},
		{"sum of a float list", "sum([1.5, 2.5])", "4.0", "float"},
		{"sum of a range_expr", "sum(Param.R)", "10", "int"},
		{"method form", "[3, 1, 2].min()", "1", "int"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %s, want %s", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestMinMax_EmptyList covers BOTH routes to RFC 0006's wording. The literal
// matches the dedicated list[nulltype] row; the symbol is typed list[int] and
// matches the int row, whose Fn must raise the same message. Only the first is
// reachable from a literal, which is what makes the second easy to miss.
func TestMinMax_EmptyList(t *testing.T) {
	syms := MapSymbols{
		"Param.EmptyInts":   List(TInt, nil),
		"Param.EmptyFloats": List(TFloat, nil),
	}
	tests := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{"min of an empty literal", "min([])", "min() requires a non-empty list"},
		{"max of an empty literal", "max([])", "max() requires a non-empty list"},
		{"min of an empty typed list", "min(Param.EmptyInts)", "min() requires a non-empty list"},
		{"max of an empty typed list", "max(Param.EmptyInts)", "max() requires a non-empty list"},
		{"min of an empty float list", "min(Param.EmptyFloats)", "min() requires a non-empty list"},
		{"max of an empty float list", "max(Param.EmptyFloats)", "max() requires a non-empty list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, syms, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("Eval(%q) error = %v, want it to contain %q", tc.src, err, tc.wantSubs)
			}
		})
	}
}

// TestSum_ReportsOverflow pins that summing routes through section 2.1.1's
// checked addition rather than wrapping.
func TestSum_ReportsOverflow(t *testing.T) {
	_, err := Eval("sum([9223372036854775807, 1])", MapSymbols{}, TAny)
	if err == nil {
		t.Fatal("sum overflowed silently; want an error")
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Errorf("error = %v, want it to report an overflow", err)
	}
}
