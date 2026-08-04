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
// 2.2.4's case transforms and its classification predicates. The two share
// this file because they share isAlnumRune and the casers below.
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
	// RFC 0006 says only "True if all characters are digits and string is
	// non-empty", so the digit set is undefined by the specification and every
	// candidate answers differently: Python's isdigit is Unicode ('²' is a
	// digit), Go's unicode.IsDigit is category Nd ('٣' is, '²' is not), and the
	// reference implementation is ASCII-only.
	//
	// ASCII wins for a reason beyond matching the reference: it preserves the
	// obvious idiom. Under a Unicode definition isdigit('٣') is true while
	// int('٣') still fails, so guarding a conversion with isdigit would
	// silently stop working on exactly the inputs the guard exists for.
	"isdigit": {
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(allRunes(args[0].AsStr(), func(r rune) bool { return r >= '0' && r <= '9' })), nil
		}},
	},
	"isalpha": {
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(allRunes(args[0].AsStr(), unicode.IsLetter)), nil
		}},
	},
	// isalnum COMPOSES from isalpha and isdigit, and that is a deliberate
	// divergence from the reference implementation, which answers
	// isalnum('٣') true while answering both isdigit('٣') and isalpha('٣')
	// false. RFC 0006 defines none of the three, so the reference's own
	// self-contradiction is the strongest evidence available about which
	// reading to take: a family that composes is defensible, one that does not
	// is a bug. Will be baselined in test/oracle/baseline.txt with that reason
	// when the oracle corpus lands.
	"isalnum": {
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(allRunes(args[0].AsStr(), isAlnumRune)), nil
		}},
	},
	"isspace": {
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(allRunes(args[0].AsStr(), unicode.IsSpace)), nil
		}},
	},
	// isupper and islower are NOT allRunes over IsUpper/IsLower. RFC 0006
	// defines them over CASED characters — "all cased characters are uppercase
	// and there is at least one cased character" — so an uncased rune neither
	// satisfies nor breaks the test. isupper("ABC1") is true because a digit is
	// uncased; isupper("1") is false because nothing in it is cased at all.
	"isupper": {
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(allCased(args[0].AsStr(), unicode.IsUpper)), nil
		}},
	},
	"islower": {
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(allCased(args[0].AsStr(), unicode.IsLower)), nil
		}},
	},
	// isascii is the ONE predicate RFC 0006 declares true on the empty string:
	// "all characters are ASCII (U+0000-U+007F), OR string is empty". The other
	// six require non-empty. That asymmetry is the specification's, not a slip.
	"isascii": {
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			for _, r := range args[0].AsStr() {
				if r > unicode.MaxASCII {
					return Bool(false), nil
				}
			}
			return Bool(true), nil
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
// single divergence, title("²x y"), which will be recorded in
// test/oracle/baseline.txt when the oracle corpus lands.
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

// allRunes reports whether every rune of s satisfies pred AND s is non-empty,
// which is RFC 0006's shape for isdigit, isalpha, isalnum and isspace. isascii
// does not use it: it is the one predicate true on the empty string.
func allRunes(s string, pred func(rune) bool) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pred(r) {
			return false
		}
	}
	return true
}

// allCased reports whether every CASED rune of s satisfies pred and at least
// one cased rune exists — RFC 0006's rule for isupper and islower.
//
// "Cased" includes titlecase, which is why isupper("ǲ") is false rather than
// true: U+01F2 is a cased rune that is not uppercase, so it fails pred instead
// of being skipped. Confirmed against the reference implementation.
func allCased(s string, pred func(rune) bool) bool {
	seen := false
	for _, r := range s {
		if !unicode.IsUpper(r) && !unicode.IsLower(r) && !unicode.IsTitle(r) {
			continue
		}
		seen = true
		if !pred(r) {
			return false
		}
	}
	return seen
}
