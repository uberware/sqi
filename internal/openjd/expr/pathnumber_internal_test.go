// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
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
