// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// errEmptyPattern is RFC 0006's rule that every regex function's pattern
	// argument must be non-empty. It is separate from errUnsupportedRegex
	// because four conformance fixtures test it on its own.
	errEmptyPattern = errors.New("the regular expression pattern must not be empty")
	// errUnsupportedRegex is every other refusal: a construct outside the
	// specification's dialect.
	errUnsupportedRegex = errors.New("unsupported regular expression feature")
)

// unicodeSpaceSet is the CONTENTS of a character class matching Unicode's
// White_Space property, which is what RFC 0006 means by \s.
//
// It is written out because \p{White_Space} does NOT work: Go's regexp
// supports Unicode categories and scripts, not properties, and rejects that
// escape outright — measured during design. \p{Zs} covers the space
// separators; the rest of White_Space is the C0 controls U+0009-U+000D plus
// U+0085, U+2028 and U+2029.
//
// TestUnicodeSpaceSet_MatchesWhiteSpace checks every member and three
// non-members, so a drift from the real property fails loudly.
const unicodeSpaceSet = `\t\n\v\f\r\x{0085}\x{2028}\x{2029}\p{Zs}`

// unicodeWordSet is the same thing for \w: letters, numbers and underscore.
const unicodeWordSet = `\p{L}\p{N}_`

// translatePattern converts a pattern written in RFC 0006's dialect into an
// equivalent pattern for Go's regexp package, or rejects it.
//
// THIS FUNCTION EXISTS BECAUSE THE THREE ENGINES DISAGREE IN THREE DIRECTIONS.
// RFC 0006 does not adopt an existing dialect; it defines one, as "the
// intersection of Python's re module and Rust's regex crate". Go's regexp is
// neither, and all three differences were measured rather than assumed:
//
//   - Go ACCEPTS what the specification forbids: "\z" and "\x{...}" compile
//     fine, and conformance fixtures test "\z" by name.
//   - Go REJECTS what the specification requires: "\uHHHH" and "\UHHHHHHHH"
//     are supported by Python and Rust and named as supported by the spec, but
//     Go's regexp has no "\u" escape at all.
//   - Go's shorthands MEAN SOMETHING DIFFERENT: "\d", "\w" and "\s" are
//     ASCII-only in Go, while the spec mandates Unicode semantics explicitly.
//     Measured: Go's "\d" does not match "٣"; the reference's does.
//
// The scanner carries two bits of state, in-class and after-backslash, and both
// are load-bearing rather than defensive. "[[\p{L}]]" COMPILES in Go and
// matches the wrong thing rather than erroring, so a context-blind rewrite
// fails silently. That single fact is why this is a scanner and not a
// strings.ReplaceAll.
//
// The translation half is now fully in place: the Unicode shorthands (\d,
// \D, \w, \W, \s, \S) and the \u/\U fixed-width Unicode escapes are all
// rewritten by scanEscape below, and a positive class holding \W or \S is
// rebuilt into an alternation by scanClass — the one rewrite that replaces a
// whole enclosing construct rather than a single escape.
func translatePattern(pattern string) (string, error) {
	if pattern == "" {
		return "", errEmptyPattern
	}
	var out strings.Builder
	out.Grow(len(pattern) * 2)
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '\\':
			n, err := scanEscape(&out, pattern, i, false, false)
			if err != nil {
				return "", err
			}
			i += n
		case '(':
			n, err := scanGroup(&out, pattern, i)
			if err != nil {
				return "", err
			}
			i += n
		case '[':
			n, err := scanClass(&out, pattern, i)
			if err != nil {
				return "", err
			}
			i += n
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), nil
}

// scanClass translates one character class, from its "[" to its matching "]".
// It returns how many EXTRA bytes it consumed beyond the "[".
//
// Two shapes come out. Ordinarily the class stays a class and its members are
// translated in place (writeOrdinaryClass). But if a POSITIVE class contains
// \W or \S — a negated union, which Go cannot union with anything else — the
// whole class is rebuilt as an alternation of its parts
// (writeClassAlternation). See TestTranslatePattern_NegatedShorthandInPositiveClass
// for why that is sound. The delimiter scan itself is split into
// scanClassBody purely to keep this function under the repo's complexity cap.
func scanClass(out *strings.Builder, pattern string, i int) (int, error) {
	if strings.HasPrefix(pattern[i:], "[[:") {
		return 0, fmt.Errorf("%w: POSIX character classes such as [[:alpha:]] are not in the Python/Rust intersection", errUnsupportedRegex)
	}
	body, negated, consumed, err := scanClassBody(pattern, i)
	if err != nil {
		return 0, err
	}

	negUnion, err := classNeedsAlternation(body, negated)
	if err != nil {
		return 0, err
	}
	if negUnion == "" {
		if err := writeOrdinaryClass(out, body, negated); err != nil {
			return 0, err
		}
		return consumed, nil
	}
	if err := writeClassAlternation(out, body, negUnion); err != nil {
		return 0, err
	}
	return consumed, nil
}

// scanClassBody scans from the "[" at pattern[i] to its matching "]",
// returning the raw (untranslated) member bytes, whether the class opened
// with "^", and how many EXTRA bytes were consumed beyond the "[". Split out
// of scanClass solely to keep that function's cyclomatic complexity under the
// repo's cap.
func scanClassBody(pattern string, i int) (body string, negated bool, consumed int, err error) {
	j := i + 1
	if j < len(pattern) && pattern[j] == '^' {
		negated = true
		j++
	}
	var b strings.Builder
	// A "]" as the first member is a literal, per both Python and Rust.
	if j < len(pattern) && pattern[j] == ']' {
		b.WriteByte(']')
		j++
	}
	closed := false
	for ; j < len(pattern); j++ {
		if pattern[j] == '\\' && j+1 < len(pattern) {
			b.WriteString(pattern[j : j+2])
			j++
			continue
		}
		if pattern[j] == ']' {
			closed = true
			break
		}
		b.WriteByte(pattern[j])
	}
	if !closed {
		return "", false, 0, fmt.Errorf("%w: the character class is never closed", errUnsupportedRegex)
	}
	return b.String(), negated, j - i, nil
}

// writeOrdinaryClass writes a class whose members translate in place: the
// shape scanClass takes when the body holds no \W or \S needing alternation.
func writeOrdinaryClass(out *strings.Builder, body string, negated bool) error {
	var inner strings.Builder
	if err := translateClassBody(&inner, body, negated); err != nil {
		return err
	}
	out.WriteByte('[')
	if negated {
		out.WriteByte('^')
	}
	out.WriteString(inner.String())
	out.WriteByte(']')
	return nil
}

// writeClassAlternation writes the alternation form: the negated union
// classNeedsAlternation pulled out, then everything else as an ordinary
// class, provided anything else remains.
func writeClassAlternation(out *strings.Builder, body, negUnion string) error {
	rest, err := classBodyWithout(body, negUnion)
	if err != nil {
		return err
	}
	out.WriteString("(?:")
	out.WriteString(negUnion)
	if rest != "" {
		out.WriteString("|[")
		out.WriteString(rest)
		out.WriteByte(']')
	}
	out.WriteByte(')')
	return nil
}

// classNeedsAlternation reports the bracketed negated set a positive class must
// be rebuilt around, or "" when the class translates in place.
//
// A NEGATED class containing \W or \S is rejected rather than rebuilt: that
// asks for word characters MINUS something, which is set subtraction, and RE2
// has no such operator.
func classNeedsAlternation(body string, negated bool) (string, error) {
	for i := 0; i+1 < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		set := ""
		switch body[i+1] {
		case 'W':
			set = "[^" + unicodeWordSet + "]"
		case 'S':
			set = "[^" + unicodeSpaceSet + "]"
		default:
			i++
			continue
		}
		if negated {
			return "", fmt.Errorf(`%w: \%c inside a negated character class needs set subtraction, which Go's engine cannot express`, errUnsupportedRegex, body[i+1])
		}
		return set, nil
	}
	return "", nil
}

// translateClassBody rewrites the members of a class that stays a class.
func translateClassBody(out *strings.Builder, body string, negated bool) error {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			out.WriteByte(body[i])
			continue
		}
		n, err := scanEscape(out, body, i, true, negated)
		if err != nil {
			return err
		}
		i += n
	}
	return nil
}

// classBodyWithout returns the class members other than the one negated
// shorthand that classNeedsAlternation pulled out, already translated.
func classBodyWithout(body, pulled string) (string, error) {
	drop := byte('W')
	if strings.Contains(pulled, unicodeSpaceSet) {
		drop = 'S'
	}
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			if body[i+1] == drop {
				i++
				continue
			}
			n, err := scanEscape(&out, body, i, true, false)
			if err != nil {
				return "", err
			}
			i += n
			continue
		}
		out.WriteByte(body[i])
	}
	return out.String(), nil
}

// scanEscape handles the backslash escape starting at pattern[i], writing its
// translation to out. It returns how many EXTRA bytes it consumed beyond the
// backslash.
//
// It rejects the constructs outside the Python/Rust intersection
// (rejectUnsupportedEscape), rewrites the six Unicode shorthands \d, \D, \w,
// \W, \s and \S to their Go equivalents (the switch below, via
// writeSetEscape), rewrites \uHHHH and \UHHHHHHHH into Go's \x{...} spelling
// (scanFixedHex), and otherwise passes the escape through unchanged.
func scanEscape(out *strings.Builder, pattern string, i int, inClass, classNegated bool) (int, error) {
	if i+1 >= len(pattern) {
		return 0, fmt.Errorf("%w: the pattern ends with a trailing backslash", errUnsupportedRegex)
	}
	c := pattern[i+1]
	// Rejections must fire before any Unicode shorthand translation below —
	// split out to keep scanEscape's own complexity under the repo's cap
	// without disturbing that ordering.
	if err := rejectUnsupportedEscape(pattern, i, c, inClass, classNegated); err != nil {
		return 0, err
	}
	switch c {
	// \u and \U: the two escapes the specification REQUIRES that Go's regexp
	// cannot parse at all (see scanFixedHex). Ordered ahead of the Unicode
	// shorthand rewrites below since both groups are translations, not
	// rejections, and this pair was the last one Task 3 added.
	case 'u':
		return scanFixedHex(out, pattern, i, 4)
	case 'U':
		return scanFixedHex(out, pattern, i, 8)
	// The Unicode shorthand rewrites. \d and \D map to a single negatable
	// property, so one spelling works in both positions. \w and \s are UNIONS,
	// so outside a class they need their own brackets and inside one they must
	// contribute only their contents.
	case 'd':
		out.WriteString(`\p{Nd}`)
	case 'D':
		out.WriteString(`\P{Nd}`)
	case 'w':
		writeSetEscape(out, unicodeWordSet, inClass, false)
	case 'W':
		writeSetEscape(out, unicodeWordSet, inClass, true)
	case 's':
		writeSetEscape(out, unicodeSpaceSet, inClass, false)
	case 'S':
		writeSetEscape(out, unicodeSpaceSet, inClass, true)
	default:
		out.WriteByte('\\')
		out.WriteByte(c)
	}
	return 1, nil
}

// rejectUnsupportedEscape reports the refusals that must fire before any
// Unicode shorthand is translated: the anchors, property escapes,
// backreferences, brace escapes and negated-shorthand-inside-a-class cases.
// Extracted from scanEscape solely to keep that function's cyclomatic
// complexity under the repo's cap (adding the six shorthand translation arms
// pushed it back over, as it did once before for the same reason — see
// negatedClassShorthandErr). Returns nil when c names no rejection, in which
// case scanEscape's own switch decides what to do with it.
func rejectUnsupportedEscape(pattern string, i int, c byte, inClass, classNegated bool) error {
	switch {
	case c == 'z' || c == 'Z':
		return fmt.Errorf(`%w: the end-of-string anchor \%c is not in the Python/Rust intersection; use $`, errUnsupportedRegex, c)
	case c == 'p' || c == 'P':
		return fmt.Errorf(`%w: the Unicode property escape \%c{...} is Rust-only; Python's re rejects it`, errUnsupportedRegex, c)
	case c >= '1' && c <= '9':
		return fmt.Errorf(`%w: backreferences such as \%c are not supported`, errUnsupportedRegex, c)
	case (c == 'x' || c == 'u' || c == 'U') && strings.HasPrefix(pattern[i+2:], "{"):
		return fmt.Errorf(`%w: the brace escape \%c{...} is Rust-only; use \xHH, \uHHHH or \UHHHHHHHH`, errUnsupportedRegex, c)
	case (c == 'W' || c == 'S') && inClass:
		// By the time scanEscape sees a \W or \S with inClass true, scanClass's
		// classNeedsAlternation has already handled both LEGAL shapes: a
		// negated class (rejected there, with its own message, before ever
		// reaching here) and a positive class's first \W/\S (pulled out into
		// the alternation by classBodyWithout and never passed to scanEscape
		// at all). So this arm fires only for what classNeedsAlternation does
		// NOT catch: a SECOND \W or \S left in a positive class's remainder,
		// where classNegated is always false. It remains the backstop against
		// ever reaching writeSetEscape with a negated set inside a class — a
		// combination that function cannot express as class contents and
		// panics on.
		return negatedClassShorthandErr(c, classNegated)
	}
	return nil
}

// writeSetEscape emits a union shorthand: bracketed when it stands alone, bare
// contents when it is contributing to an enclosing class.
//
// A NEGATED set is only ever reached outside a class, and that is enforced by
// the caller rather than assumed here: whenever scanEscape's own switch would
// otherwise reach the \W or \S arm with inClass true, rejectUnsupportedEscape
// has already returned an error for it first (see that function's inClass
// case) — every \W/\S that reaches scanEscape from inside a class is, by
// scanClass's construction, one classNeedsAlternation did NOT already pull
// out (the one it does pull out is excluded from the call entirely, by
// translateClassBody never running when the body holds one and by
// classBodyWithout skipping the one it dropped), so it is always the
// disallowed extra. The reason the invariant matters regardless of how it
// ends up enforced: there IS no way to write a negated set as class CONTENTS
// — so if this were ever reached with inClass and negate both true it would
// emit the positive set and silently invert the match. It panics instead.
func writeSetEscape(out *strings.Builder, set string, inClass, negate bool) {
	if inClass {
		if negate {
			panic("expr: negated shorthand reached writeSetEscape inside a class; scanEscape should have refused it")
		}
		out.WriteString(set)
		return
	}
	out.WriteByte('[')
	if negate {
		out.WriteByte('^')
	}
	out.WriteString(set)
	out.WriteByte(']')
}

// negatedClassShorthandErr explains why a \W or \S that reaches scanEscape
// inside a class cannot be honored there — split out of scanEscape to keep it
// under the complexity cap without disturbing the arm ordering Tasks 2 and 3
// extend. classNegated is always false when this is called as
// rejectUnsupportedEscape's backstop for a second \W/\S in a positive class's
// remainder (see that call site); a genuinely negated class is rejected
// earlier, by classNeedsAlternation, using its own message.
func negatedClassShorthandErr(c byte, classNegated bool) error {
	if classNegated {
		return fmt.Errorf(`%w: \%c inside a negated character class needs set subtraction, which Go's engine cannot express`, errUnsupportedRegex, c)
	}
	return fmt.Errorf(`%w: a character class may contain at most one of \W or \S`, errUnsupportedRegex)
}

// scanGroup handles the group opener starting at pattern[i]. It returns how
// many EXTRA bytes it consumed beyond the "(".
//
// Go rejects lookaround and (?P=name) on its own, but with Go's wording. These
// refusals name the construct the specification names, because sixteen
// conformance fixtures test them by name and a generic "invalid pattern" would
// make it impossible to tell which rule fired.
func scanGroup(out *strings.Builder, pattern string, i int) (int, error) {
	rest := pattern[i:]
	switch {
	case strings.HasPrefix(rest, "(?=") || strings.HasPrefix(rest, "(?!"):
		return 0, fmt.Errorf("%w: lookahead is not supported", errUnsupportedRegex)
	case strings.HasPrefix(rest, "(?<=") || strings.HasPrefix(rest, "(?<!"):
		return 0, fmt.Errorf("%w: lookbehind is not supported", errUnsupportedRegex)
	case strings.HasPrefix(rest, "(?P="):
		return 0, fmt.Errorf("%w: named backreferences are not supported", errUnsupportedRegex)
	case strings.HasPrefix(rest, "(?("):
		return 0, fmt.Errorf("%w: conditional patterns are not supported", errUnsupportedRegex)
	case strings.HasPrefix(rest, "(?<"):
		// Reached only after the lookbehind cases above, so this is Rust's
		// (?<name>...) spelling. Python's re requires (?P<name>...) and
		// rejects this one.
		return 0, fmt.Errorf("%w: the named group spelling (?<name>...) is Rust-only; use (?P<name>...)", errUnsupportedRegex)
	}
	out.WriteByte('(')
	return 0, nil
}

// scanFixedHex translates \uHHHH and \UHHHHHHHH into Go's \x{...} spelling.
//
// Go's regexp has neither escape, so passing them through would produce
// "invalid escape sequence" from the engine rather than a working pattern —
// and the specification names both as supported, so refusing them is not an
// option either.
//
// This is the mirror image of rejectUnsupportedEscape's brace-escape case,
// not a contradiction of it: \x{...}/\u{...}/\U{...} is Rust-only brace
// syntax, outside the Python/Rust intersection, so the scanner refuses it on
// INPUT. That says nothing about what Go itself accepts — \x{HHHH} is exactly
// how Go spells a codepoint escape, so it is what this function EMITS. The
// rejection governs what a template may write; this emission governs what
// Go's engine is handed.
func scanFixedHex(out *strings.Builder, pattern string, i, digits int) (int, error) {
	start := i + 2
	if start+digits > len(pattern) {
		return 0, fmt.Errorf(`%w: \%c needs %d hexadecimal digits`, errUnsupportedRegex, pattern[i+1], digits)
	}
	hex := pattern[start : start+digits]
	for j := range len(hex) {
		if !isHexDigit(hex[j]) {
			return 0, fmt.Errorf(`%w: \%c needs %d hexadecimal digits, found %q`, errUnsupportedRegex, pattern[i+1], digits, hex)
		}
	}
	out.WriteString(`\x{`)
	out.WriteString(hex)
	out.WriteByte('}')
	return 1 + digits, nil
}

// isHexDigit reports whether b is one of 0-9, a-f or A-F.
func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}
