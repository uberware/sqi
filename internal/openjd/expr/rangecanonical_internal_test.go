// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestRangeExpr_StringFormIsCanonical pins RFC 0005 §1.2.2/§1.2.3 and RFC 0006
// as restated by openjd-specifications#202: a range_expr's value is its list of
// values, and its string form is the canonical form of Template Schemas
// §3.4.1.1.1 — however the expression was written. Every path that turns one
// into text agrees: interpolation, string(), "+", and the repr_* functions.
func TestRangeExpr_StringFormIsCanonical(t *testing.T) {
	tests := []struct{ src, want string }{
		{`range_expr("1-10:2")`, "1-9:2"},
		{`string(range_expr("1-10:2"))`, "1-9:2"},
		{`string(range_expr("1,3,5,7"))`, "1-7:2"},
		{`string(range_expr("1,5-7"))`, "1,5-7"},
		{`string(range_expr("10-15:2,1-5"))`, "1-5,10-14:2"},
		{`string(range_expr("5-1:-1"))`, "1-5"},
		{`range_expr("1-10:2") + "!"`, "1-9:2!"},
		{`"!" + range_expr("1-10:2")`, "!1-9:2"},
		{`repr_py(range_expr("1-10:4"))`, "'1-9:4'"},
		{`repr_json(range_expr("13-18,19"))`, `"13-19"`},
		{`string(range_expr("1-5,3-7"))`, "1-7"},
		{`string(range_expr([5, 1, 3]))`, "1-5:2"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestRangeExpr_EqualityIsByValue pins §1.2.5 as restated: two range
// expressions are equal when they have the same list of values, however they
// were written — including ones far too large to list.
func TestRangeExpr_EqualityIsByValue(t *testing.T) {
	for src, want := range map[string]string{
		`range_expr("1,2,3") == range_expr("1-3")`: "true",
		`range_expr("1-3") == range_expr("1-4")`:   "false",
	} {
		t.Run(src, func(t *testing.T) {
			v, err := Eval(src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", src, err)
			}
			if got := v.String(); got != want {
				t.Errorf("Eval(%q) = %s, want %s", src, got, want)
			}
		})
	}
}

// TestRangeExpr_ChunkValueKeepsItsOwnForm pins the one exception: the string
// form of a CHUNK[INT] value is the chunk's own form, "<N>-<M>" for a
// CONTIGUOUS chunk even with one or two values, so a value bound from its text
// keeps that text. A range_expr computed from it is canonical.
func TestRangeExpr_ChunkValueKeepsItsOwnForm(t *testing.T) {
	syms := MapSymbols{
		"Task.Param.F": ValueFromText(TRangeExpr, "7-8", PathPOSIX),
		"Task.Param.G": ValueFromText(TRangeExpr, "5-5", PathPOSIX),
	}
	for src, want := range map[string]string{
		`Task.Param.F`:                      "7-8",
		`Task.Param.G`:                      "5-5",
		`string(Task.Param.F)`:              "7-8",
		`range_expr(string(Task.Param.F))`:  "7,8",
		`Task.Param.F[0:]`:                  "7,8",
		`Task.Param.F == range_expr("7,8")`: "true",
	} {
		t.Run(src, func(t *testing.T) {
			v, err := Eval(src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", src, err)
			}
			if got := v.String(); got != want {
				t.Errorf("Eval(%q) = %q, want %q", src, got, want)
			}
		})
	}
}
