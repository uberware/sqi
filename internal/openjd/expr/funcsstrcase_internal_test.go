// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestCaseTransforms covers the four case functions. The non-ASCII rows are the
// entire reason this package depends on golang.org/x/text/cases: Go's stdlib
// strings.ToUpper is SIMPLE case mapping and answers "STRAßE", while the
// specification's reference implementation and Python both apply FULL case
// mapping and answer "STRASSE". Every expectation below was produced by running
// openjd-model 0.11.1 during design.
func TestCaseTransforms(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"upper ascii", `upper('hello')`, "HELLO"},
		{"lower ascii", `lower('HeLLo')`, "hello"},
		{"upper expands the sharp s", `upper('straße')`, "STRASSE"},
		{"upper expands a ligature", `upper('ﬁ')`, "FI"},
		{"lower keeps the combining dot", `lower('İ')`, "i̇"},
		{"upper maps a digraph", `upper('ǳ')`, "Ǳ"},
		{"capitalize lowers the rest", `capitalize('hELLO')`, "Hello"},
		{"capitalize on empty", `capitalize('')`, ""},
		{"capitalize expands a ligature", `capitalize('ﬁne day')`, "FIne day"},
		{"capitalize an accented letter", `capitalize('éA')`, "Éa"},
		{"title two words", `title('hello world')`, "Hello World"},
		{"title breaks on an apostrophe", `title("they're ok")`, "They'Re Ok"},
		{"title keeps a digit in-word", `title('a1b c')`, "A1b C"},
		{"title starting with a digit", `title('1st place')`, "1st Place"},
		{"title breaks on an underscore", `title('a_b c')`, "A_B C"},
		{"title breaks on a hyphen", `title('3d-max shot')`, "3d-Max Shot"},
		{"title lowers the rest", `title('HELLO WORLD')`, "Hello World"},
		{"title mixed case", `title('mcDONALD')`, "Mcdonald"},
		{"title on empty", `title('')`, ""},
		{"method form", `'hello world'.title()`, "Hello World"},
		{"a path argument coerces in function position", `upper(Param.Dir)`, "/FOO/BAR"},
	}
	syms := MapSymbols{"Param.Dir": Value{Type: TPath, s: "/foo/bar"}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, syms, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != "string" {
				t.Errorf("Eval(%q) typed %s, want string", tc.src, got)
			}
		})
	}
}
