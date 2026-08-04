// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"testing"
)

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
		// The case that discriminates per-rune casing (what titleString does)
		// from casing a whole word run in one transform: Greek sigma is
		// context-sensitive full-Unicode case mapping. Lowering "Σ" in
		// isolation (per rune, no preceding cased letter in view) answers
		// medial sigma "σ"; lowering it as part of a wider substring — e.g.
		// the two-rune "ΒΣ" — answers final sigma "ς" instead, because "Σ" is
		// then preceded by a cased letter and at the end of the input. The
		// reference implementation (openjd-model 0.11.1) answers title('ΑΒΣ')
		// = "Αβσ" — medial — so per-rune casing is not just faster than a
		// substring rewrite here, it is the only reading that matches the
		// reference. If this ever flips to "Αβς", titleString was rewritten
		// to case whole word runs and silently changed behavior.
		{"title final sigma stays medial", `title('ΑΒΣ')`, "Αβσ"},
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

// TestClassificationPredicates covers RFC 0006's seven is*() functions.
//
// Two rows encode deliberate divergences from the reference implementation and
// must not be "fixed" to match it:
//
//   - isalnum('٣') is FALSE here and true there. The reference answers
//     isdigit('٣') false and isalpha('٣') false while answering isalnum('٣')
//     true, which is not a rule, it is a contradiction. Ours composes:
//     isalnum == isalpha || isdigit, always.
//   - isdigit is ASCII-only, matching the reference and preserving the
//     guard-then-convert idiom — under a Unicode definition isdigit('٣') would
//     be true while int('٣') still fails.
func TestClassificationPredicates(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"isdigit ascii", `isdigit('123')`, "true"},
		{"isdigit rejects a superscript", `isdigit('²')`, "false"},
		{"isdigit rejects an arabic-indic digit", `isdigit('٣')`, "false"},
		{"isdigit rejects mixed", `isdigit('12a')`, "false"},
		{"isdigit rejects empty", `isdigit('')`, "false"},
		{"isalpha letters", `isalpha('abc')`, "true"},
		{"isalpha accepts an accent", `isalpha('é')`, "true"},
		{"isalpha rejects a digit", `isalpha('a1')`, "false"},
		{"isalpha rejects empty", `isalpha('')`, "false"},
		{"isalnum letters and digits", `isalnum('a1')`, "true"},
		{"isalnum diverges from the reference", `isalnum('٣')`, "false"},
		{"isalnum rejects punctuation", `isalnum('a_b')`, "false"},
		{"isalnum rejects empty", `isalnum('')`, "false"},
		{"isspace spaces and tabs", `isspace('  \t')`, "true"},
		{"isspace rejects a letter", `isspace(' a ')`, "false"},
		{"isspace rejects empty", `isspace('')`, "false"},
		{"isupper all upper", `isupper('ABC')`, "true"},
		{"isupper ignores an uncased digit", `isupper('ABC1')`, "true"},
		{"isupper needs a cased rune", `isupper('1')`, "false"},
		{"isupper rejects titlecase", `isupper('ǲ')`, "false"},
		{"islower all lower", `islower('abc1')`, "true"},
		{"islower accepts a digraph", `islower('ǳ')`, "true"},
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

// TestIsAlnum_ComposesFromIsAlphaAndIsDigit is the invariant the divergence
// above buys. It is asserted over runes rather than by spot-checking, and it
// evaluates isalpha(), isdigit() and isalnum() THROUGH the registry with Eval
// — not by reimplementing the predicates' logic locally — so a future edit to
// any one of the SHIPPED predicates cannot break the relation without failing
// here.
//
// An earlier version of this test compared isAlnumRune against a local
// unicode.IsLetter call and a local "r >= '0' && r <= '9'" check — copies of
// the predicates, not the predicates themselves — so it was only ever
// comparing isAlnumRune against itself. Verified by mutation: temporarily
// changing the registered isdigit predicate (strCaseFuncs["isdigit"]) to
// unicode.IsDigit made isdigit('٣') true while isalpha('٣') and isAlnumRune
// stayed false, genuinely breaking the isalnum == isalpha || isdigit
// invariant — and the OLD version of this test still passed, because it
// never called the registered isdigit at all. This version fails under that
// same mutation, as it must.
func TestIsAlnum_ComposesFromIsAlphaAndIsDigit(t *testing.T) {
	predicate := func(t *testing.T, fn string, r rune) bool {
		t.Helper()
		v, err := Eval(fmt.Sprintf("%s(%q)", fn, string(r)), MapSymbols{}, TAny)
		if err != nil {
			t.Fatalf("Eval(%s(%q)) failed: %v", fn, r, err)
		}
		return v.AsBool()
	}
	for _, r := range []rune{'a', 'Z', '0', '9', '²', '٣', '_', ' ', 'é', '½', '①'} {
		alpha := predicate(t, "isalpha", r)
		digit := predicate(t, "isdigit", r)
		alnum := predicate(t, "isalnum", r)
		if alnum != (alpha || digit) {
			t.Errorf("isalnum(%q) = %v, want %v (isalpha=%v isdigit=%v)", r, alnum, alpha || digit, alpha, digit)
		}
	}
}
