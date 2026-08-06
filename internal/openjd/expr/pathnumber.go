// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"regexp"
	"strconv"
)

// errPaddingTooWide backs with_number when a %0Nd printf specifier or a #
// run asks for more than maxNumberPadding characters of padding. Measured
// against the reference (openjd-model 0.11.1): %099d and a 33-character #
// run both fail this way; %032d and a 32-character # run are both accepted.
//
// The width is INTERPOLATED FROM THE CONSTANT rather than written out beside
// it: the message and maxNumberPadding stated "32" twice, and a message is
// exactly the kind of second copy that comes to lie the first time the limit
// moves. The function-name prefix the message used to carry is gone, so this
// reads in the same register as funcspath.go's five path errors, none of which
// names the function it backs — the caller supplies that context already.
var errPaddingTooWide = fmt.Errorf(
	"a padding width may not exceed %d characters", maxNumberPadding,
)

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
//
// A hash run immediately followed by a digit run — "###003", "##a3" — is TWO
// candidates, not one: "#+" stops at the first non-'#' character, so it
// never reaches into the digits (or letters) that follow, and the LAST
// candidate (the digit run, when there is one) is what withNumber replaces,
// per RFC 0006 line 822's "replaces the last match found" — a match, not
// "the match through the end of the stem". with_number("###003", 7) is
// therefore "###007": the "###" is untouched literal text, exactly as if it
// were any other prefix like "shot01_####". The reference implementation
// (openjd-model 0.11.1) disagrees — it lets a hash run consume the rest of
// the stem, so with_number("##a3", 7) comes back "07" there, DESTROYING the
// literal "a" that was in the input — which is a reference defect against
// its own specification's "replaces the last match found" wording, not a
// second valid reading of it. Confirmed adjudicated in sqi's favor during
// Task 8 review; do not "fix" this file to match the reference. See
// TestWithNumber_HashThenDigits and this task's report for the full
// side-by-side.
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
// stem, lastNumberMatch finds the LAST candidate — by START POSITION, not by
// which pattern kind it is — and only that one is replaced. That is what lets
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
	start, end, found := lastNumberMatch(stem)
	if !found {
		padded, err := paddedNumber(n, 4)
		if err != nil {
			return "", err
		}
		return stem + "_" + padded + suffix, nil
	}
	replacement, err := numberReplacement(stem[start:end], n)
	if err != nil {
		return "", err
	}
	return stem[:start] + replacement + stem[end:] + suffix, nil
}

// lastNumberMatch reports the byte range of the LAST numberPattern candidate
// in stem, or found == false when the stem holds none.
//
// IT SCANS RATHER THAN ENUMERATING, and that is a memory bound, not a
// micro-optimization. The original wrote numberPattern.FindAllStringIndex(stem,
// -1) and then took the final element — materializing every candidate to use
// exactly one. The "-1" is unbounded, and funcsre.go's reSub already stated the
// rule this file did not carry across: "an unbounded -1 here would let a
// zero-width pattern enumerate maxStringBytes+1 matches before anything
// downstream could object." C3's reSub, reFindAll and reMatchCount all pass
// maxElements+1 for that reason. Here a bound is not needed at all, because
// nothing is accumulated: successive FindStringIndex calls over the shrinking
// remainder keep ONE pair of indices live however many candidates the stem has,
// so with_number('#a' * 5000000, 7) — every operand inside maxStringBytes —
// costs a small multiple of the input instead of the 794 MB the enumerating
// form measured. Pinned by TestWithNumber_ScanIsAllocationBounded, which
// asserts cumulative bytes rather than allocation count, since the two forms'
// allocation COUNTS are indistinguishable.
//
// Searching a SUFFIX of the stem gives the same answer as searching the whole
// stem because numberPattern contains no anchor, boundary or lookaround — it is
// an alternation of literal character runs, so no match's admissibility depends
// on what precedes it. The zero-width guard below is defensive rather than
// reachable: every alternative requires at least one character ("%d" at the
// shortest), so loc[1] > loc[0] always holds today; it is there so that
// widening the pattern to something nullable cannot silently turn this loop
// into a non-terminating one.
func lastNumberMatch(stem string) (start, end int, found bool) {
	for off := 0; off <= len(stem); {
		loc := numberPattern.FindStringIndex(stem[off:])
		if loc == nil {
			break
		}
		start, end, found = off+loc[0], off+loc[1], true
		if loc[1] == loc[0] {
			off = end + 1
			continue
		}
		off = end
	}
	return start, end, found
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
// N, which numberPattern guarantees contains only digits — but NOT that
// those digits fit in an int: "%0999999999999999999999d" is a well-formed
// match whose width literal overflows strconv.Atoi. That overflow is itself
// proof the requested width exceeds maxNumberPadding (32), so it is reported
// as errPaddingTooWide rather than leaking Atoi's raw *NumError — a caller
// matching on the sentinel via errors.Is must see it regardless of which of
// the two ways "too wide" was detected. The reference silently ACCEPTS this
// input with no padding at all, which given RFC 0006 line 831's explicit
// "wider printf or hash patterns are an error" is a second reference defect
// in the same neighborhood as the hash-then-digit one above; sqi erroring
// here is correct, only the error VALUE was wrong before this fix.
func printfReplacement(candidate string, n int64) (string, error) {
	digits := candidate[1 : len(candidate)-1]
	if digits == "" {
		return strconv.FormatInt(n, 10), nil
	}
	width, err := strconv.Atoi(digits)
	if err != nil {
		return "", errPaddingTooWide
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
