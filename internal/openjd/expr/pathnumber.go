// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"regexp"
	"strconv"
)

// errPaddingTooWide backs with_number when a %0Nd printf specifier or a #
// run asks for more than maxNumberPadding characters of padding. Measured
// against the reference (openjd-model 0.11.1): %099d and a 33-character #
// run both fail this way; %032d and a 32-character # run are both accepted.
var errPaddingTooWide = errors.New("with_number: padding width exceeds maximum of 32")

// maxNumberPadding is RFC 0006's own limit on with_number's printf and hash
// padding widths ("The maximum padding width is 32 characters; wider printf
// or hash patterns are an error."). It does NOT bound a plain digit run —
// the specification never restricts that one, and measurement against the
// reference agrees: a digit run wider than 32 already present in the name is
// left alone, because its "width" is just how many digit characters were
// already there, not a padding request.
const maxNumberPadding = 32

// numberPattern finds a with_number frame-number placeholder in a stem: a
// printf specifier (%d or %0Nd), a run of one or more '#', or a run of one or
// more digits — in that ALTERNATION ORDER, which is load-bearing. Go's
// regexp package matches alternatives leftmost-first (the same "backtracking
// search" order Perl and Python use), not leftmost-longest: at the position
// where "%04d" begins, the printf alternative is tried first and consumes
// the WHOLE token, so the digit-run alternative never gets a separate chance
// to match the "04" embedded inside it. Reusing this single regex is what
// keeps that embedding rule in ONE place rather than something withNumber
// would otherwise have to re-derive as "skip digits already inside a printf
// match".
var numberPattern = regexp.MustCompile(`%(0\d+)?d|#+|\d+`)

// withNumber implements RFC 0006's with_number frame-number substitution on
// a bare final-component NAME. It knows nothing about paths — no flavor, no
// separators, no root — which is why this file stands apart from
// pathval.go/funcspath.go beyond splitStemSuffix: the string form
// (with_number(s, n)) calls this directly on its whole argument, and the
// path form (funcspath.go) calls it on the receiver's name and then rebuilds
// a path through withName, the SAME replacement-name validation every other
// with_* function uses, rather than a second one grown here.
//
// The scan runs over the STEM only, per the specification: splitStemSuffix
// splits at the LAST dot (matching pathlib's .stem), so "render.0001.exr"
// keeps its ".exr" suffix untouched and a version-looking suffix like ".v2"
// on "file.v2.003.exr" is never mistaken for the number itself. Within the
// stem, numberPattern finds every candidate; the LAST one — by START
// POSITION, not by which pattern kind it is — is replaced. That is what lets
// "shot01_####" replace only the hashes (the digits in "01" are never a
// candidate on their own; "####" starts later) and what makes
// "f_%d_abc_###.exr" replace the "###" while "file_%04d_003.exr" replaces
// the "003" instead, even though both share a stem with two candidates: in
// each case the SECOND candidate simply starts later in the text.
//
// When no candidate is found at all, "_" plus the number padded to 4 is
// appended to the stem — RFC 0006 states this fallback explicitly, and
// leaves it as an open question whether it should be an error instead; sqi
// follows the specification's stated behavior rather than the open question.
func withNumber(name string, n int64) (string, error) {
	stem, suffix := splitStemSuffix(name)
	matches := numberPattern.FindAllStringIndex(stem, -1)
	if len(matches) == 0 {
		padded, err := paddedNumber(n, 4)
		if err != nil {
			return "", err
		}
		return stem + "_" + padded + suffix, nil
	}
	last := matches[len(matches)-1]
	start, end := last[0], last[1]
	replacement, err := numberReplacement(stem[start:end], n)
	if err != nil {
		return "", err
	}
	return stem[:start] + replacement + stem[end:] + suffix, nil
}

// numberReplacement renders n for ONE matched candidate, dispatching on the
// candidate's own leading character: '%' is a printf specifier, handled
// separately because its width comes from a captured digit string rather
// than the candidate's own length; a '#' run and a plain digit run both pad
// to their OWN length, but only the '#' run is capped by maxNumberPadding —
// a digit run's width is just however many digits the input already had,
// which the specification never restricts.
func numberReplacement(candidate string, n int64) (string, error) {
	switch candidate[0] {
	case '%':
		return printfReplacement(candidate, n)
	case '#':
		if len(candidate) > maxNumberPadding {
			return "", errPaddingTooWide
		}
		return paddedNumber(n, len(candidate))
	default:
		return paddedNumber(n, len(candidate))
	}
}

// printfReplacement renders n for a printf-style candidate. candidate is
// either the bare "%d" (no padding at all) or "%0Nd" for some digit string
// N, which numberPattern guarantees is well-formed — slicing off the
// leading "%" and trailing "d" recovers exactly the captured width text
// with no further parsing needed.
func printfReplacement(candidate string, n int64) (string, error) {
	digits := candidate[1 : len(candidate)-1]
	if digits == "" {
		return strconv.FormatInt(n, 10), nil
	}
	width, err := strconv.Atoi(digits)
	if err != nil {
		return "", err
	}
	if width > maxNumberPadding {
		return "", errPaddingTooWide
	}
	return paddedNumber(n, width)
}

// paddedNumber renders n zero-padded to width VISIBLE characters, with the
// sign counting toward that width — RFC 0006's own rule for with_number
// ("file_003.exr" with with_number(-1) is "file_-01.exr", 3 characters, not
// 4) is exactly funcsstrpad.go's zfillString rule for zfill (zfill(-1, 3) is
// "-01", not "0-1"), so this defers to that formula rather than writing a
// second sign-then-zeros implementation beside it. width == 0 asks for no
// padding at all, which zfillString already gives for free: a width at or
// below the rendered number's own length is a no-op — that is what makes the
// bare "%d" row (no padding) and the "overflow never truncates" rule (a
// number wider than its padding) both fall out of the SAME call rather than
// needing their own cases here.
func paddedNumber(n int64, width int) (string, error) {
	v, err := zfillString(strconv.FormatInt(n, 10), int64(width))
	if err != nil {
		return "", err
	}
	return v.AsStr(), nil
}
