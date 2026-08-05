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
		{"posix class, digit name", `[[:digit:]]`, "POSIX"},
		{"posix class, negated form", `[^[:alpha:]]`, "POSIX"},
		{"posix class, not the first member", `[a[:alpha:]]`, "POSIX"},
		{"negated class with W", `[^\Wa]`, `\W`},
		{"negated class with S", `[^\Sa]`, `\S`},
		{"unclosed class with content", `[abc`, "never closed"},
		{"unclosed class, nothing after the bracket", `[`, "never closed"},
		{"unclosed class holding only the literal-] convention", `[]`, "never closed"},
		{"unclosed negated class holding only the literal-] convention", `[^]`, "never closed"},
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
		{"w outside", `\w`, `[\p{L}_\p{N}]`},
		{"w inside", `[\wx]`, `[\p{L}_\p{N}x]`},
		{"W outside", `\W`, `[^\p{L}_\p{N}]`},
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

// TestTranslatePattern_UnicodeEscapes covers the escapes RFC 0006 REQUIRES but
// Go cannot parse. Both Python and Rust accept \uHHHH and \UHHHHHHHH; Go's
// regexp has no \u escape at all and rejects them — measured during design.
//
// Note the asymmetry with the rejection tests: \u{41} is REFUSED (Rust-only
// brace syntax) while \u0041 is TRANSLATED, and the translation emits the
// very \x{...} spelling the input is not allowed to use. That is deliberate:
// the rejection governs what a template may contain, the emission governs
// what Go is handed.
func TestTranslatePattern_UnicodeEscapes(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{"four-digit", `\u0041`, `\x{0041}`},
		{"eight-digit", `\U0001F600`, `\x{0001F600}`},
		{"inside a class", `[\u0041x]`, `[\x{0041}x]`},
		{"two-digit hex is left alone", `\xE9`, `\xE9`},
		{"escaped backslash then u is literal", `\\u0041`, `\\u0041`},
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

func TestTranslatePattern_UnicodeEscapesMatch(t *testing.T) {
	for _, tc := range []struct{ pattern, input string }{
		{`^\u0041$`, "A"},
		{`^\U0001F600$`, "\U0001F600"},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			translated, err := translatePattern(tc.pattern)
			if err != nil {
				t.Fatalf("translatePattern(%q) failed: %v", tc.pattern, err)
			}
			re, err := regexp.Compile(translated)
			if err != nil {
				t.Fatalf("translated %q to %q, which does not compile: %v", tc.pattern, translated, err)
			}
			if !re.MatchString(tc.input) {
				t.Errorf("%q (translated %q) did not match %q", tc.pattern, translated, tc.input)
			}
		})
	}
}

func TestTranslatePattern_RejectsMalformedUnicodeEscape(t *testing.T) {
	for _, p := range []string{`\u00`, `\U0001`, `\uZZZZ`} {
		t.Run(p, func(t *testing.T) {
			if _, err := translatePattern(p); err == nil {
				t.Errorf("translatePattern(%q) accepted a malformed escape", p)
			}
		})
	}
}

// TestTranslatePattern_NegatedShorthandInPositiveClass covers the one place the
// scanner rewrites an entire enclosing construct.
//
// "[\Wa]" means "not a word character, or the letter a". Go's engine cannot
// build a class that UNIONS a negated set with more members, so the class
// becomes an alternation instead. That is sound here because each alternative
// matches exactly one character, so there is no leftmost-match ambiguity, and
// "(?:...)" is an atom just as a class is, so a following quantifier still
// binds to the whole thing. Alternation takes any number of branches, so a
// class may hold BOTH \W and \S — one branch per distinct shorthand present,
// plus one more for any ordinary members left over.
//
// The negated form "[^\Wa]" is a different problem — word characters MINUS a,
// i.e. set subtraction — and is rejected directly by classNeedsAlternation.
func TestTranslatePattern_NegatedShorthandInPositiveClass(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		input   string
		want    bool
	}{
		{"W class matches non-word", `^[\Wa]$`, "!", true},
		{"W class matches the extra member", `^[\Wa]$`, "a", true},
		{"W class rejects other word chars", `^[\Wa]$`, "b", false},
		{"W class rejects digits", `^[\Wa]$`, "3", false},
		{"quantifier still binds", `^[\Wa]+$`, "!a!a", true},
		{"quantifier rejects a non-member", `^[\Wa]+$`, "!ab", false},
		{"S class matches non-space", `^[\Sx]$`, "q", true},
		{"S class matches the extra member", `^[\Sx]$`, "x", true},
		{"S class rejects a space", `^[\Sx]$`, " ", false},
		// \W and \S together: every character is either not-a-word-char or
		// not-a-space-char (no character is BOTH a word char and a space
		// char), so the union covers everything — a degenerate but correct
		// result, and the case the class used to be refused for outright.
		{"W and S together matches a word char, via the S branch", `^[\W\S]$`, "a", true},
		{"W and S together matches a space char, via the W branch", `^[\W\S]$`, " ", true},
		{"W and S together matches ordinary punctuation", `^[\W\S]$`, "!", true},
		// A third, ordinary member alongside two shorthands: doesn't change
		// what matches here (\W|\S already covers everything), but proves the
		// extra branch is wired in — TestTranslatePattern_AlternationText pins
		// that it is actually present in the output.
		{"W, S and an ordinary member together", `^[\W\Sx]$`, "x", true},
		// Order of appearance in the source reverses the branch order in the
		// translation (pinned by TestTranslatePattern_AlternationText); match
		// behavior is unaffected either way.
		{"S before W still matches both directions", `^[\S\W]$`, "a", true},
		// A remainder starting with "]" via the leading-]-is-literal
		// convention, alongside a shorthand.
		{"leading ] literal plus W matches the bracket", `^[]\W]$`, "]", true},
		{"leading ] literal plus W matches non-word", `^[]\W]$`, "!", true},
		{"leading ] literal plus W rejects a word char", `^[]\W]$`, "a", false},
		// The same shape spelled with an escaped "\]" instead of the
		// leading-] convention — same meaning, different source syntax.
		{"escaped ] plus W matches the bracket", `^[\W\]]$`, "]", true},
		{"escaped ] plus W rejects a word char", `^[\W\]]$`, "a", false},
		// A remainder containing a TRANSLATED escape (\d -> \p{Nd}), not a
		// bare literal — exercises classBodyWithout's call into scanEscape.
		{"W plus d matches a digit via the remainder", `^[\W\d]$`, "3", true},
		{"W plus d matches non-word via the W branch", `^[\W\d]$`, "!", true},
		{"W plus d rejects a non-digit word char", `^[\W\d]$`, "a", false},
		// The critical case: a literal "^" that was NOT first in the source
		// can be promoted to first position once the shorthand ahead of it is
		// dropped. Escaped, it stays literal; unescaped, Go would read it as
		// the class's negation marker and invert the whole match.
		{"caret after a dropped W stays literal, matches the caret", `^[\W^a]$`, "^", true},
		{"caret after a dropped W stays literal, matches the other member", `^[\W^a]$`, "a", true},
		{"caret after a dropped W stays literal, rejects an unlisted word char", `^[\W^a]$`, "b", false},
		{"caret after a dropped W stays literal, rejects a digit", `^[\W^a]$`, "3", false},
		{"caret after a dropped S stays literal, matches the caret", `^[\S^a]$`, "^", true},
		{"caret after a dropped S stays literal, rejects a space", `^[\S^a]$`, " ", false},
		// The degenerate case: dropping \W leaves ONLY a caret behind. Before
		// this fix the remainder was the bare, uncompilable "[^]"; now it is
		// the one-member literal class "[\^]".
		{"W with nothing but a caret left behind matches the caret", `^[\W^]$`, "^", true},
		{"W with nothing but a caret left behind rejects a word char", `^[\W^]$`, "a", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

// TestTranslatePattern_AlternationText pins the exact translated TEXT of
// every alternation-producing class, not just its match behavior: a wrong
// bracket placement can compile and even match every probe character in
// TestTranslatePattern_NegatedShorthandInPositiveClass correctly by
// coincidence (as "[\W^a]" mistranslated to "(?:[^\p{L}_\p{N}]|[^a])" did —
// it only diverges from the correct translation on inputs neither test
// suite happened to probe until this one was written), while still being
// the wrong regex. Every string here was verified independently: compiled
// with regexp.Compile and checked against the pattern's OWN meaning across a
// probe set, not merely copied from whatever this package currently emits.
func TestTranslatePattern_AlternationText(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{"single W plus a member", `[\Wa]`, `(?:[^\p{L}_\p{N}]|[a])`},
		{"single S plus a member", `[\Sx]`, `(?:[^` + unicodeSpaceSet + `]|[x])`},
		{"W and S together, nothing left over", `[\W\S]`, `(?:[^\p{L}_\p{N}]|[^` + unicodeSpaceSet + `])`},
		{"W and S together plus an ordinary member", `[\W\Sx]`, `(?:[^\p{L}_\p{N}]|[^` + unicodeSpaceSet + `]|[x])`},
		{"S before W: branch order follows source order", `[\S\W]`, `(?:[^` + unicodeSpaceSet + `]|[^\p{L}_\p{N}])`},
		{"leading ] literal plus W", `[]\W]`, `(?:[^\p{L}_\p{N}]|[]])`},
		{"escaped ] plus W", `[\W\]]`, `(?:[^\p{L}_\p{N}]|[\]])`},
		{"W plus a translated escape in the remainder", `[\W\d]`, `(?:[^\p{L}_\p{N}]|[\p{Nd}])`},
		{"caret after a dropped W is escaped, not left as the negation marker", `[\W^a]`, `(?:[^\p{L}_\p{N}]|[\^a])`},
		{"caret after a dropped S is escaped", `[\S^a]`, `(?:[^` + unicodeSpaceSet + `]|[\^a])`},
		{"a lone caret left behind is escaped rather than emitting the uncompilable []", `[\W^]`, `(?:[^\p{L}_\p{N}]|[\^])`},
		// A shorthand repeated with itself: classNeedsAlternation collapses
		// every occurrence of a given shorthand to the SAME one branch (see
		// its own doc), so nothing is left over and the alternation holds a
		// single branch. This guards a live panic: if classBodyWithout ever
		// dropped only the FIRST occurrence instead of every one, the second
		// \W/\S would reach writeSetEscape as class CONTENTS with negate
		// true, which panics by construction (writeSetEscape's own doc) —
		// and nothing else in the suite would go red first.
		{"W repeated with itself collapses to one branch", `[\W\W]`, `(?:[^\p{L}_\p{N}])`},
		{"S repeated with itself collapses to one branch", `[\S\S]`, `(?:[^` + unicodeSpaceSet + `])`},
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
			if _, err := regexp.Compile(got); err != nil {
				t.Errorf("translatePattern(%q) = %q, which does not compile: %v", tc.pattern, got, err)
			}
		})
	}
}

// TestTranslatePattern_WordShorthandAdjacentDash covers the code-review
// finding that \w's expansion, unlike \d, \D and \s, used to END IN A BARE
// LITERAL character ("_"), so a naive translation let an adjacent "-" in the
// SOURCE form an unintended character RANGE with it: "[\w-a]" used to
// translate its set into "[\p{L}\p{N}_-a]", where Go reads "_-a" as the
// range U+005F..U+0061 — matching the backtick, which is neither a word
// character nor "a" under any reading of the source. Verified directly
// against Python's re: it and the reference both REJECT "[\w-a]" outright
// ("bad character range \w-a"; the reference: "regex parse error").
//
// The fix (unicodeWordSet's own doc) moves the underscore into the MIDDLE of
// the set, between the two \p{...} property escapes, so it can never be the
// first or last character spliced in and therefore can never directly abut
// whatever SOURCE text sits next to the \w escape, on either side — checked
// here in both directions, not only the trailing one the finding named.
func TestTranslatePattern_WordShorthandAdjacentDash(t *testing.T) {
	t.Run("translated text", func(t *testing.T) {
		tests := []struct {
			name    string
			pattern string
			want    string
		}{
			{"dash then a", `[\w-a]`, `[\p{L}_\p{N}-a]`},
			{"trailing dash stays literal by position alone", `[\w-]`, `[\p{L}_\p{N}-]`},
			{"escaped dash then a", `[\w\-a]`, `[\p{L}_\p{N}\-a]`},
			{"ordinary range after \\w is unaffected", `[\wa-c]`, `[\p{L}_\p{N}a-c]`},
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
	})

	t.Run("match behavior", func(t *testing.T) {
		tests := []struct {
			pattern string
			input   string
			want    bool
		}{
			// The critical case: "[\w-a]" must NOT match the backtick.
			// U+0060 is neither a word character nor "a" under any reading
			// of the source, but the pre-fix translation matched it via the
			// accidental "_-a" range.
			{`^[\w-a]$`, "`", false},
			{`^[\w-a]$`, "a", true},
			{`^[\w-a]$`, "-", true},
			{`^[\w-a]$`, "_", true},
			{`^[\w-]$`, "-", true},
			{`^[\w-]$`, "`", false},
			{`^[\w\-a]$`, "-", true},
			{`^[\w\-a]$`, "`", false},
			{`^[\wa-c]$`, "b", true},
			{`^[\wa-c]$`, "-", false},
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
	})

	// "[a-\w]" is the mirror direction: a literal immediately before \w.
	// Both Python and the reference reject it outright. sqi's scanner does
	// not special-case this — it still fails, but via Go's own compile-time
	// rejection of "-\p{...}" as an invalid class range starter, reached
	// through compilePattern rather than translatePattern's own scanner. The
	// sandwiched set order (unicodeWordSet's own doc) keeps this failing
	// safe rather than silently matching wrong, the same as the trailing
	// direction above — verified directly, not merely assumed from the
	// trailing case.
	t.Run("literal before backslash-w still fails safe, not silently wrong", func(t *testing.T) {
		if _, err := compilePattern(`[a-\w]`); err == nil {
			t.Fatal(`compilePattern("[a-\\w]") succeeded; want an error`)
		}
	})
}

// TestTranslatePattern_AlternationAdjacentDash covers the code-review finding
// that dropping a lifted \W/\S shorthand SPLICES its two former neighbors
// together, and if one of them is a literal "-" the splice can form a
// character RANGE the source never wrote: "[a\W-c]" used to translate to
// "(?:[^\p{L}_\p{N}]|[a-c])", which Go reads as the range a..c and silently
// MATCHES "b" — outside every reading of the source, since the "-" sat
// between "\W" and "c" in the original text, never between "a" and "c".
// Verified directly: both Python's re ("bad character range \W-c") and the
// reference ("regex parse error") refuse to parse "[a\W-c]" and "[0\W-9]"
// outright, so there is no reading of the source under which "b" or "5" is a
// member.
//
// The fix (classBodyWithout's own doc, via dashesAdjacentToDroppedShorthand)
// escapes any "-" that sat immediately next to a dropped occurrence, so the
// remainder reads as sqi's own permissive union — {not-a-word-char} ∪ {a} ∪
// {-} ∪ {c} for "[a\W-c]" — rather than a spliced range.
func TestTranslatePattern_AlternationAdjacentDash(t *testing.T) {
	t.Run("translated text", func(t *testing.T) {
		tests := []struct {
			name    string
			pattern string
			want    string
		}{
			{"dash between W and a following member", `[a\W-c]`, `(?:[^\p{L}_\p{N}]|[a\-c])`},
			{"dash between W and a following digit", `[0\W-9]`, `(?:[^\p{L}_\p{N}]|[0\-9])`},
			{"trailing dash after W alone stays literal by position", `[\W-]`, `(?:[^\p{L}_\p{N}]|[\-])`},
			{"trailing dash after W plus a member stays literal by position", `[a\W-]`, `(?:[^\p{L}_\p{N}]|[a\-])`},
			{"no dash at all is unaffected", `[a\Wc]`, `(?:[^\p{L}_\p{N}]|[ac])`},
			{"dash between a member and S", `[a\S-c]`, `(?:[^` + unicodeSpaceSet + `]|[a\-c])`},
			{"dash before the dropped shorthand, mirror direction", `[a-\Wc]`, `(?:[^\p{L}_\p{N}]|[a\-c])`},
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
				if _, err := regexp.Compile(got); err != nil {
					t.Errorf("translatePattern(%q) = %q, which does not compile: %v", tc.pattern, got, err)
				}
			})
		}
	})

	t.Run("match behavior", func(t *testing.T) {
		tests := []struct {
			pattern string
			input   string
			want    bool
		}{
			// The critical cases: neither pattern may match a character that
			// only exists because "a"/"0" and "c"/"9" got spliced into a range.
			{`^[a\W-c]$`, "b", false},
			{`^[a\W-c]$`, "a", true},
			{`^[a\W-c]$`, "c", true},
			{`^[a\W-c]$`, "-", true},
			{`^[a\W-c]$`, "!", true},  // not-a-word-char branch
			{`^[a\W-c]$`, "d", false}, // ordinary word char, not in {a,-,c}, not non-word
			{`^[0\W-9]$`, "5", false},
			{`^[0\W-9]$`, "0", true},
			{`^[0\W-9]$`, "9", true},
			{`^[0\W-9]$`, "-", true},
			// Trailing dash: must keep matching the literal "-".
			{`^[\W-]$`, "-", true},
			{`^[\W-]$`, "!", true},
			{`^[\W-]$`, "a", false},
			{`^[a\W-]$`, "-", true},
			{`^[a\W-]$`, "a", true},
			{`^[a\W-]$`, "!", true},
			{`^[a\W-]$`, "b", false},
			// No dash at all: unaffected by the fix.
			{`^[a\Wc]$`, "c", true},
			{`^[a\Wc]$`, "a", true},
			{`^[a\Wc]$`, "!", true},
			{`^[a\Wc]$`, "b", false},
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
	})
}

// TestTranslatePattern_NestedClassIsNeverEmitted pins the failure this whole
// scanner exists to prevent. Go compiles "[[\p{L}]]" WITHOUT ERROR and matches
// something other than intended, so a translation that emitted a nested class
// would ship a silently wrong matcher. No output may contain "[[".
func TestTranslatePattern_NestedClassIsNeverEmitted(t *testing.T) {
	for _, p := range []string{
		`[\wx]`, `[\sx]`, `[\dx]`, `[\Wa]`, `[\Sx]`, `[\w\s\d]`,
		`[\W\S]`, `[\W\Sx]`, `[\W^a]`, `[\W^]`, `[]\W]`, `[\W\]]`,
	} {
		t.Run(p, func(t *testing.T) {
			got, err := translatePattern(p)
			if err != nil {
				t.Fatalf("translatePattern(%q) failed: %v", p, err)
			}
			if strings.Contains(got, "[[") {
				t.Errorf("translatePattern(%q) = %q, which nests a character class", p, got)
			}
		})
	}
}
