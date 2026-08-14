// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"math"
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

// TestMinMax_EmptyList_SelectsNoReturnRow pins the REAL behavior behind
// TestMinMax_EmptyList's error text, which a defect could satisfy by accident:
// a prior revision of shape.go's argCostList scored list[nulltype] against
// EVERY concrete-element list row (list[int], list[float], and the dedicated
// list[nulltype] row itself) as the same cost-1 widen, so the three rows tied
// and matchShapesExactFirst's earliest-wins tiebreak picked list[int] — never
// the dedicated noreturn row. That was invisible from the error text alone
// because extremumInt independently raises the identical RFC 0006 wording for
// an empty []Value. Asserting on the matched Shape's Ret — TNoReturn only the
// dedicated row declares — is the only way to tell the two rows apart.
func TestMinMax_EmptyList_SelectsNoReturnRow(t *testing.T) {
	for _, name := range []string{"min", "max"} {
		t.Run(name, func(t *testing.T) {
			shape, _, ok := matchShapes(mathFuncs[name], []Type{ListOf(TNull)})
			if !ok {
				t.Fatalf("%s([]) matched no shape", name)
			}
			if !shape.Ret.Equal(TNoReturn) {
				t.Errorf("%s([]) matched a shape returning %s, want %s (the dedicated list[nulltype] row)",
					name, shape.Ret, TNoReturn)
			}
		})
	}
}

// TestSum_EmptyListIsOrderIndependent pins that sum([]) returns int because
// its list[nulltype] row wins ON COST against the list[int]/list[float] rows
// (shape.go's argCostList scores list[nulltype] vs list[nulltype] as an exact
// match now, cost 0, against those rows' cost-1 widen) — not because it
// happens to be registered first. Reordering the rows must not change the
// result; if it did, the selection would be a position-dependent tie rather
// than a real cost win, which is exactly the defect this guards against.
func TestSum_EmptyListIsOrderIndependent(t *testing.T) {
	original := mathFuncs["sum"]
	reordered := make([]Shape, len(original))
	copy(reordered, original)
	// Move the list[nulltype] row (index 0) to the end, so a position-based
	// tiebreak would hand the match to list[int] (now first) instead.
	reordered = append(reordered[1:], reordered[0])

	shape, _, ok := matchShapes(reordered, []Type{ListOf(TNull)})
	if !ok {
		t.Fatal("sum([]) matched no shape after reordering")
	}
	if !shape.Ret.Equal(TInt) {
		t.Fatalf("sum([]) matched a shape returning %s after reordering, want %s", shape.Ret, TInt)
	}
	v, err := shape.Fn(nil)
	if err != nil {
		t.Fatalf("sum([]) shape.Fn failed: %v", err)
	}
	if got := v.String(); got != "0" {
		t.Errorf("sum([]) = %s, want 0", got)
	}
}

// TestFloorCeilRound_RejectAnUnrepresentableFloat pins that floor, ceil and a
// bare round(float) report an integer overflow for a magnitude outside
// int64's range, rather than the architecture-dependent wrong answer Go's raw
// "int64(f)" conversion gives for such a value: measured on the same source,
// math.MaxInt64 on arm64 and math.MinInt64 on amd64 for the exact same input.
// floatToInt (funcsmath.go) is the shared guard all three (and roundToDigits's
// non-positive-ndigits branch, covered separately below) now go through.
func TestFloorCeilRound_RejectAnUnrepresentableFloat(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"floor of a magnitude past int64's max", "floor(1e30)"},
		{"ceil of a magnitude past int64's max", "ceil(1e30)"},
		{"bare round of a magnitude past int64's max", "round(1e30)"},
		{"floor of the largest representable int64 magnitude as a float", "floor(9223372036854775807.0)"},
		{"floor exactly at the upper bound, 2^63", "floor(9223372036854775808.0)"},
		{"ceil of a magnitude past int64's min", "ceil(-1e30)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want an overflow error", tc.src)
			}
			if !strings.Contains(err.Error(), "overflow") {
				t.Errorf("Eval(%q) error = %v, want it to report an overflow", tc.src, err)
			}
		})
	}
}

// TestFloorCeilRound_AcceptTheExactBoundary pins the other side of the same
// guard: both bounds floatToInt checks (-2^63 and just under +2^63) are
// exactly representable as float64, so a value sitting exactly on the
// in-range side must still succeed rather than being caught by an off-by-one
// in the comparison.
func TestFloorCeilRound_AcceptTheExactBoundary(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"floor at exactly -2^63, the lowest in-range value", "floor(-9223372036854775808.0)", "-9223372036854775808"},
		{"ceil at exactly -2^63", "ceil(-9223372036854775808.0)", "-9223372036854775808"},
		{"round at exactly -2^63", "round(-9223372036854775808.0)", "-9223372036854775808"},
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
		})
	}
}

// TestRoundIntToDigits_RejectsMultiplyOverflow pins that the final q*scale
// multiplication in roundIntToDigits (funcsmath.go) is checked. Both cases are
// plain int64 literals — no float involved — and both used to return the
// same wrong, sign-flipped -9223372036854775806 on every architecture: the
// half-adjustment and the scale accumulation were each individually guarded,
// but the multiplication that combines them was not.
func TestRoundIntToDigits_RejectsMultiplyOverflow(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"rounding the maximum int64 down a digit overflows", "round(9223372036854775807, -1)"},
		{"one below the maximum overflows the same way", "round(9223372036854775806, -1)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want an overflow error", tc.src)
			}
			if !strings.Contains(err.Error(), "overflow") {
				t.Errorf("Eval(%q) error = %v, want it to report an overflow", tc.src, err)
			}
		})
	}
}

// TestRoundToDigits_NegativeNdigitsBeyondFloatRange pins roundToDigits's
// negative-ndigits branch against math.Pow(10, 400) silently overflowing to
// +Inf: f/Inf gives 0, and 0*Inf gives NaN, whose narrowing to int64 used to
// be 0 on arm64 but math.MinInt64 on amd64 — sqi's primary deployment arch —
// for the exact same source. The fix bounds the scale accumulation the same
// way roundIntToDigits already bounds its own, so this now computes 0
// directly rather than discovering it through a NaN.
func TestRoundToDigits_NegativeNdigitsBeyondFloatRange(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"a positive value rounds to 0 at an astronomically coarse place", "round(3.5, -400)"},
		{"a negative value rounds to 0 the same way", "round(-3.5, -400)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != "0" {
				t.Errorf("Eval(%q) = %s, want 0", tc.src, got)
			}
			if got := v.Type.String(); got != "int" {
				t.Errorf("Eval(%q) typed %s, want int", tc.src, got)
			}
		})
	}
}

// TestRoundToDigits_PositiveNdigitsBeyondFloatRange is the positive-ndigits
// counterpart to TestRoundToDigits_NegativeNdigitsBeyondFloatRange above, and
// it exists because only the negative branch was ever guarded.
//
// The positive branch scales by math.Pow(10, ndigits), which is +Inf from
// ndigits 309 up; f*Inf is then ±Inf (NaN when f is 0), and dividing that by
// Inf gives NaN. Below 309 the scale is finite but f*scale can still overflow
// on its own, which is why round(2.0, 308) and round(1e300, 300) fail too --
// the window depends on the magnitude of f, not on ndigits alone.
//
// None of these is an error case. Rounding a float64 to that many decimal
// places is the IDENTITY: no float64 carries enough precision for a rounding
// at 1e-308 resolution to change it, so the answer is f itself, rendered to
// ndigits places. round(2.0, 307) already returns 2.000...0 and there is no
// mathematical discontinuity at 308 -- only an artifact of the multiply.
func TestRoundToDigits_PositiveNdigitsBeyondFloatRange(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		prefix  string
		ndigits int
	}{
		{"the scale itself overflows", "round(2.0, 309)", "2.", 309},
		{"far beyond the scale overflow", "round(2.0, 600)", "2.", 600},
		{"the scale is finite but f*scale overflows", "round(2.0, 308)", "2.", 308},
		{"a negative value is unchanged the same way", "round(-2.0, 400)", "-2.", 400},
		{"zero is unchanged", "round(0.0, 600)", "0.", 600},
		{"a large f overflows at a much smaller ndigits", "round(1e300, 300)", "1", 300},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.Type.String(); got != "float" {
				t.Errorf("Eval(%q) typed %s, want float", tc.src, got)
			}
			got := v.String()
			if !strings.HasPrefix(got, tc.prefix) {
				t.Errorf("Eval(%q) = %.20s..., want it to start with %q", tc.src, got, tc.prefix)
			}
			point := strings.IndexByte(got, '.')
			if point < 0 {
				t.Fatalf("Eval(%q) = %.20s..., want a decimal point", tc.src, got)
			}
			if places := len(got) - point - 1; places != tc.ndigits {
				t.Errorf("Eval(%q) rendered %d decimal places, want %d", tc.src, places, tc.ndigits)
			}
		})
	}
}

// TestRoundToDigits_PositiveNdigitsRoundsBelowTheOverflow guards the fix above
// from being written as "return f whenever ndigits is large": inside the range
// where the scale is usable, round must still round.
func TestRoundToDigits_PositiveNdigitsRoundsBelowTheOverflow(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"rounds at two places", "round(3.14159, 2)", "3.14"},
		{"keeps a trailing zero", "round(3.5, 2)", "3.50"},
		{"rounds half to even at one place", "round(3.55, 1)", "3.6"},
		{"the largest usable scale still renders", "round(2.0, 307)", "2." + strings.Repeat("0", 307)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %.20s..., want %.20s...", tc.src, got, tc.want)
			}
		})
	}
}

// TestRoundIntToDigits_CoarserThanTheValueIsZeroNotOverflow is the int
// counterpart to the float branch's "compute the answer directly" rule, and it
// covers the one place the two disagreed.
//
// roundIntToDigits bounds its scale accumulation to avoid an int64 multiply
// overflow, but bailed out with errIntOverflow as soon as the scale itself
// grew past MaxInt64 — even though rounding a small value at a place that
// coarse has an exact, representable answer: 0. roundToDigits' own negative
// branch already returns 0 for round(3.5, -400); this made round(1234, -19)
// an error for the same shape of question.
//
// A genuine overflow is still an overflow: a value at or above half the scale
// rounds away from zero to ±scale, which is not representable.
func TestRoundIntToDigits_CoarserThanTheValueIsZeroNotOverflow(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"the scale exceeds int64 and the value is far below half of it", "round(1234, -19)", "0"},
		{"astronomically coarse", "round(1234, -400)", "0"},
		{"a negative value rounds to 0 the same way", "round(-1234, -19)", "0"},
		{"the float branch already agreed", "round(1234.0, -400)", "0"},
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
		})
	}
}

// TestRoundIntToDigits_BeyondScaleStillReportsRealOverflow is the other half of
// the test above: returning 0 for a coarse place must not swallow the case that
// genuinely does not fit. A value past half the scale rounds away from zero to
// ±scale, and no scale in this branch is representable.
//
// The 5e18 boundary is exact and is the tie: at exactly half, round-to-even
// keeps the already-even quotient 0, and one above it rounds away. The
// reference disagrees on that single value — it answers 0 for
// round(5000000000000000001, -19) because it scales through float64, where
// 5000000000000000001 IS 5e18 and the +1 is gone. roundIntToDigits exists to
// avoid exactly that ("without going through float64 and its precision"), so
// sqi is right here; see test/oracle/baseline.txt.
func TestRoundIntToDigits_BeyondScaleStillReportsRealOverflow(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"one past the tie rounds away to an unrepresentable scale", "round(5000000000000000001, -19)"},
		{"the maximum int64 rounds away the same way", "round(9223372036854775807, -19)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want an overflow error", tc.src)
			}
			if !strings.Contains(err.Error(), "overflow") {
				t.Errorf("Eval(%q) error = %v, want it to report an overflow", tc.src, err)
			}
		})
	}
}

// TestRoundIntToDigits_ExactlyAtTheTieRoundsToEven pins the inclusive bound.
func TestRoundIntToDigits_ExactlyAtTheTieRoundsToEven(t *testing.T) {
	for _, src := range []string{"round(5000000000000000000, -19)", "round(-5000000000000000000, -19)"} {
		t.Run(src, func(t *testing.T) {
			v, err := Eval(src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", src, err)
			}
			if got := v.String(); got != "0" {
				t.Errorf("Eval(%q) = %s, want 0 (the tie keeps the even quotient)", src, got)
			}
		})
	}
}

// TestRoundIntBeyondScale_MinInt64 covers the one input the source-level tests
// cannot reach: math.MinInt64 has no integer literal, because the lexer sees
// "-9223372036854775808" as unary minus applied to 9223372036854775808, which
// is itself out of int64's range. Calling the helper directly is the only way
// to exercise the negative half of the away-from-zero branch.
func TestRoundIntBeyondScale_MinInt64(t *testing.T) {
	if _, err := roundIntBeyondScale(math.MinInt64, 19); err == nil {
		t.Fatal("roundIntBeyondScale(MinInt64, 19) succeeded; want an overflow error")
	}
	v, err := roundIntBeyondScale(math.MinInt64, 20)
	if err != nil {
		t.Fatalf("roundIntBeyondScale(MinInt64, 20) failed: %v", err)
	}
	if got := v.String(); got != "0" {
		t.Errorf("roundIntBeyondScale(MinInt64, 20) = %s, want 0", got)
	}
}
