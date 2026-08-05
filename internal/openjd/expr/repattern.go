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
// The translation half is now partly in place: the Unicode shorthands (\d,
// \D, \w, \W, \s, \S) are rewritten by scanEscape below. Translating \u and
// \U into a Go-compatible escape is Task 3's remaining scope.
func translatePattern(pattern string) (string, error) {
	if pattern == "" {
		return "", errEmptyPattern
	}
	var out strings.Builder
	out.Grow(len(pattern) * 2)
	inClass := false
	classNegated := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '\\':
			n, err := scanEscape(&out, pattern, i, inClass, classNegated)
			if err != nil {
				return "", err
			}
			i += n
		case !inClass && c == '(':
			n, err := scanGroup(&out, pattern, i)
			if err != nil {
				return "", err
			}
			i += n
		case !inClass && c == '[':
			if strings.HasPrefix(pattern[i:], "[[:") {
				return "", fmt.Errorf("%w: POSIX character classes such as [[:alpha:]] are not in the Python/Rust intersection", errUnsupportedRegex)
			}
			inClass = true
			classNegated = strings.HasPrefix(pattern[i+1:], "^")
			out.WriteByte(c)
		case inClass && c == ']':
			inClass = false
			classNegated = false
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
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
// writeSetEscape), and otherwise passes the escape through unchanged. \u and
// \U translation is still Task 3's: today their Rust-only brace form
// (\u{...}, \U{...}) is rejected and their plain form passes through
// unchanged rather than becoming a Go-compatible escape.
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
		// Today this refuses EVERY \W or \S found inside a character class,
		// negated or not, first occurrence or not: there is no
		// alternation-lifting yet to make room for one permitted occurrence
		// (that is Task 4's work). Until then, this is the first guard
		// against ever reaching writeSetEscape with a negated set inside a
		// class — a combination that function cannot express as class
		// contents and panics on as a backstop.
		return negatedClassShorthandErr(c, classNegated)
	}
	return nil
}

// writeSetEscape emits a union shorthand: bracketed when it stands alone, bare
// contents when it is contributing to an enclosing class.
//
// A NEGATED set is only ever reached outside a class today, and that is
// enforced by the caller rather than assumed here: rejectUnsupportedEscape
// refuses \W and \S inside ANY class unconditionally (Task 4 will relax this
// to allow one lifted occurrence into an alternation). The reason the
// invariant matters regardless of how it ends up enforced: there IS no way to
// write a negated set as class CONTENTS — so if this were ever reached with
// inClass and negate both true it would emit the positive set and silently
// invert the match. It panics instead.
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

// negatedClassShorthandErr explains why \W or \S cannot appear inside a
// character class, split out of scanEscape to keep it under the complexity
// cap without disturbing the arm ordering Tasks 2 and 3 extend.
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
