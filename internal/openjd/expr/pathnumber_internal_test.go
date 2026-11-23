// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// TestWithNumber covers RFC 0006's five substitution formats. Every
// expectation was produced by the reference implementation, and every one
// matches the specification's own table.
//
// The rule that makes this non-obvious: the scan runs from the end of the stem
// and replaces the last match. That is what preserves a shot number —
// "shot01_####" replaces only the hashes — and what makes "a12_b34" replace the
// "34" rather than the "12".
func TestWithNumber(t *testing.T) {
	tests := []struct{ src, want string }{
		{`path('/r/shot_003.exr').with_number(72)`, "/r/shot_072.exr"},
		{`path('/r/shot_%d.exr').with_number(72)`, "/r/shot_72.exr"},
		{`path('/r/shot_%04d.exr').with_number(72)`, "/r/shot_0072.exr"},
		{`path('/r/shot_####.exr').with_number(72)`, "/r/shot_0072.exr"},
		{`path('/r/shot_######.exr').with_number(72)`, "/r/shot_000072.exr"},
		{`path('/r/shot.exr').with_number(72)`, "/r/shot_0072.exr"},
		{`path('/r/f_###.exr').with_number(10000)`, "/r/f_10000.exr"},
		{`path('/r/f_003.exr').with_number(-1)`, "/r/f_-01.exr"},
		{`path('/r/f_####.exr').with_number(-1)`, "/r/f_-001.exr"},
		{`path('/r/f_%04d.exr').with_number(-1)`, "/r/f_-001.exr"},
		{`path('/r/shot01_####.exr').with_number(72)`, "/r/shot01_0072.exr"},
		{`path('/r/a12_b34.exr').with_number(7)`, "/r/a12_b07.exr"},
		{`path('/r/render.0001.exr').with_number(72)`, "/r/render.0072.exr"},
		{`path('/r/shot_003').with_number(72)`, "/r/shot_072"},
		{`with_number('shot_003.exr', 72)`, "shot_072.exr"},
		// mixed_hash_after_printf / mixed_digits_after_printf from the
		// upstream conformance corpus: with two candidates in the stem, the
		// one that starts later wins, regardless of which pattern kind
		// either one is.
		{`path('/out/f_%d_abc_###.exr').with_number(42)`, "/out/f_%d_abc_042.exr"},
		{`path('/out/file_%04d_003.exr').with_number(72)`, "/out/file_%04d_072.exr"},
		// A name that is entirely dots has no suffix at all (splitLeadingDots),
		// so the whole thing is the stem, and no digit/hash/printf pattern is
		// in it — the no-pattern fallback applies. Measured against the
		// reference.
		{`path('/r/...').with_number(3)`, "/r/..._0003"},
		// A DIGIT RUN INSIDE THE SUFFIX is not a candidate, because the scan
		// runs over the stem alone — the case pathnumber.go's own doc comment
		// names ("file.v2.003.exr"). The .exr row alone does not pin it:
		// scanning the whole name instead of the stem gives the identical
		// answer there, since ".exr" holds no digits at all. The .mp4 row is
		// the one that separates the two readings — scanning the name would
		// take the "4" as the last candidate and answer "render.0001.mp72",
		// destroying the container extension. The reference agrees on both.
		{`with_number('file.v2.003.exr', 7)`, "file.v2.007.exr"},
		{`path('/r/render.0001.mp4').with_number(72)`, "/r/render.0072.mp4"},
		// Several suffixes: splitStemSuffix cuts at the last dot only, so
		// "a.tar.gz" has stem "a.tar" (no digit pattern in it) and suffix
		// ".gz" — the no-pattern fallback applies to the stem, not the whole
		// name. Measured against the reference.
		{`path('/r/a.tar.gz').with_number(72)`, "/r/a.tar_0072.gz"},
		// Overflow, zero and a large negative all pad through the same
		// zfillString formula with no special-casing. Measured against the
		// reference.
		{`path('/r/f_003.exr').with_number(0)`, "/r/f_000.exr"},
		{`path('/r/f_003.exr').with_number(999999999999)`, "/r/f_999999999999.exr"},
		{`path('/r/f_003.exr').with_number(-999999999999)`, "/r/f_-999999999999.exr"},
		{`path('/r/f_%d.exr').with_number(0)`, "/r/f_0.exr"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestWithNumber_ReturnTypes pins that the path row returns a path and the
// string row a string — the two rows exist precisely so a caller keeps the type
// it started with.
func TestWithNumber_ReturnTypes(t *testing.T) {
	p, err := Eval(`path('/r/s_003.exr').with_number(7)`, MapSymbols{}, TAny)
	if err != nil {
		t.Fatalf("path form failed: %v", err)
	}
	if got := p.Type.String(); got != "path" {
		t.Errorf("path form typed %s, want path", got)
	}
	s, err := Eval(`with_number('s_003.exr', 7)`, MapSymbols{}, TAny)
	if err != nil {
		t.Fatalf("string form failed: %v", err)
	}
	if got := s.Type.String(); got != "string" {
		t.Errorf("string form typed %s, want string", got)
	}
}

// TestWithNumber_PaddingCap checks the bound at the limit and one past it, not
// near it: a defect such as a float-to-int narrowing shows only at the bound.
func TestWithNumber_PaddingCap(t *testing.T) {
	wide32 := `path('/r/f_%032d.exr').with_number(1)`
	if _, err := Eval(wide32, MapSymbols{}, TAny); err != nil {
		t.Errorf("a padding width of 32 must be accepted: %v", err)
	}
	for _, src := range []string{
		`path('/r/f_%033d.exr').with_number(1)`,
		`path('/r/f_' + '#' * 33 + '.exr').with_number(1)`,
	} {
		_, err := Eval(src, MapSymbols{}, TAny)
		if err == nil {
			t.Fatalf("Eval(%q) succeeded; a padding width of 33 must be refused", src)
		}
		if !errors.Is(err, errPaddingTooWide) {
			t.Errorf("Eval(%q) = %v, want it to wrap errPaddingTooWide", src, err)
		}
	}
}

// TestWithNumber_PaddingCap_WidthOverflowsInt pins that a %0Nd width literal
// so large it overflows strconv.Atoi (as opposed to merely exceeding
// maxNumberPadding) surfaces as errPaddingTooWide, not a raw
// *strconv.NumError from printfReplacement. A width that doesn't fit in an int
// at all necessarily exceeds 32, so folding the overflow into the same
// sentinel is correct, not just convenient. The reference accepts this input
// with no padding at all (with_number("f_%0999999999999999999999d", 1) is
// "f_1" there) — a second reference defect against RFC 0006 line 831's
// "wider printf or hash patterns are an error" rule, in the same family as
// the hash-then-digit one in TestWithNumber_HashThenDigits. sqi erroring is
// correct.
func TestWithNumber_PaddingCap_WidthOverflowsInt(t *testing.T) {
	src := `path('/r/f_%0999999999999999999999d.exr').with_number(1)`
	_, err := Eval(src, MapSymbols{}, TAny)
	if err == nil {
		t.Fatalf("Eval(%q) succeeded; a width literal that overflows int must be refused", src)
	}
	if !errors.Is(err, errPaddingTooWide) {
		t.Errorf("Eval(%q) = %v, want it to wrap errPaddingTooWide, not leak strconv's raw error", src, err)
	}
}

// TestWithNumber_HashThenDigits pins that a hash run immediately followed by
// a digit run (or a non-digit, non-hash
// character) is two independent candidates, and withNumber replaces only the
// last one — never everything from the last match to the end of the stem.
//
// RFC 0006 line 822 says with_number "searches the filename stem from the
// end for these patterns and replaces the last match found" — a match, not a
// suffix of the stem starting at a match. The reference implementation
// disagrees: it lets a hash run swallow the rest of the
// stem, which for "##a3" destroys the literal "a" that was in the input
// (reference: "07"; sqi, matching the spec text: "##a7"). The reference is
// wrong here, sqi is not, so every "sqi" value below is pinned as correct, not
// as a known divergence to eventually match. Each row that disagrees with the
// reference is baselined in test/oracle/baseline.txt; the "reference"
// comments give its value.
func TestWithNumber_HashThenDigits(t *testing.T) {
	tests := []struct {
		src, want string
	}{
		// Hash run then digit run: the digit run is the later (rightmost)
		// candidate, so only it is replaced; the hash run is untouched
		// literal text, same as any other prefix.
		// reference: "007"
		{`with_number('###003', 7)`, "###007"},
		// reference: "007"
		{`with_number('#003', 7)`, "#007"},
		// reference: "007"
		{`with_number('##003', 7)`, "##007"},
		// reference: "007"
		{`with_number('####003', 7)`, "####007"},
		// Hash run, then a non-digit, non-hash character, then a digit run:
		// same rule — only the trailing digit run is the last candidate.
		// reference: "a007"
		{`with_number('a###5', 9)`, "a###9"},
		// Digit run, then hash run, then digit run: the last digit run is
		// the last candidate; the leading digit run and the hash run are
		// both untouched.
		// reference: "1207"
		{`with_number('12##34', 7)`, "12##07"},
		// Hash run immediately followed by a non-digit, non-hash character
		// with no further digit run after it: the hash run itself is then
		// the last (and only) candidate, same as "shot_####" with no
		// trailing digits.
		{`with_number('##a3', 7)`, "##a7"},
		{`with_number('##a', 7)`, "07a"},
		// Cases where sqi and the reference already agree, included so a
		// later change that "fixes" sqi toward the reference on the
		// disagreeing rows above fails loudly here too if it goes too far.
		{`with_number('##b##', 7)`, "##b07"},
		{`with_number('3###', 7)`, "3007"},
		{`with_number('#3#', 7)`, "#37"},
		{`with_number('%04d3', 7)`, "%04d7"},
		{`with_number('##%04d', 7)`, "##0007"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestWithNumber_ScanIsAllocationBounded pins that withNumber, which needs
// only the last candidate in the stem, does not materialize every candidate
// first (numberPattern.FindAllStringIndex(stem, -1)) to get it. The "-1" is
// unbounded, the rule funcsre.go's reSub states as "an unbounded -1 here
// would let a zero-width pattern enumerate maxStringBytes+1 matches before
// anything downstream could object".
//
// The invariant is "no candidate but the current best is still reachable when
// the last one is chosen", so the assertion is on live heap across the call,
// with the collector's pace forced for the duration so it reads retention
// rather than the pacer. Two cheaper metrics cannot separate the forms:
//
//   - allocation count: both forms enter the regexp engine once per
//     candidate, so their malloc counts are nearly equal and a
//     testing.AllocsPerRun assertion passes either way.
//
//   - cumulative bytes (MemStats.TotalAlloc): the race detector's per-call
//     instrumentation swamps the difference, and make test and make ci run
//     -race by default. Normalizing it against a one-candidate stem of the
//     same length does not help, because the per-call constant the ratio is
//     meant to cancel does not appear in a two-call baseline.
//
//   - live heap separates them once the collector is taken out of the
//     reading. Left to the default pacer it reads garbage accumulated since
//     whichever collection fired mid-call, which depends on GOGC, GOMEMLIMIT,
//     the live heap left by preceding tests and the pacer's state.
//     debug.SetGCPercent below overrides the environment for the measurement
//     window and restores it afterwards, so collection runs often enough that
//     transient garbage never accumulates and what remains is reachable. The
//     enumerating form's match list is reachable by construction and no pacer
//     setting can collect it.
//
// At the 400,000 candidates below, the scanning form retains under 2 MB in
// every build mode (plain, -race, -cover, and under hostile GOGC settings),
// and the enumerating form retains over 14 MB. The 6 MB bound sits between
// them, biased toward the passing side because a test that fails on correct
// code is worse than the gap it closes. The retained figure and the live heap
// it was taken against are logged on every run, so an engine or runtime change
// moving either side shows up before it becomes a flake or a false green.
func TestWithNumber_ScanIsAllocationBounded(t *testing.T) {
	// 400,000 candidates rather than 200,000. Raising the count does not
	// widen the ratio between the two forms, because under -race the
	// enumerating form's retained reading is itself affected by collection
	// timing; what the larger count buys is a bigger absolute gap, so the
	// bound can sit further from both sides in bytes.
	const pairs = 400_000
	// gcPercentDuringMeasurement is low enough that collection is frequent
	// relative to what one call allocates, so HeapAlloc tracks the live set.
	// It is not zero-cost — it is why the -race run takes a few seconds — and
	// it is the price of an assertion that does not depend on the pacer.
	const gcPercentDuringMeasurement = 10
	// maxRetainedBytes sits between the two forms' readings in the doc comment
	// above, and is stated absolutely rather than as a
	// multiple of the input because the two sides scale differently.
	const maxRetainedBytes = 6_000_000

	name := strings.Repeat("#a", pairs)

	// Correctness first: only the last candidate — the final "#", every other
	// one being separated from it by an "a" — is replaced.
	got, err := withNumber(name, 7)
	if err != nil {
		t.Fatalf("withNumber on a %d-candidate stem failed: %v", pairs, err)
	}
	if want := strings.Repeat("#a", pairs-1) + "7a"; got != want {
		t.Fatalf("withNumber replaced the wrong candidate: got ...%q, want ...%q",
			got[len(got)-8:], want[len(want)-8:])
	}

	defer debug.SetGCPercent(debug.SetGCPercent(gcPercentDuringMeasurement))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, err := withNumber(name, 7)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("withNumber failed: %v", err)
	}
	// The result must stay reachable across the reading, so that what is
	// measured is the scan's retention and not an artifact of the result
	// itself having become collectable early.
	runtime.KeepAlive(out)

	// Signed, because the scan's transient garbage can leave the heap smaller
	// than it started after a collection runs mid-call — a negative delta is a
	// pass, not an underflow to a huge unsigned number.
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("retained %d bytes of heap for a %d-byte stem with %d candidates (live heap before: %d, bound: %d)",
		retained, len(name), pairs, before.HeapAlloc, maxRetainedBytes)
	if retained > maxRetainedBytes {
		t.Errorf("withNumber retained %d bytes of heap across a %d-byte stem (%d candidates), bound %d — "+
			"the scan is materializing every candidate instead of keeping only the last",
			retained, len(name), pairs, maxRetainedBytes)
	}
}

// TestWithNumber_WindowsFlavor pins that the path row's reconstruction goes
// through withName under the Windows flavor too, same as every other with_*
// function. The oracle evaluates path functions under POSIX only, so it
// cannot adjudicate these; they are pinned by test alone.
func TestWithNumber_WindowsFlavor(t *testing.T) {
	tests := []struct{ src, want string }{
		{`path('C:/shot_003.exr').with_number(72)`, `C:\shot_072.exr`},
		{`path('C:/a.tar.gz').with_number(72)`, `C:\a.tar_0072.gz`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny, WithPathFormat(PathWindows))
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) under PathWindows = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}
