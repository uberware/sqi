// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

// TestTranslatePattern_Rejects covers every construct RFC 0006 excludes.
//
// Two groups are represented. The first is the specification's own explicit
// "Not supported" list. The second is derived from its stated RULE — that the
// syntax is the INTERSECTION of Python's re and Rust's regex — which the
// reference implementation does not enforce: it accepts \p{...}, (?<n>...) and
// [[:alpha:]], all of which Python's re either rejects outright or reads as
// something different. See the design doc for that adjudication.
func TestTranslatePattern_Rejects(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    string // substring the error must name
	}{
		{"empty pattern", ``, "must not be empty"},
		{"lower z anchor", `llo\z`, `\z`},
		{"upper Z anchor", `llo\Z`, `\Z`},
		{"hex brace escape", `\x{1F600}`, `\x{`},
		{"unicode brace escape", `\u{41}`, `\u{`},
		{"upper unicode brace escape", `\U{41}`, `\U{`},
		{"lookahead", `foo(?=bar)`, "lookahead"},
		{"negative lookahead", `foo(?!bar)`, "lookahead"},
		{"lookbehind", `(?<=foo)bar`, "lookbehind"},
		{"negative lookbehind", `(?<!foo)bar`, "lookbehind"},
		{"backreference", `(a)\1`, "backreference"},
		{"named backreference", `(?P<n>a)(?P=n)`, "backreference"},
		{"conditional", `(?(1)a|b)`, "conditional"},
		{"unicode property", `\p{Nd}`, `\p{`},
		{"negated unicode property", `\P{Nd}`, `\P{`},
		{"rust named group", `(?<n>a)`, "named group"},
		{"posix class", `[[:alpha:]]`, "POSIX"},
		{"negated class with W", `[^\Wa]`, `\W`},
		{"negated class with S", `[^\Sa]`, `\S`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := translatePattern(tc.pattern)
			if err == nil {
				t.Fatalf("translatePattern(%q) succeeded; want a rejection", tc.pattern)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("translatePattern(%q) = %q, want it to name %q", tc.pattern, err, tc.want)
			}
		})
	}
}

// TestTranslatePattern_EmptyUsesItsOwnSentinel keeps the empty-pattern case
// distinguishable from every other rejection: four conformance fixtures test it
// separately from the unsupported-feature fixtures.
func TestTranslatePattern_EmptyUsesItsOwnSentinel(t *testing.T) {
	_, err := translatePattern("")
	if !errors.Is(err, errEmptyPattern) {
		t.Fatalf("translatePattern(\"\") = %v, want it to wrap errEmptyPattern", err)
	}
	_, err = translatePattern(`\z`)
	if errors.Is(err, errEmptyPattern) {
		t.Errorf(`translatePattern("\\z") wrapped errEmptyPattern; want errUnsupportedRegex`)
	}
}

// TestTranslatePattern_AcceptsSupported is the other half of the rejection
// tests: the constructs RFC 0006 lists as supported must survive translation
// and compile. A scanner that rejects too much fails here rather than silently
// refusing valid templates.
func TestTranslatePattern_AcceptsSupported(t *testing.T) {
	for _, p := range []string{
		`[abc]`, `[^abc]`, `[a-z]`, `^abc$`, `\bword\b`,
		`a*`, `a+`, `a?`, `a{2}`, `a{2,5}`, `a+?`, `a*?`,
		`(ab)`, `(?:ab)`, `a|b`, `(?i)abc`, `(?P<name>a)`,
		`\xE9`, `\t`, `\n`, `\.`, `\\`, `\[`,
	} {
		t.Run(p, func(t *testing.T) {
			if _, err := translatePattern(p); err != nil {
				t.Errorf("translatePattern(%q) rejected a supported construct: %v", p, err)
			}
		})
	}
}
