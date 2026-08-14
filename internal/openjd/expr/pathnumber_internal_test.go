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
// expectation was produced by running the reference implementation during
// design, and every one matched the specification's own table.
//
// The rule that makes this non-obvious: the scan runs from the END of the STEM
// and replaces the LAST match. That is what preserves a shot number —
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
		// one that starts LATER wins, regardless of which pattern kind
		// either one is.
		{`path('/out/f_%d_abc_###.exr').with_number(42)`, "/out/f_%d_abc_042.exr"},
		{`path('/out/file_%04d_003.exr').with_number(72)`, "/out/file_%04d_072.exr"},
		// A name that is entirely dots has no suffix at all (splitLeadingDots),
		// so the whole thing is the stem, and no digit/hash/printf pattern is
		// in it — the no-pattern fallback applies. Measured against the
		// reference during design.
		{`path('/r/...').with_number(3)`, "/r/..._0003"},
		// A DIGIT RUN INSIDE THE SUFFIX is not a candidate, because the scan
		// runs over the STEM alone — the case pathnumber.go's own doc comment
		// names ("file.v2.003.exr") and the one shape the table had no row
		// for until the final fix wave. The .exr row alone does not pin it:
		// scanning the whole NAME instead of the stem gives the identical
		// answer there, since ".exr" holds no digits at all. The .mp4 row is
		// the one that separates the two readings — scanning the name would
		// take the "4" as the last candidate and answer "render.0001.mp72",
		// destroying the container extension. Both values measured against the
		// reference at openjd-model 0.11.1, which agrees on both.
		{`with_number('file.v2.003.exr', 7)`, "file.v2.007.exr"},
		{`path('/r/render.0001.mp4').with_number(72)`, "/r/render.0072.mp4"},
		// Several suffixes: splitStemSuffix cuts at the LAST dot only, so
		// "a.tar.gz" has stem "a.tar" (no digit pattern in it) and suffix
		// ".gz" — the no-pattern fallback applies to the stem, not the whole
		// name. Measured against the reference during design.
		{`path('/r/a.tar.gz').with_number(72)`, "/r/a.tar_0072.gz"},
		// Overflow, zero and a large negative all pad through the same
		// zfillString formula with no special-casing. Measured against the
		// reference during design.
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

// TestWithNumber_PaddingCap checks the bound AT the limit and one past it, not
// near it. An earlier wave's float-narrowing defect was found only that way.
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

// TestWithNumber_PaddingCap_WidthOverflowsInt pins fix round 1's Important
// finding: a %0Nd width literal so large it overflows strconv.Atoi (as
// opposed to merely exceeding maxNumberPadding) must still surface as
// errPaddingTooWide, not the raw *strconv.NumError the earlier version of
// printfReplacement returned unwrapped. A width that doesn't fit in an int at
// all necessarily exceeds 32, so folding the overflow into the same sentinel
// is correct, not just convenient. The reference silently ACCEPTS this input
// with no padding at all (with_number("f_%0999999999999999999999d", 1) is
// "f_1" there) — a second reference defect against RFC 0006 line 831's
// "wider printf or hash patterns are an error" rule, in the same family as
// the hash-then-digit one in TestWithNumber_HashThenDigits. sqi erroring is
// correct; only the error's shape was wrong before this fix.
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

// TestWithNumber_HashThenDigits pins fix round 1's refuted Critical: a hash
// run immediately followed by a digit run (or a non-digit, non-hash
// character) is TWO independent candidates, and withNumber replaces only the
// LAST one — never everything from the last match to the end of the stem.
//
// RFC 0006 line 822 says with_number "searches the filename stem from the
// end for these patterns and replaces the last match found" — a match, not a
// suffix of the stem starting at a match. The reference implementation
// (openjd-model 0.11.1) disagrees: it lets a hash run swallow the rest of the
// stem, which for "##a3" DESTROYS the literal "a" that was in the input
// (reference: "07"; sqi, matching the spec text: "##a7"). This was reviewed
// and adjudicated in sqi's favor — the reference is wrong here, sqi is not —
// so every "sqi" value below is pinned as CORRECT, not as a known divergence
// to eventually match. Task 11's oracle baseline needs an entry for each row
// that disagrees with the reference; see the "reference" column in the
// comments and this task's report for the exact values to record.
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
		// Digit run, then hash run, then digit run: the LAST digit run is
		// the last candidate; the leading digit run and the hash run are
		// both untouched.
		// reference: "1207"
		{`with_number('12##34', 7)`, "12##07"},
		// Hash run immediately followed by a non-digit, non-hash character
		// with NO further digit run after it: the hash run itself is then
		// the last (and only) candidate, same as "shot_####" with no
		// trailing digits.
		{`with_number('##a3', 7)`, "##a7"},
		{`with_number('##a', 7)`, "07a"},
		// Cases where sqi and the reference already AGREE, included so a
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

// TestWithNumber_ScanIsAllocationBounded pins the final fix wave's Important
// finding: withNumber needs only the LAST candidate in the stem, and the
// original implementation materialized EVERY candidate first
// (numberPattern.FindAllStringIndex(stem, -1)) to get it. The "-1" is
// unbounded, and funcsre.go's reSub already wrote the rule down one wave
// earlier — "an unbounded -1 here would let a zero-width pattern enumerate
// maxStringBytes+1 matches before anything downstream could object" — which
// this file did not carry across. Measured before the fix:
// with_number('#a' * 5000000, 7), every operand inside maxStringBytes,
// allocated 794 MB to produce a 10 MB result.
//
// WHAT IS ASSERTED, AND WHY IT IS THE ONLY THING THAT CAN BE. The invariant is
// "no candidate but the current best is still reachable when the last one is
// chosen", so the assertion is on LIVE HEAP across the call — with the
// collector's pace forced for the duration, which is what makes it a
// measurement of retention rather than of the pacer. Two cheaper-looking
// metrics were measured and rejected, each for a concrete reason:
//
//   - ALLOCATION COUNT is blind to the defect. Both forms enter the regexp
//     engine once per candidate, so their malloc counts land within 0.02% of
//     each other (1,000,192 against 1,000,045 for a million candidates). A
//     testing.AllocsPerRun assertion passes either way.
//
//   - CUMULATIVE BYTES (MemStats.TotalAlloc) separates them only in a plain
//     build — 160 MB against 16 MB — and NOT under the race detector, whose
//     per-call instrumentation swamps the difference (1.71 GB against 1.63 GB).
//     make test and make ci run -race by default. NORMALIZING IT AS A RATIO
//     AGAINST A ONE-CANDIDATE STEM OF THE SAME LENGTH DOES NOT RESCUE IT, and
//     that was measured rather than assumed: under -race the ratios come out
//     47,000 for the scan against 43,000 for the enumerating form, i.e.
//     INVERTED as well as indistinguishable, because the per-call constant the
//     ratio is meant to cancel does not appear in a two-call baseline at all.
//
//   - LIVE HEAP does separate them, but only once the collector is taken out of
//     the measurement. Left to the default pacer it measures garbage
//     accumulated since whichever collection happened to fire mid-call, which
//     is a function of GOGC, GOMEMLIMIT, the live heap left by preceding tests
//     and the pacer's own state — under -race that put a correct
//     implementation at 76% of an earlier version of this bound, and GOGC=off
//     failed outright, because the fixed form still allocates ~377 MB
//     cumulatively for this input under -race even though it retains almost
//     none of it. debug.SetGCPercent below overrides the environment for the
//     measurement window and restores it afterwards, so collection runs often
//     enough that transient garbage never accumulates and what remains is what
//     is genuinely REACHABLE. The enumerating form's match list is reachable by
//     construction and no pacer setting can collect it.
//
// MEASURED SPREAD at the 400,000 candidates below, on the machine this was
// written on. FIXED, 90 runs: 25 isolated under -race (max 1,956,192), 20 plain
// (max 1,125,296), 20 under -cover (max 1,173,456), 10 whole-package runs in
// the make ci shape, -race -cover, so preceding tests' live heap is present
// (max 1,810,496), and 5 each under GOGC=off, GOGC=400 and GOGC=1000 to show
// the forced pace neutralizes a hostile environment (maxima 1,806,064,
// 1,738,448, 1,475,336). Worst case over all 90: 1.96 MB. BROKEN, 32 runs: 8
// each under plain (min 32,577,304), -race (min 14,501,312), -cover (min
// 32,577,288) and -race GOGC=off (min 14,245,016); it went RED in every one of
// those modes and in the whole-package make ci shape. Best case over all 32:
// 14.2 MB.
//
// The bound sits between them at 6 MB — 3.07x of room on the passing side,
// 2.37x on the failing side, deliberately biased toward the passing side
// because a test that fails on correct code is worse than the gap it closes.
// The measured value and the live heap it was taken against are logged on every
// run, so a future engine or runtime change moving either side shows up before
// it becomes a flake or a false green.
func TestWithNumber_ScanIsAllocationBounded(t *testing.T) {
	// 400,000 candidates rather than the 200,000 first tried. Raising the count
	// does NOT widen the RATIO, and saying so matters because the opposite is
	// the intuitive guess: measured separation is 7.3x here against 7.7x at
	// 200,000, because under -race the enumerating form's retained reading is
	// itself affected by collection timing and grew only 18% when the candidate
	// count doubled. What the larger count buys is a bigger ABSOLUTE gap
	// (12.3 MB against 10.7 MB) so the bound can sit further from both sides in
	// bytes, at a cost of about one extra second under -race.
	const pairs = 400_000
	// gcPercentDuringMeasurement is low enough that collection is frequent
	// relative to what one call allocates, so HeapAlloc tracks the live set.
	// It is not zero-cost — it is why the -race run takes a few seconds — and
	// it is the price of an assertion that does not depend on the pacer.
	const gcPercentDuringMeasurement = 10
	// maxRetainedBytes is chosen from the measurements in the doc comment
	// above, not by guessing, and is stated absolutely rather than as a
	// multiple of the input because the two sides scale differently.
	const maxRetainedBytes = 6_000_000

	name := strings.Repeat("#a", pairs)

	// Correctness first: only the LAST candidate — the final "#", every other
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
	// measured is the SCAN's retention and not an artifact of the result
	// itself having become collectable early.
	runtime.KeepAlive(out)

	// Signed, because the scan's transient garbage can leave the heap SMALLER
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
// function since Task 7. The reference is POSIX-only for path functions (see
// this package's other Windows-flavor tests, and CLAUDE.md's note on the
// oracle), so it cannot adjudicate these — they are pinned by test alone, not
// measured against the reference.
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
