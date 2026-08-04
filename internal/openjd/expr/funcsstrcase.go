// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// strCaseFuncs is the first of sub-project C2's four groups: RFC 0006 section
// 2.2.4's case transforms and, once Task 2 lands, its classification
// predicates. The two share this file because they share isAlnumRune and the
// casers below.
var strCaseFuncs = map[string][]Shape{
	"upper": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(upperString(args[0].AsStr()))
		}},
	},
	"lower": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(lowerString(args[0].AsStr()))
		}},
	},
	"capitalize": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(capitalizeString(args[0].AsStr()))
		}},
	},
	"title": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(titleString(args[0].AsStr()))
		}},
	},
}

// upperString and lowerString apply FULL Unicode case mapping, not Go's
// stdlib simple mapping.
//
// This is a deliberate dependency and not a stylistic preference. RFC 0006
// defines upper() as "Convert to uppercase" and says nothing further, so the
// mapping model is undefined by the specification. strings.ToUpper applies
// SIMPLE case mapping — one rune in, one rune out — and answers "STRAßE" for
// "straße" and leaves the "ﬁ" ligature untouched. The specification's own
// reference implementation and the Python language RFC 0006 defines itself
// against both apply FULL mapping and answer "STRASSE" and "FI".
//
// Matching the stdlib would therefore produce oracle divergences with no
// reason available to record for them, and test/oracle/baseline.txt treats a
// missing reason as a hard error — correctly. Where the specification is
// silent, the reference is the only available authority.
//
// A cases.Caser is documented as stateful and unsafe for concurrent use, so
// one is built per call rather than cached in a package-level variable. These
// functions run once per expression evaluation, not in a hot loop.
func upperString(s string) string { return cases.Upper(language.Und).String(s) }

func lowerString(s string) string { return cases.Lower(language.Und).String(s) }

// capitalizeString implements RFC 0006's "Capitalize first character,
// lowercase rest" literally: uppercase the first rune, lowercase everything
// after it.
//
// The literal reading has a surprising consequence, and it is the reference
// implementation's behavior rather than an accident here:
// capitalize("ﬁne day") is "FIne day", because full case mapping expands the
// ﬁ ligature to two uppercase letters. Python answers "Fine day" because it
// TITLECASES the first character instead of uppercasing it. RFC 0006 says
// "Capitalize", not "titlecase", so we uppercase. doc.go records this.
func capitalizeString(s string) string {
	if s == "" {
		return ""
	}
	_, size := utf8.DecodeRuneInString(s)
	return upperString(s[:size]) + lowerString(s[size:])
}

// titleString uppercases the first rune of each maximal run of alphanumerics
// and lowercases the remainder of each run.
//
// The word-boundary rule was derived by running the reference implementation,
// not assumed: "they're ok" -> "They'Re Ok" (an apostrophe breaks a word),
// "a1b c" -> "A1b C" and "1st place" -> "1st Place" (a digit does NOT),
// "a_b c" -> "A_B C" (an underscore DOES), "3d-max shot" -> "3d-Max Shot",
// "HELLO WORLD" -> "Hello World" (the rest of a word is lowercased).
//
// Go's strings.Title reproduces most of that but treats "_" as part of a word,
// so it answers "A_b C" and is unusable here even setting aside its
// deprecation.
//
// The alphanumeric predicate is isAlnumRune — the SAME one the isalnum()
// function uses — on purpose. The reference uses a Unicode notion here while
// its own isdigit() is ASCII-only, which is the self-contradiction the design
// rules against; adopting it for word boundaries alone would put two
// conflicting definitions of "alphanumeric" in this one file. The cost is a
// single baselined divergence, title("²x y"), recorded in
// test/oracle/baseline.txt.
func titleString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWord := false
	for _, r := range s {
		switch {
		case !isAlnumRune(r):
			inWord = false
			b.WriteRune(r)
		case !inWord:
			inWord = true
			b.WriteString(upperString(string(r)))
		default:
			b.WriteString(lowerString(string(r)))
		}
	}
	return b.String()
}

// isAlnumRune is the alphanumeric predicate shared by isalnum() and by
// titleString's word boundary. A letter by Unicode's definition, or an ASCII
// digit.
//
// The ASCII restriction on digits is isdigit()'s ruling, argued in full at
// isdigit's registration in this file. The point of sharing one predicate is
// that "alphanumeric" means exactly one thing in this package.
func isAlnumRune(r rune) bool {
	return unicode.IsLetter(r) || (r >= '0' && r <= '9')
}

// boundedString wraps a produced string in the package's size bound.
//
// Case mapping can GROW a string — "ß" is one byte longer as "SS" — so a
// result built from an argument already at the limit can exceed it. The check
// is cheap and keeps every C2 function honest about bounding its own single
// operation.
func boundedString(s string) (Value, error) {
	if err := checkStringBytes(len(s)); err != nil {
		return Value{}, err
	}
	return String(s), nil
}
