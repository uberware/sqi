// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"testing"
)

// TestPaddingFunctions covers ljust, rjust, center and zfill.
//
// Two rows are easy to get wrong in opposite directions:
//   - center splits the padding by FLOOR division and puts the remainder on the
//     right, so center('ab',7) is '  ab   '. CPython answers '   ab  ' — it
//     biases the split with marg & width & 1 — and the reference does not.
//     Do not "correct" this toward Python; it would manufacture an oracle
//     divergence out of nothing.
//   - a width at or below the current length returns the input UNCHANGED, which
//     is what makes a negative width a harmless no-op. The reference panics on
//     a negative width instead.
func TestPaddingFunctions(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"ljust pads on the right", `ljust('ab', 5)`, "ab   "},
		{"rjust pads on the left", `rjust('ab', 5)`, "   ab"},
		{"center splits by floor, remainder right", `center('ab', 7)`, "  ab   "},
		{"center with one to spare", `center('abc', 4)`, "abc "},
		{"center counts codepoints", `center('é', 5)`, "  é  "},
		{"ljust below the width is unchanged", `ljust('abcd', 2)`, "abcd"},
		{"ljust at the width is unchanged", `ljust('ab', 2)`, "ab"},
		{"a negative width is a no-op", `ljust('ab', -3)`, "ab"},
		{"a negative width is a no-op for center", `center('ab', -3)`, "ab"},
		{"a negative width is a no-op for zfill", `zfill('ab', -3)`, "ab"},
		{"zfill a string", `zfill('42', 5)`, "00042"},
		{"zfill counts codepoints", `zfill('é', 3)`, "00é"},
		{"zfill preserves a minus", `zfill('-10', 4)`, "-010"},
		{"zfill preserves a plus", `zfill('+7', 5)`, "+0007"},
		{"zfill a lone sign", `zfill('-', 4)`, "-000"},
		{"zfill an empty string", `zfill('', 3)`, "000"},
		{"zfill an int", `zfill(42, 5)`, "00042"},
		{"zfill a negative int", `zfill(-1, 3)`, "-01"},
		{"zfill a float", `zfill(1.5, 6)`, "0001.5"},
		{"zfill a negative float", `zfill(-1.5, 7)`, "-0001.5"},
		{"zfill an integral float keeps its point", `zfill(2.0, 6)`, "0002.0"},
		{"method form on a string", `'42'.zfill(5)`, "00042"},
		{"method form on a parenthesized int", `(42).zfill(5)`, "00042"},
		{"the RFC's own worked example", `'frame_'.rjust(10) + zfill(1, 4)`, "    frame_0001"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.src, got, tc.want)
			}
			if got := v.Type.String(); got != "string" {
				t.Errorf("Eval(%q) typed %s, want string", tc.src, got)
			}
		})
	}
}

// TestPadding_BoundsWidth checks the bound AT the limit and one past it, and at
// an int64 extreme where naive arithmetic overflows rather than refusing.
//
// The reference implementation fails all three: it PANICS with "capacity
// overflow" on a negative zfill width and "Formatting argument out of range" on
// a large ljust width, and builds a 20 MB string for zfill('a', 20000000)
// without complaint.
func TestPadding_BoundsWidth(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr bool
		// checkInt, when true, checks wantInt against the SUCCESSFUL result's
		// value — not merely that Eval returned no error. Without this, "at
		// the limit" could not catch a regression in padWidth that reported
		// no-padding-needed at the boundary: ljust would then silently
		// return "a" instead of padding to 10000000, and an error-only
		// assertion would still pass.
		checkInt bool
		wantInt  int64
	}{
		{"at the limit", `len(ljust('a', 10000000))`, false, true, 10_000_000},
		{"one past the limit", `ljust('a', 10000001)`, true, false, 0},
		{"a width at the int64 ceiling", `ljust('a', 9223372036854775807)`, true, false, 0},
		{"zfill past the limit", `zfill('a', 10000001)`, true, false, 0},
		{"center past the limit", `center('a', 10000001)`, true, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Eval(tc.src, MapSymbols{}, TAny)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Eval(%q) succeeded; want it refused as too large", tc.src)
			case tc.wantErr && !errors.Is(err, errTooLarge):
				t.Errorf("Eval(%q) = %v, want it to wrap errTooLarge", tc.src, err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Eval(%q) failed: %v", tc.src, err)
			case !tc.wantErr && tc.checkInt && v.AsInt() != tc.wantInt:
				t.Errorf("Eval(%q) = %d, want %d", tc.src, v.AsInt(), tc.wantInt)
			}
		})
	}
}

// TestZfill_UsesTheSameFloatRenderingAsString pins that zfill does not invent a
// second float rendering. formatFloat (value.go) is the package's only one, so
// zfill(x, w) and string(x) must agree on every float digit-for-digit.
func TestZfill_UsesTheSameFloatRenderingAsString(t *testing.T) {
	for _, src := range []string{"1.5", "-1.5", "2.0", "0.00001 * 1.0", "round(3.5, 2)"} {
		t.Run(src, func(t *testing.T) {
			want, err := Eval("string("+src+")", MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("string(%s) failed: %v", src, err)
			}
			got, err := Eval("zfill("+src+", 0)", MapSymbols{}, TAny)
			if err != nil {
				t.Fatalf("zfill(%s, 0) failed: %v", src, err)
			}
			if got.String() != want.String() {
				t.Errorf("zfill(%s, 0) = %q, string(%s) = %q — the two renderings must agree",
					src, got.String(), src, want.String())
			}
		})
	}
}
