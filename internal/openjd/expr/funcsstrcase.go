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

// strCaseFuncs is the first of the four string-function groups: RFC 0006
// section 2.2.4's case transforms and its classification predicates. The two
// share this file because they share the rune predicates and casers below.
//
// Section 1.3.10 rule 3: every row in this file declares Cost{ArgBytes:
// []int{0}} -- the receiver's own byte length, not the produced result's, as
// the reference charges. 'İ' (U+0130, 2 UTF-8 bytes) lowers to 'i' plus a
// combining dot above (3 UTF-8 bytes), so 100 copies is 200 input bytes
// (ceil/256 = 1) but 300 output bytes (ceil/256 = 2), and ('İ'*100).lower()
// costs 2 -- the opposite of string "+" (ResultBytes, ops.go). See
// cost_string_internal_test.go.
//
// The four case transforms are named directly ("upper()", "lower()" by rule
// 3's own text; capitalize()/title() are not, but do the identical
// length-proportional work, so "and similar" covers them). The seven
// classification predicates below are not named by rule 3's enumeration
// either, and are decided explicitly rather than left to fall out of the
// zero value: they scan the whole string (RFC 0006's "all characters are X"),
// so they are rule-3 charges under "and similar" -- not len()-style exempt
// lookups. Confirmed scaling 10/300 bytes -> 2/3 for every one of the seven,
// with len() on the identical input pinned flat at 1 as the control (see
// TestOperationCount_ClassificationFunctionsAreNotLenExempt).
var strCaseFuncs = map[string][]Shape{
	"upper":      {caseShape(upperString)},
	"lower":      {caseShape(lowerString)},
	"capitalize": {caseShape(capitalizeString)},
	"title":      {caseShape(titleString)},
	// The six predicates below other than isascii have the semantics of the
	// Python str methods of the same name (RFC 0006 §2.2.4); doc.go lists the
	// consequences that are easy to mistake for bugs.
	"isdigit": {runePredShape(allRunes, isDigitRune)},
	"isalpha": {runePredShape(allRunes, unicode.IsLetter)},
	"isalnum": {runePredShape(allRunes, isAlnumRune)},
	"isspace": {runePredShape(allRunes, isSpaceRune)},
	// isupper and islower are not allRunes over a case predicate. RFC 0006
	// defines them over cased characters — "all cased characters are uppercase
	// and there is at least one cased character" — so an uncased rune neither
	// satisfies nor breaks the test. isupper("ABC1") is true because a digit is
	// uncased; isupper("1") is false because nothing in it is cased at all.
	"isupper": {runePredShape(allCased, isUpperRune)},
	"islower": {runePredShape(allCased, isLowerRune)},
	// isascii is the one predicate RFC 0006 declares true on the empty string:
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

// caseShape builds the single row of one string -> string case transform.
//
// The pre-check is in the constructor, which is why this exists rather than
// four hand-written rows: checkCaseInputBytes is a pre-allocation bound
// (without it, upper() on a 10 MB string of U+0390 builds a result several
// times larger before refusing it; see its own doc comment), and it is the
// line a fifth
// transform written out longhand would forget. Here a fifth transform is one
// call and cannot omit it. Cost is likewise declared once: see this file's own
// cost comment for why every row charges the receiver's bytes rather than the
// produced result's.
func caseShape(f func(string) string) Shape {
	return Shape{
		Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}},
		Fn: func(args []Value) (Value, error) {
			s := args[0].AsStr()
			if err := checkCaseInputBytes(len(s)); err != nil {
				return Value{}, err
			}
			return boundedString(f(s))
		},
	}
}

// runePredShape builds the single row of one string -> bool classification
// predicate: scan is the rune-scanning rule the specification gives that
// predicate -- allRunes for the four defined over every character, allCased for
// the two defined over cased characters only -- and pred is what each rune is
// tested against.
//
// isascii keeps a row of its own rather than coming through here. It is the one
// predicate whose scan admits the empty string, and writing that out is what
// keeps the asymmetry visible where the specification puts it.
func runePredShape(scan func(string, func(rune) bool) bool, pred func(rune) bool) Shape {
	return Shape{
		Params: []Type{TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}},
		Fn: func(args []Value) (Value, error) {
			return Bool(scan(args[0].AsStr(), pred)), nil
		},
	}
}

// upperString and lowerString apply full Unicode case mapping, not Go's
// stdlib simple mapping.
//
// This dependency is required, not a stylistic preference. RFC 0006
// defines upper() as "Convert to uppercase" and says nothing further, so the
// mapping model is undefined by the specification. strings.ToUpper applies
// simple case mapping — one rune in, one rune out — and answers "STRAßE" for
// "straße" and leaves the "ﬁ" ligature untouched. The specification's own
// reference implementation and the Python language RFC 0006 defines itself
// against both apply full mapping and answer "STRASSE" and "FI".
//
// Matching the stdlib would therefore produce oracle divergences with no
// reason available to record for them, and test/oracle/baseline.txt treats a
// missing reason as a hard error. Where the specification is
// silent, the reference is the only available authority.
//
// A cases.Caser is documented as stateful and unsafe for concurrent use, so
// one is built per call rather than cached in a package-level variable. That
// is affordable here because upperString and lowerString are each called at
// most a small constant number of times per expression evaluation — once by
// upper()/lower(), twice by capitalizeString() — never in a loop over the
// input's runes.
//
// titleString does not route through these: transforming one word at a time
// still needs a fresh Caser per word run rather than per expression (see its
// own doc comment for why), so it builds and reuses its own pair of Casers
// instead of paying cases.Upper/cases.Lower's construction cost on every
// rune, which on a multi-megabyte title() argument would dominate the call.
func upperString(s string) string { return cases.Upper(language.Und).String(s) }

func lowerString(s string) string { return cases.Lower(language.Und).String(s) }

// capitalizeString is Python's str.capitalize (3.8 and later): the first rune
// takes its full titlecase mapping and every later rune its full lowercase
// mapping. Titlecase is not uppercase: capitalize("ﬁne day") is "Fine day", not
// "FIne day", and capitalize("ßx") is "Ssx".
//
// Without a capital sigma in it, the rest is lowered in one call: sigma is the
// only rune whose lowercase mapping depends on its neighbors (writeLowerRune).
func capitalizeString(s string) string {
	if s == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(s)
	head := cases.Title(language.Und, cases.NoLower).String(string(first))
	if !strings.ContainsRune(s[size:], capitalSigma) {
		return head + lowerString(s[size:])
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(head)
	lowerCaser := cases.Lower(language.Und)
	for i := 1; i < len(rs); i++ {
		writeLowerRune(&b, lowerCaser, rs, i)
	}
	return b.String()
}

// titleString is Python's str.title. A rune preceded by a cased rune takes its
// full lowercase mapping; any other rune takes its full titlecase mapping. So a
// word starts after anything uncased (a digit, punctuation, an uncased letter):
// title("1st place") is "1St Place", title("a1b") is "A1B".
//
// Each rune is cased individually, but sigma is not cased in isolation: Python
// lowercases a capital sigma by the Unicode Final_Sigma rule over the whole
// string (writeLowerRune), so title("ΑΒΣ") is "Αβς".
//
// The Casers are built once, outside the loop, and an ASCII rune (whose
// titlecase is its uppercase) skips them.
func titleString(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	titleCaser := cases.Title(language.Und, cases.NoLower)
	lowerCaser := cases.Lower(language.Und)
	prevCased := false
	for i, r := range rs {
		switch {
		case prevCased:
			writeLowerRune(&b, lowerCaser, rs, i)
		case r < utf8.RuneSelf:
			b.WriteRune(unicode.ToUpper(r))
		default:
			b.WriteString(titleCaser.String(string(r)))
		}
		prevCased = isCasedRune(r)
	}
	return b.String()
}

// writeLowerRune appends the full lowercase mapping of rs[i], deciding a capital
// sigma's form the way CPython does: final "ς" when a cased rune precedes it and
// no cased rune follows it, skipping case-ignorable runes in both directions;
// medial "σ" otherwise. Any other rune is lowered on its own, which is exact
// because sigma is the only rune whose full lowercase mapping in the root
// locale depends on context.
func writeLowerRune(b *strings.Builder, lowerCaser cases.Caser, rs []rune, i int) {
	switch r := rs[i]; {
	case r < utf8.RuneSelf:
		b.WriteRune(unicode.ToLower(r))
		return
	case r != capitalSigma:
		b.WriteString(lowerCaser.String(string(r)))
		return
	}
	if isFinalSigma(rs, i) {
		b.WriteRune(finalSigma)
	} else {
		b.WriteRune(smallSigma)
	}
}

const (
	capitalSigma = 'Σ' // U+03A3
	smallSigma   = 'σ' // U+03C3
	finalSigma   = 'ς' // U+03C2
)

// isFinalSigma is the Unicode Final_Sigma condition for the sigma at rs[i].
//
// The scan stops at the first rune that is not case-ignorable in each
// direction, so a long run of case-ignorable runes costs time proportional to
// the run; across a whole string every rune is scanned at most once per
// adjacent sigma.
func isFinalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && isCaseIgnorableRune(rs[j]) {
		j--
	}
	if j < 0 || !isCasedRune(rs[j]) {
		return false
	}
	j = i + 1
	for j < len(rs) && isCaseIgnorableRune(rs[j]) {
		j++
	}
	return j == len(rs) || !isCasedRune(rs[j])
}

// isDigitRune is Python's str.isdigit for one rune: Unicode Numeric_Type
// Decimal (exactly general category Nd) or Digit.
func isDigitRune(r rune) bool {
	return unicode.Is(unicode.Nd, r) || unicode.Is(numericTypeDigit, r)
}

// numericTypeDigit is every code point whose Unicode Numeric_Type is Digit:
// digits that are not decimal digits, such as superscripts and circled digits.
// The Go standard library has no Numeric_Type table. Every member is general
// category No. The table is the set of code points for which CPython 3.14
// (Unicode 16.0) answers str.isdigit() true and str.isdecimal() false.
var numericTypeDigit = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00B2, Hi: 0x00B3, Stride: 1},
		{Lo: 0x00B9, Hi: 0x00B9, Stride: 1},
		{Lo: 0x1369, Hi: 0x1371, Stride: 1},
		{Lo: 0x19DA, Hi: 0x19DA, Stride: 1},
		{Lo: 0x2070, Hi: 0x2070, Stride: 1},
		{Lo: 0x2074, Hi: 0x2079, Stride: 1},
		{Lo: 0x2080, Hi: 0x2089, Stride: 1},
		{Lo: 0x2460, Hi: 0x2468, Stride: 1},
		{Lo: 0x2474, Hi: 0x247C, Stride: 1},
		{Lo: 0x2488, Hi: 0x2490, Stride: 1},
		{Lo: 0x24EA, Hi: 0x24EA, Stride: 1},
		{Lo: 0x24F5, Hi: 0x24FD, Stride: 1},
		{Lo: 0x24FF, Hi: 0x24FF, Stride: 1},
		{Lo: 0x2776, Hi: 0x277E, Stride: 1},
		{Lo: 0x2780, Hi: 0x2788, Stride: 1},
		{Lo: 0x278A, Hi: 0x2792, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x10A40, Hi: 0x10A43, Stride: 1},
		{Lo: 0x10E60, Hi: 0x10E68, Stride: 1},
		{Lo: 0x11052, Hi: 0x1105A, Stride: 1},
		{Lo: 0x1F100, Hi: 0x1F10A, Stride: 1},
	},
}

// isAlnumRune is Python's str.isalnum for one rune: alphabetic (general
// category L) or any Unicode Numeric_Type. Every code point with a
// Numeric_Type is in general category N or L, so category N covers the numeric
// half exactly.
func isAlnumRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

// isSpaceRune is Python's str.isspace for one rune: general category Zs, Zl or
// Zp, or bidirectional class WS, B or S. That is Unicode White_Space, which
// unicode.IsSpace tests, plus the four information separators U+001C–U+001F
// (bidirectional class B and S, but not White_Space).
func isSpaceRune(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1C && r <= 0x1F)
}

// isUpperRune and isLowerRune are the Unicode Uppercase and Lowercase derived
// properties (general category Lu or Ll, plus Other_Uppercase or
// Other_Lowercase), which Python's case predicates use. Category alone misses
// "Ⓐ" (So, Other_Uppercase) and "ª" (Lo, Other_Lowercase).
func isUpperRune(r rune) bool {
	return unicode.IsUpper(r) || unicode.Is(unicode.Other_Uppercase, r)
}

func isLowerRune(r rune) bool {
	return unicode.IsLower(r) || unicode.Is(unicode.Other_Lowercase, r)
}

// isCasedRune is the Unicode Cased property: Uppercase, Lowercase or general
// category Lt.
func isCasedRune(r rune) bool {
	return isUpperRune(r) || isLowerRune(r) || unicode.IsTitle(r)
}

// isCaseIgnorableRune is the Unicode Case_Ignorable property, which the Go
// standard library does not provide: general category Mn, Me, Cf, Lm or Sk, or
// Word_Break MidLetter, MidNumLet or Single_Quote. The Word_Break members are
// listed explicitly from Unicode's WordBreakProperty.txt.
func isCaseIgnorableRune(r rune) bool {
	switch r {
	case '\'', '.', ':', 0x00B7, 0x0387, 0x055F, 0x05F4, 0x2018, 0x2019, 0x2024,
		0x2027, 0xFE13, 0xFE52, 0xFE55, 0xFF07, 0xFF0E, 0xFF1A:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
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

// checkCaseInputBytes is a conservative pre-check that runs before a case
// transform, using caseMapWorstCaseExpansion to reject an input that cannot
// possibly fit under the bound no matter how it expands — without paying for
// the transform at all. It is not exact: most input expands by far less than
// the worst case, so an input that passes here can still be rejected
// afterward by boundedString's exact post-check on the actual result. That
// asymmetry is the point — a cheap, sound pre-check that only ever rejects
// inputs that were always going to fail, paired with the expensive exact
// check that catches everything else. Without it, upper() on a 10MB string of
// U+0390 allocates far more than its 30MB result before refusing it; this
// pre-check rejects the same input before allocating.
func checkCaseInputBytes(n int) error {
	if n < 0 || n > maxStringBytes/caseMapWorstCaseExpansion {
		return fmt.Errorf("%w: %d input bytes could exceed %d bytes after case mapping",
			errTooLarge, n, maxStringBytes)
	}
	return nil
}

// boundedString wraps a produced string in the package's exact size bound —
// the check that runs after a transform, on the real result, and is not
// fooled by an input that stayed under checkCaseInputBytes's conservative
// pre-check but still expanded close to the worst case in practice.
//
// Case mapping can grow a string: lowercasing "İ" (U+0130, LATIN CAPITAL
// LETTER I WITH DOT ABOVE) produces "i" plus a combining dot above, 2 input
// bytes to 3 output bytes, so a result built from an argument already near
// the limit can exceed it. The check is cheap and makes every string function
// bound its own single operation.
func boundedString(s string) (Value, error) {
	if err := checkStringBytes(len(s)); err != nil {
		return Value{}, err
	}
	return String(s), nil
}

// allRunes reports whether every rune of s satisfies pred and s is non-empty,
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

// allCased reports whether every cased rune of s satisfies pred and at least
// one cased rune exists — RFC 0006's rule for isupper and islower.
//
// "Cased" includes titlecase, which is why isupper("ǲ") is false rather than
// true: U+01F2 is a cased rune that is not uppercase, so it fails pred instead
// of being skipped.
func allCased(s string, pred func(rune) bool) bool {
	seen := false
	for _, r := range s {
		if !isCasedRune(r) {
			continue
		}
		seen = true
		if !pred(r) {
			return false
		}
	}
	return seen
}
