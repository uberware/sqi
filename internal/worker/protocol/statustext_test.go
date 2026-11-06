// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeStatusText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short unchanged", "frame 3 of 10", "frame 3 of 10"},
		{"unicode unchanged", "rendering é世界", "rendering é世界"},
		{"nul stripped", "ab\x00cd", "abcd"},
		{"whitespace controls become spaces", "a\tb\nc\rd", "a b c d"},
		{"other controls stripped", "a\x01b\x1bc\x7fd\u0085e", "abcde"},
		{"invalid utf8 dropped", "a\xffb\xc3", "ab"},
		{"surrounding space trimmed", "  \n x \t", "x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeStatusText(tc.in); got != tc.want {
				t.Errorf("SanitizeStatusText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeStatusText_Truncation(t *testing.T) {
	exact := strings.Repeat("a", MaxStatusTextBytes)
	if got := SanitizeStatusText(exact); got != exact {
		t.Errorf("a text of exactly the limit was changed (len %d)", len(got))
	}
	if got := SanitizeStatusText(exact + "b"); got != exact {
		t.Errorf("oversized ASCII: len = %d, want %d", len(got), MaxStatusTextBytes)
	}

	// A three-byte rune straddling the limit is dropped whole, never split.
	straddle := strings.Repeat("a", MaxStatusTextBytes-1) + "世"
	got := SanitizeStatusText(straddle)
	if !utf8.ValidString(got) || got != strings.Repeat("a", MaxStatusTextBytes-1) {
		t.Errorf("straddling rune: len = %d, valid = %v", len(got), utf8.ValidString(got))
	}

	// Controls are removed before the limit applies.
	padded := strings.Repeat("\x00", 5000) + "ok"
	if got := SanitizeStatusText(padded); got != "ok" {
		t.Errorf("controls counted toward the limit: %q", got)
	}
}
