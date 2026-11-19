// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"testing"
)

// TestCaseTransforms covers the four case functions. The non-ASCII rows are why
// this package depends on golang.org/x/text/cases: Go's stdlib strings.ToUpper
// is simple case mapping and answers "STRAßE", while the specification and
// Python both apply full case mapping and answer "STRASSE".
//
// title() and capitalize() have the semantics of Python's str.title and
// str.capitalize (RFC 0006 §2.2.4's case mapping note), so their expectations
// are CPython 3.14's answers: a word starts after any uncased character, a
// word-start character takes its full titlecase mapping, and lowercasing
// applies the Final_Sigma context rule across the whole string.
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
		{"capitalize titlecases a ligature", `capitalize('ﬁne day')`, "Fine day"},
		{"capitalize titlecases the sharp s", `capitalize('ßx')`, "Ssx"},
		{"capitalize titlecases a digraph", `capitalize('ǳa')`, "ǲa"},
		{"capitalize an accented letter", `capitalize('éA')`, "Éa"},
		{"capitalize keeps a dotted capital", `capitalize('İX')`, "İx"},
		{"capitalize titlecase differs from upper", `capitalize('ᾳ')`, "ᾼ"},
		{"capitalize applies final sigma", `capitalize('ΑΣ')`, "Ας"},
		{"capitalize a lone sigma", `capitalize('Σ')`, "Σ"},
		{"title two words", `title('hello world')`, "Hello World"},
		{"title breaks on an apostrophe", `title("they're ok")`, "They'Re Ok"},
		{"title breaks on a digit", `title('a1b c')`, "A1B C"},
		{"title starting with a digit", `title('1st place')`, "1St Place"},
		{"title breaks on an underscore", `title('a_b c')`, "A_B C"},
		{"title breaks on a hyphen", `title('3d-max shot')`, "3D-Max Shot"},
		{"title after an uncased numeric", `title('²x y')`, "²X Y"},
		{"title lowers the rest", `title('HELLO WORLD')`, "Hello World"},
		{"title mixed case", `title('mcDONALD')`, "Mcdonald"},
		{"title on empty", `title('')`, ""},
		{"title titlecases a digraph", `title('ǆab')`, "ǅab"},
		{"title treats an Other_Uppercase symbol as cased", `title('ⓐⓑ')`, "Ⓐⓑ"},
		{"title titlecase differs from upper", `title('x ᾳ')`, "X ᾼ"},
		// Final_Sigma looks across the whole string, past case-ignorable
		// characters, not at a one-rune or one-word window.
		{"title final sigma at the end", `title('ΑΒΣ')`, "Αβς"},
		{"title final sigma before a space", `title('OΣ K')`, "Oς K"},
		{"title sigma starting a word is titlecased", `title('ΣΑ')`, "Σα"},
		{"title sigma before an ignorable dot then a letter", `title('ΑΣ.Β')`, "Ασ.Β"},
		{"title sigma before an ignorable apostrophe then a letter", `title("ΑΣ'Β")`, "Ασ'Β"},
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

// TestClassificationPredicates covers RFC 0006's seven is*() functions. The six
// that are not isascii have the semantics of the Python str methods of the same
// name over the Unicode Character Database (§2.2.4's classification note), so
// the expectations below are CPython 3.14's answers.
func TestClassificationPredicates(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"isdigit ascii", `isdigit('123')`, "true"},
		{"isdigit accepts a superscript", `isdigit('²')`, "true"},
		{"isdigit accepts an arabic-indic digit", `isdigit('٣')`, "true"},
		{"isdigit accepts a circled digit", `isdigit('①')`, "true"},
		{"isdigit accepts superscripts", `isdigit('¹²³')`, "true"},
		{"isdigit rejects a fraction", `isdigit('½')`, "false"},
		{"isdigit rejects a roman numeral", `isdigit('Ⅻ')`, "false"},
		{"isdigit rejects mixed", `isdigit('12a')`, "false"},
		{"isdigit rejects empty", `isdigit('')`, "false"},
		{"isalpha letters", `isalpha('abc')`, "true"},
		{"isalpha accepts an accent", `isalpha('é')`, "true"},
		{"isalpha accepts a modifier letter", `isalpha('ʰ')`, "true"},
		{"isalpha rejects a combining mark", `isalpha('\u0345')`, "false"},
		{"isalpha rejects a fraction", `isalpha('½')`, "false"},
		{"isalpha rejects a digit", `isalpha('a1')`, "false"},
		{"isalpha rejects empty", `isalpha('')`, "false"},
		{"isalnum letters and digits", `isalnum('a1')`, "true"},
		{"isalnum accepts an arabic-indic digit", `isalnum('٣')`, "true"},
		{"isalnum accepts a fraction", `isalnum('a½')`, "true"},
		{"isalnum accepts a roman numeral", `isalnum('Ⅻ')`, "true"},
		{"isalnum rejects punctuation", `isalnum('a_b')`, "false"},
		{"isalnum rejects empty", `isalnum('')`, "false"},
		{"isspace spaces and tabs", `isspace('  \t')`, "true"},
		{"isspace accepts the information separators", `isspace('\u001c\u001d\u001e\u001f')`, "true"},
		{"isspace accepts a no-break space", `isspace('\u00a0\u0085')`, "true"},
		{"isspace rejects a zero-width space", `isspace('\u200b')`, "false"},
		{"isspace rejects a letter", `isspace(' a ')`, "false"},
		{"isspace rejects empty", `isspace('')`, "false"},
		{"isupper all upper", `isupper('ABC')`, "true"},
		{"isupper ignores an uncased digit", `isupper('ABC1')`, "true"},
		{"isupper needs a cased rune", `isupper('1')`, "false"},
		{"isupper rejects titlecase", `isupper('ǲ')`, "false"},
		{"isupper accepts an Other_Uppercase symbol", `isupper('ⒶB')`, "true"},
		{"isupper rejects an Other_Lowercase letter", `isupper('ª')`, "false"},
		{"islower all lower", `islower('abc1')`, "true"},
		{"islower accepts a digraph", `islower('ǳ')`, "true"},
		{"islower accepts an Other_Lowercase letter", `islower('ªʰ')`, "true"},
		{"islower accepts an Other_Lowercase symbol", `islower('ⓐ')`, "true"},
		{"islower rejects titlecase", `islower('aǅ')`, "false"},
		{"islower rejects empty", `islower('')`, "false"},
		{"isascii plain", `isascii('a b')`, "true"},
		{"isascii rejects non-ascii", `isascii('é')`, "false"},
		{"isascii is true on empty", `isascii('')`, "true"},
		{"method form", `'123'.isdigit()`, "true"},
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
			if got := v.Type.String(); got != "bool" {
				t.Errorf("Eval(%q) typed %s, want bool", tc.src, got)
			}
		})
	}
}
