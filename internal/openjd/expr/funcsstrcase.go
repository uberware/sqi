// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// strCaseFuncs is the first of sub-project C2's four groups: RFC 0006 section
// 2.2.4's case transforms and its classification predicates. The two share
// this file because they share isAlnumRune and the casers below.
//
// Section 1.3.10 rule 3 (sub-project E1, Task 7): every row in this file
// declares Cost{ArgBytes: []int{0}} -- the RECEIVER's own byte length, not
// the produced result's. Confirmed by a probe designed to tell the two
// apart: 'İ' (U+0130, 2 UTF-8 bytes) lowers to 'i' plus a combining dot above
// (3 UTF-8 bytes), so 100 copies is 200 INPUT bytes (ceil/256 = 1) but 300
// OUTPUT bytes (ceil/256 = 2). The reference measures 2 for
// ('İ'*100).lower() -- matching ArgBytes, not ResultBytes -- which is the
// OPPOSITE of Task 5's finding for string "+" (ResultBytes there). See
// cost_string_internal_test.go's PROBE comment for the full transcript.
//
// The four case transforms are named directly ("upper()", "lower()" by rule
// 3's own text; capitalize()/title() are not, but do the identical
// length-proportional work, so "and similar" covers them). The seven
// classification predicates below are NOT named by rule 3's enumeration
// either, and are decided explicitly rather than left to fall out of the
// zero value: they scan the WHOLE string (RFC 0006's "all characters are X"),
// so they are rule-3 charges under "and similar" -- NOT len()-style exempt
// lookups. Confirmed scaling 10/300 bytes -> 2/3 for every one of the seven,
// with len() on the identical input pinned flat at 1 as the control (see
// TestOperationCount_ClassificationFunctionsAreNotLenExempt).
var strCaseFuncs = map[string][]Shape{
	"upper": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			s := args[0].AsStr()
			if err := checkCaseInputBytes(len(s)); err != nil {
				return Value{}, err
			}
			return boundedString(upperString(s))
		}},
	},
	"lower": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			s := args[0].AsStr()
			if err := checkCaseInputBytes(len(s)); err != nil {
				return Value{}, err
			}
			return boundedString(lowerString(s))
		}},
	},
	"capitalize": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			s := args[0].AsStr()
			if err := checkCaseInputBytes(len(s)); err != nil {
				return Value{}, err
			}
			return boundedString(capitalizeString(s))
		}},
	},
	"title": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			s := args[0].AsStr()
			if err := checkCaseInputBytes(len(s)); err != nil {
				return Value{}, err
			}
			return boundedString(titleString(s))
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
		{Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return Bool(allRunes(args[0].AsStr(), func(r rune) bool { return r >= '0' && r <= '9' })), nil
		}},
	},
	"isalpha": {
		{Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
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
		{Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return Bool(allRunes(args[0].AsStr(), isAlnumRune)), nil
		}},
	},
	"isspace": {
		{Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return Bool(allRunes(args[0].AsStr(), unicode.IsSpace)), nil
		}},
	},
	// isupper and islower are NOT allRunes over IsUpper/IsLower. RFC 0006
	// defines them over CASED characters — "all cased characters are uppercase
	// and there is at least one cased character" — so an uncased rune neither
	// satisfies nor breaks the test. isupper("ABC1") is true because a digit is
	// uncased; isupper("1") is false because nothing in it is cased at all.
	"isupper": {
		{Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return Bool(allCased(args[0].AsStr(), unicode.IsUpper)), nil
		}},
	},
	"islower": {
		{Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return Bool(allCased(args[0].AsStr(), unicode.IsLower)), nil
		}},
	},
	// isascii is the ONE predicate RFC 0006 declares true on the empty string:
	// "all characters are ASCII (U+0000-U+007F), OR string is empty". The other
	// six require non-empty. That asymmetry is the specification's, not a slip.
	"isascii": {
		{Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
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
// one is built per call rather than cached in a package-level variable. That
// is affordable here because upperString and lowerString are each called at
// most a small constant number of times per expression evaluation — once by
// upper()/lower(), twice by capitalizeString() — never in a loop over the
// input's runes.
//
// titleString does NOT route through these: transforming one word at a time
// still needs a fresh Caser per WORD RUN rather than per expression (see its
// own doc comment for why), so it builds and reuses its own pair of Casers
// instead of paying cases.Upper/cases.Lower's construction cost on every
// rune. Measured on len(title('ab ' * 3333333)): calling these two helpers
// per rune cost ~997ms and 1.6GB of allocator traffic; hoisting the Casers
// out of the loop brought that to ~392ms.
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
//
// Each rune is cased INDIVIDUALLY — a fresh one-rune context every call —
// rather than casing a whole word run in one transform, and that choice is
// load-bearing, not cosmetic: full Unicode case mapping is
// CONTEXT-SENSITIVE, and Greek sigma is the rune that proves it. Lowering the
// isolated rune "Σ" answers medial sigma "σ" (there is no preceding cased
// letter in a one-rune context, so the Final_Sigma rule cannot fire), while
// lowering the two-rune substring "ΒΣ" in one transform answers final sigma
// "ς" (now "Σ" IS preceded by a cased letter and at the end of the input, so
// Final_Sigma fires). Run against the reference implementation
// (openjd-model 0.11.1), title('ΑΒΣ') is "Αβσ" — MEDIAL sigma — confirming
// the reference itself cases the run rune by rune rather than as one
// contiguous lowercase transform; lower('ΑΒΣ') on the same string, by
// contrast, is "Αβς" with final sigma, because lower() has no word-boundary
// splitting and cases the whole string in one transform. So per-rune casing
// here is not merely defensible, it is the ONLY reading that reproduces the
// reference's own title() output — a substring-based rewrite for speed would
// silently change title()'s behavior on Greek text. See
// TestCaseTransforms/title_final_sigma_stays_medial for the pinned case.
//
// Per-rune casing costs allocations, so the two Casers are still hoisted out
// of the loop (see upperString's doc comment) rather than reaching for the
// package-level upperString/lowerString helpers, which would build a new
// Caser on every rune.
func titleString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	upperCaser := cases.Upper(language.Und)
	lowerCaser := cases.Lower(language.Und)
	inWord := false
	for _, r := range s {
		switch {
		case !isAlnumRune(r):
			inWord = false
			b.WriteRune(r)
		case !inWord:
			inWord = true
			b.WriteString(upperCaser.String(string(r)))
		default:
			b.WriteString(lowerCaser.String(string(r)))
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

// caseMapWorstCaseExpansion is the largest byte-length ratio between one
// input rune and its full-case-mapped form, anywhere in Unicode. Verified,
// not assumed: a full scan of golang.org/x/text/cases's upper, lower and
// title tables over every codepoint (0 through 0x10FFFF, surrogates
// excluded) finds the maximum at U+0390 GREEK SMALL LETTER IOTA WITH
// DIALYTIKA AND TONOS, whose full uppercase mapping is three codepoints
// (U+0399 U+0308 U+0301) — 2 input bytes in UTF-8 expanding to 6 output
// bytes, a ratio of exactly 3. Nothing else in the tables exceeds it.
const caseMapWorstCaseExpansion = 3

// checkCaseInputBytes is a CONSERVATIVE pre-check that runs BEFORE a case
// transform, using caseMapWorstCaseExpansion to reject an input that cannot
// possibly fit under the bound no matter how it expands — without paying for
// the transform at all. It is not exact: most input expands by far less than
// the worst case, so an input that passes here can still be rejected
// afterward by boundedString's exact post-check on the actual result. That
// asymmetry is the point — a cheap, sound pre-check that only ever rejects
// inputs that were always going to fail, paired with the expensive exact
// check that catches everything else. Measured before this existed: upper()
// on a 10MB string of U+0390 allocated 172MB to produce (and then refuse) a
// 30MB result; this pre-check rejects the same input for ~0 bytes allocated.
func checkCaseInputBytes(n int) error {
	if n < 0 || n > maxStringBytes/caseMapWorstCaseExpansion {
		return fmt.Errorf("%w: %d input bytes could exceed %d bytes after case mapping",
			errTooLarge, n, maxStringBytes)
	}
	return nil
}

// boundedString wraps a produced string in the package's EXACT size bound —
// the check that runs after a transform, on the real result, and is not
// fooled by an input that stayed under checkCaseInputBytes's conservative
// pre-check but still expanded close to the worst case in practice.
//
// Case mapping can GROW a string: lowercasing "İ" (U+0130, LATIN CAPITAL
// LETTER I WITH DOT ABOVE) produces "i" plus a combining dot above, 2 input
// bytes to 3 output bytes, so a result built from an argument already near
// the limit can exceed it. The check is cheap and keeps every C2 function
// honest about bounding its own single operation.
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
