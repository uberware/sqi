// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"regexp"
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

// TestTranslatePattern_UnicodeShorthands pins the rewrite of Go's ASCII-only
// Perl classes into Unicode ones.
//
// The in-class column is the reason this is a scanner. Outside a class \w
// becomes a bracketed class; INSIDE one it must become the bracket's CONTENTS,
// because Go reads a nested "[" as a literal "[" — "[[\p{L}]]" compiles and
// matches the wrong thing rather than erroring.
func TestTranslatePattern_UnicodeShorthands(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{"d outside", `\d`, `\p{Nd}`},
		{"D outside", `\D`, `\P{Nd}`},
		{"d inside", `[\dx]`, `[\p{Nd}x]`},
		{"D inside", `[\Dx]`, `[\P{Nd}x]`},
		{"w outside", `\w`, `[\p{L}\p{N}_]`},
		{"w inside", `[\wx]`, `[\p{L}\p{N}_x]`},
		{"W outside", `\W`, `[^\p{L}\p{N}_]`},
		{"s outside", `\s`, `[` + unicodeSpaceSet + `]`},
		{"s inside", `[\sx]`, `[` + unicodeSpaceSet + `x]`},
		{"S outside", `\S`, `[^` + unicodeSpaceSet + `]`},
		{"escaped backslash then d is literal", `\\d`, `\\d`},
		{"escaped backslash then w is literal", `\\w`, `\\w`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := translatePattern(tc.pattern)
			if err != nil {
				t.Fatalf("translatePattern(%q) failed: %v", tc.pattern, err)
			}
			if got != tc.want {
				t.Errorf("translatePattern(%q) = %q, want %q", tc.pattern, got, tc.want)
			}
		})
	}
}

// TestTranslatePattern_ShorthandsMatchUnicode is the test that actually
// matters: the rewrite must change WHICH STRINGS MATCH, not merely produce
// plausible text. Go's own \d does not match an Arabic-Indic digit; the
// specification says it must.
func TestTranslatePattern_ShorthandsMatchUnicode(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    bool
	}{
		{`^\d$`, "٣", true},
		{`^\d$`, "3", true},
		{`^\d$`, "a", false},
		{`^\D$`, "٣", false},
		{`^\w$`, "é", true},
		{`^\w$`, "_", true},
		{`^\w$`, "-", false},
		{`^\W$`, "-", true},
		{`^\W$`, "é", false},
		{`^\s$`, " ", true},
		{`^\s$`, "　", true},
		{`^\s$`, "\u200b", false},
		{`^\S$`, "a", true},
		{`^[\dx]$`, "٣", true},
		{`^[\dx]$`, "x", true},
	}
	for _, tc := range tests {
		t.Run(tc.pattern+"_on_"+tc.input, func(t *testing.T) {
			translated, err := translatePattern(tc.pattern)
			if err != nil {
				t.Fatalf("translatePattern(%q) failed: %v", tc.pattern, err)
			}
			re, err := regexp.Compile(translated)
			if err != nil {
				t.Fatalf("translated %q to %q, which does not compile: %v", tc.pattern, translated, err)
			}
			if got := re.MatchString(tc.input); got != tc.want {
				t.Errorf("%q (translated %q) on %q = %v, want %v", tc.pattern, translated, tc.input, got, tc.want)
			}
		})
	}
}

// TestUnicodeSpaceSet_MatchesWhiteSpace pins the expansion against Unicode's
// White_Space property, one codepoint at a time.
//
// \p{White_Space} is NOT usable: Go's regexp supports Unicode categories and
// scripts only, not properties, and rejects it — measured during design. So the
// set is written out, and this test is what stops it drifting.
func TestUnicodeSpaceSet_MatchesWhiteSpace(t *testing.T) {
	re := regexp.MustCompile(`^[` + unicodeSpaceSet + `]$`)
	for _, r := range []rune{
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x20, 0x85, 0xA0,
		0x1680, 0x2000, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000,
	} {
		if !re.MatchString(string(r)) {
			t.Errorf("unicodeSpaceSet misses U+%04X, which has White_Space=yes", r)
		}
	}
	for _, r := range []rune{'a', '3', 0x200B} {
		if re.MatchString(string(r)) {
			t.Errorf("unicodeSpaceSet wrongly matches U+%04X", r)
		}
	}
}
