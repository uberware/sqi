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

// TestReprData_SpecificRowsBeatTheVariableRow is the third appearance of C1's
// flatten hazard.
//
// repr_py and repr_json each pair a list[varT]-style catch-all with specific
// nulltype, range_expr and path rows. matchShapesExactFirst breaks an exact
// cost tie to the EARLIEST shape, so if the catch-all were registered first the
// specific rows would never execute and null would render as its generic form
// rather than "None"/"null".
func TestReprData_SpecificRowsBeatTheVariableRow(t *testing.T) {
	syms := MapSymbols{
		"Param.Dir":    Value{Type: TPath, s: "/a/b"},
		"Param.Frames": Value{Type: TRangeExpr, s: "1-10"},
	}
	tests := []struct{ src, want string }{
		{`repr_py(null)`, "None"},
		{`repr_json(null)`, "null"},
		{`repr_py(Param.Dir)`, "'/a/b'"},
		{`repr_json(Param.Dir)`, `"/a/b"`},
		{`repr_py(Param.Frames)`, "'1-10'"},
		{`repr_json(Param.Frames)`, `"1-10"`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q — has a variable row been registered before the specific ones?", tc.src, got, tc.want)
			}
		})
	}
}
