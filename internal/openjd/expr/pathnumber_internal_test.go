// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"runtime"
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
// THE ASSERTION IS ON LIVE HEAP ACROSS THE CALL, not on the allocation COUNT,
// not on cumulative bytes, and not by exhausting memory in CI. Each candidate
// was measured before the threshold was chosen:
//
//   - Allocation COUNT cannot see the defect at all. Both forms call into the
//     regexp engine once per candidate, so their malloc counts land within
//     0.02% of each other (1,000,192 against 1,000,045 for a million
//     candidates) and a testing.AllocsPerRun assertion would pass either way.
//   - CUMULATIVE bytes (MemStats.TotalAlloc) separates them cleanly in a plain
//     build — 160 MB against 16 MB — but NOT under -race, where the detector's
//     own per-call instrumentation dominates both (1.71 GB against 1.63 GB, a
//     4% gap). make test runs the race detector by default, so a TotalAlloc
//     bound tuned on a plain build fails there for a reason that has nothing
//     to do with this function. That is not hypothetical: it is what the first
//     version of this test did.
//   - LIVE HEAP measured immediately after the call, before any collection can
//     sweep the result, is stable across every build mode — 100.8 MB against
//     4.2 MB with -race, 97.2 MB against 4.4 MB without — because it measures
//     what the two forms genuinely differ in: whether every candidate is still
//     reachable at the moment the last one is chosen. It is also the quantity
//     the 794 MB figure above actually names.
//
// The forward scan keeps ONE pair of indices live at a time, so its peak is
// the input plus the result rather than a multiple of the candidate count.
// Measured for the 200,000 candidates below, plain / -race / -cover: 0.51 MB,
// 1.89 MB, 0.43 MB retained here against 16.5 MB, 21.9 MB, 16.5 MB with
// FindAllStringIndex. The 12x-of-input bound sits between them with at least
// 2.5x of room on the passing side and 3.4x on the failing side in every mode,
// which is why it is stated as a multiple of the input rather than an absolute
// byte count. The value is logged on every run so a future engine change moving
// either side is visible before it becomes a false green.
func TestWithNumber_ScanIsAllocationBounded(t *testing.T) {
	const pairs = 200_000
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
	t.Logf("retained %d bytes of heap for a %d-byte stem with %d candidates", retained, len(name), pairs)
	if limit := int64(12 * len(name)); retained > limit {
		t.Errorf("withNumber retained %d bytes of heap across a %d-byte stem (%d candidates), limit %d — "+
			"the scan is materializing every candidate instead of keeping only the last",
			retained, len(name), pairs, limit)
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
