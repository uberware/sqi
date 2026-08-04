// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestTrimAndAffix covers strip/lstrip/rstrip and removeprefix/removesuffix.
//
// The empty-argument rows are the point. RFC 0006 declares an empty argument an
// ERROR for count/find/rfind/index/rindex/replace/split/rsplit and says nothing
// of the kind for these five, so an empty chars, prefix or suffix here is a
// no-op rather than a rejection. Confirmed against the reference.
func TestTrimAndAffix(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"strip whitespace", `strip('  a  ')`, "a"},
		{"strip a tab and newline", `strip('\t a \n')`, "a"},
		{"strip nothing to do", `strip('a')`, "a"},
		{"strip an all-space string", `strip('   ')`, ""},
		{"lstrip whitespace", `lstrip('  a  ')`, "a  "},
		{"rstrip whitespace", `rstrip('  a  ')`, "  a"},
		{"strip a character set", `strip('xxayx', 'xy')`, "a"},
		{"lstrip a character set", `lstrip('xxayx', 'xy')`, "ayx"},
		{"rstrip a character set", `rstrip('xxayx', 'xy')`, "xxa"},
		{"strip an empty set is a no-op", `strip('abc', '')`, "abc"},
		{"rstrip an empty set is a no-op", `rstrip('abc', '')`, "abc"},
		{"removeprefix present", `removeprefix('abcdef', 'abc')`, "def"},
		{"removeprefix absent", `removeprefix('abcdef', 'z')`, "abcdef"},
		{"removeprefix empty is a no-op", `removeprefix('abc', '')`, "abc"},
		{"removesuffix present", `removesuffix('abcdef', 'def')`, "abc"},
		{"removesuffix absent", `removesuffix('abcdef', 'z')`, "abcdef"},
		{"removesuffix empty is a no-op", `removesuffix('abc', '')`, "abc"},
		{"method form", `'  a  '.strip()`, "a"},
		{"method form with an argument", `'xxayx'.strip('xy')`, "a"},
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
		})
	}
}
