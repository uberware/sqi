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
// Tasks 2 and 3 add the translation half; this file starts as validation only.
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
// Task 2 replaces the shorthand arm and Task 3 the \u/\U arm; this version
// rejects and passes through.
func scanEscape(out *strings.Builder, pattern string, i int, inClass, classNegated bool) (int, error) {
	if i+1 >= len(pattern) {
		return 0, fmt.Errorf("%w: the pattern ends with a trailing backslash", errUnsupportedRegex)
	}
	c := pattern[i+1]
	switch {
	case c == 'z' || c == 'Z':
		return 0, fmt.Errorf(`%w: the end-of-string anchor \%c is not in the Python/Rust intersection; use $`, errUnsupportedRegex, c)
	case c == 'p' || c == 'P':
		return 0, fmt.Errorf(`%w: the Unicode property escape \%c{...} is Rust-only; Python's re rejects it`, errUnsupportedRegex, c)
	case c >= '1' && c <= '9':
		return 0, fmt.Errorf(`%w: backreferences such as \%c are not supported`, errUnsupportedRegex, c)
	case (c == 'x' || c == 'u' || c == 'U') && strings.HasPrefix(pattern[i+2:], "{"):
		return 0, fmt.Errorf(`%w: the brace escape \%c{...} is Rust-only; use \xHH, \uHHHH or \UHHHHHHHH`, errUnsupportedRegex, c)
	case (c == 'W' || c == 'S') && inClass:
		// Reaching here means a NEGATED shorthand inside a class that
		// scanClass did not lift into an alternation — either the class is
		// itself negated (set subtraction, which RE2 cannot express) or it
		// already spent its one alternation on the other shorthand. Both are
		// refusals. This must NOT fall through to writeSetEscape: that
		// function cannot express a negated set as class contents, so it would
		// emit the POSITIVE set and silently turn \S into \s.
		return 0, negatedClassShorthandErr(c, classNegated)
	}
	out.WriteByte('\\')
	out.WriteByte(c)
	return 1, nil
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
