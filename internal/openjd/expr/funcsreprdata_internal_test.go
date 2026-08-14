// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestReprPy pins Python's repr, which RFC 0006 names as repr_py's behavior.
//
// The quote-selection row is the one that matters: Python prefers a single
// quote but switches to a double quote when the string contains a single quote
// and no double quote. The reference escapes instead, giving 'it\'s', so this
// is a deliberate divergence with the spec on sqi's side.
func TestReprPy(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"plain string", `repr_py('abc')`, "'abc'"},
		{"switches quote for an apostrophe", `repr_py("it's")`, `"it's"`},
		{"escapes when both quotes present", `repr_py('a\'b"c')`, `'a\'b"c'`},
		{"newline escapes", `repr_py('a\nb')`, `'a\nb'`},
		{"tab escapes", `repr_py('a\tb')`, `'a\tb'`},
		{"backslash escapes", `repr_py('a\\b')`, `'a\\b'`},
		{"non-ascii stays literal", `repr_py('café')`, "'café'"},
		{"null", `repr_py(null)`, "None"},
		{"true", `repr_py(true)`, "True"},
		{"false", `repr_py(false)`, "False"},
		{"int", `repr_py(42)`, "42"},
		{"float", `repr_py(1.5)`, "1.5"},
		{"integral float keeps its point", `repr_py(1.0)`, "1.0"},
		{"list", `repr_py(['a', 'b'])`, "['a', 'b']"},
		{"int list", `repr_py([1, 2])`, "[1, 2]"},
		{"empty list", `repr_py([])`, "[]"},
		{"c1 control uses the two-digit \\x tier", `repr_py('\x85')`, `'\x85'`},
		{"astral non-printable uses the eight-digit capital \\U tier", `repr_py('\U000f0000')`, `'\U000f0000'`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

// TestReprJSON pins Python's json.dumps defaults, which is what the reference
// produces.
//
// Two rows encode why this does NOT share writeJSONValue (funcsconv.go, used by
// string(list)): Go's encoding/json escapes "<", ">" and "&" as < and
// friends, and does NOT escape non-ASCII. json.dumps does exactly the reverse,
// and so does the reference. Measured during design.
func TestReprJSON(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"plain string", `repr_json('abc')`, `"abc"`},
		{"non-ascii escapes", `repr_json('café')`, `"caf\u00e9"`},
		{"angle brackets stay literal", `repr_json('a<b>c&d')`, `"a<b>c&d"`},
		{"newline escapes", `repr_json('a\nb')`, `"a\nb"`},
		{"tab escapes", `repr_json('a\tb')`, `"a\tb"`},
		{"quote escapes", `repr_json('say "hi"')`, `"say \"hi\""`},
		{"null", `repr_json(null)`, "null"},
		{"true", `repr_json(true)`, "true"},
		{"int", `repr_json(42)`, "42"},
		{"float", `repr_json(1.5)`, "1.5"},
		{"list", `repr_json(['a', 'b'])`, `["a", "b"]`},
		{"empty list", `repr_json([])`, "[]"},
		{"backspace uses the named \\b escape", `repr_json('\x08')`, `"\b"`},
		{"form feed uses the named \\f escape", `repr_json('\x0c')`, `"\f"`},
		{"del has no named escape", `repr_json('\x7f')`, `"\u007f"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

// TestReprData_RendersSpecificTypes pins the OUTPUT repr_py and repr_json
// produce for null, path and range_expr — not which Shape row produces it.
//
// repr_py and repr_json each pair a type-variable catch-all with specific
// nulltype, range_expr and path rows, which looks like C1's flatten hazard
// (matchShapesExactFirst breaks an exact cost tie to the EARLIEST shape). It
// is not observable here: pyRepr and jsonRepr each switch on v.Type.Code
// directly, so the catch-all's Fn renders CodeNull/CodePath/CodeRangeExpr
// exactly the same way the dedicated rows do. Reordering the rows so the
// catch-all runs first was tried during development and this test kept
// passing — confirmed by re-running it with the rows swapped, not asserted.
//
// Consequently: if a later change ever gives pyRepr/jsonRepr's catch-all
// branch behavior that diverges from the dedicated null/path/range_expr
// rows (for example, by narrowing that switch), it must add a test that can
// tell the rows apart, because this one structurally cannot.
func TestReprData_RendersSpecificTypes(t *testing.T) {
	syms := MapSymbols{
		"Param.Dir":    Value{Type: TPath, s: "/a/b"},
		"Param.Frames": Value{Type: TRangeExpr, s: "1-10"},
		"Param.Dirs": List(TPath, []Value{
			{Type: TPath, s: "/a"},
			{Type: TPath, s: "/b"},
		}),
	}
	tests := []struct{ src, want string }{
		{`repr_py(null)`, "None"},
		{`repr_json(null)`, "null"},
		{`repr_py(Param.Dir)`, "'/a/b'"},
		{`repr_json(Param.Dir)`, `"/a/b"`},
		{`repr_py(Param.Frames)`, "'1-10'"},
		{`repr_json(Param.Frames)`, `"1-10"`},
		// Previously untested registered row: repr_json's list[path] shape
		// (it has no dedicated list[path] row of its own — this exercises
		// the varT catch-all rendering a list of paths via jsonRepr's
		// CodeList case, each element through the CodePath case).
		{`repr_json(Param.Dirs)`, `["/a", "/b"]`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}
