// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"testing"
)

// TestRegexFunctions covers RFC 0006's six regex functions. Every expected
// value was produced by running the reference implementation during design.
//
// The re_findall rows encode its most surprising rule: the result SHAPE
// depends on the pattern's group count. Zero groups yields the full matches,
// ONE group yields that group's captures instead of the full matches, and two
// or more yields a list per match.
func TestRegexFunctions(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		want     string
		wantType string
	}{
		{"re_search finds anywhere", `re_search('asset_v042_final.abc', '_v(\d+)')`, "[_v042, 042]", "list[string]"},
		{"re_search with no groups", `re_search('hello123', '\d+')`, "[123]", "list[string]"},
		{"re_search misses", `re_search('abc', 'z')`, "null", "nulltype"},
		{"re_match anchors at the start", `re_match('v042_final', 'v(\d+)')`, "[v042, 042]", "list[string]"},
		{"re_match refuses a later match", `re_match('asset_v042', 'v(\d+)')`, "null", "nulltype"},
		{"re_findall no groups gives full matches", `re_findall('shot010_shot020', 'shot\d+')`, "[shot010, shot020]", "list[string]"},
		{"re_findall one group gives that group", `re_findall('shot010_shot020', 'shot(\d+)')`, "[010, 020]", "list[string]"},
		{"re_findall two groups gives lists", `re_findall('v1.2 and v4.5', 'v(\d+)\.(\d+)')`, "[[1, 2], [4, 5]]", "list[list[string]]"},
		{"re_findall misses", `re_findall('abc', '\d')`, "[]", "list[string]"},
		{"re_sub replaces every match", `re_sub('frame_001', '\d+', '002')`, "frame_002", "string"},
		{"re_sub with no match", `re_sub('abc', '\d', 'x')`, "abc", "string"},
		{"re_escape quotes metacharacters", `re_escape('file[1].txt')`, `file\[1\]\.txt`, "string"},
		{"re_split on a pattern", `re_split('a1b2c', '\d')`, "[a, b, c]", "list[string]"},
		{"re_split with maxsplit", `re_split('a1b2c', '\d', 1)`, "[a, b2c]", "list[string]"},
		{"unicode digit class", `re_search('٣', '\d')`, "[٣]", "list[string]"},
		{"method form", `'hello123'.re_search('\d+')`, "[123]", "list[string]"},
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
			if got := v.Type.String(); got != tc.wantType {
				t.Errorf("Eval(%q) typed %s, want %s", tc.src, got, tc.wantType)
			}
		})
	}
}

// TestRegexFunctions_RejectPatterns confirms the scanner's refusals actually
// reach a caller, rather than the functions compiling the raw pattern with Go
// and getting Go's own answer.
func TestRegexFunctions_RejectPatterns(t *testing.T) {
	tests := []struct {
		src     string
		wantErr error
	}{
		{`re_search('abc', '')`, errEmptyPattern},
		{`re_split('abc', '')`, errEmptyPattern},
		{`re_split('abc', '', 2)`, errEmptyPattern},
		{`re_sub('abc', '', 'x')`, errEmptyPattern},
		{`re_search('abc', r'llo\z')`, errUnsupportedRegex},
		{`re_search('abc', r'foo(?=bar)')`, errUnsupportedRegex},
		{`re_search('abc', r'(?<=foo)bar')`, errUnsupportedRegex},
		{`re_search('abc', r'(a)\1')`, errUnsupportedRegex},
		{`re_search('abc', r'(?P<n>a)(?P=n)')`, errUnsupportedRegex},
		{`re_search('abc', r'\p{Nd}')`, errUnsupportedRegex},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := Eval(tc.src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want %v", tc.src, tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Eval(%q) = %v, want it to wrap %v", tc.src, err, tc.wantErr)
			}
		})
	}
}

// TestReSub_RejectsGroupReferences pins RFC 0006's rule that re_sub's
// replacement is LITERAL text and every group-reference spelling is an error.
//
// The raw-string prefix is load-bearing: sqi's lexer preserves '\1' as two
// characters, so a non-raw literal would still reach re_sub, but writing it raw
// makes the intent unambiguous and matches how the oracle corpus must spell it.
func TestReSub_RejectsGroupReferences(t *testing.T) {
	for _, src := range []string{
		`re_sub('frame_001', '\d+', r'\1')`,
		`re_sub('frame_001', '\d+', r'\g<1>')`,
		`re_sub('frame_001', '\d+', '$1')`,
		`re_sub('frame_001', '\d+', '${1}')`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := Eval(src, MapSymbols{}, TAny)
			if err == nil {
				t.Fatalf("Eval(%q) succeeded; want a group-reference rejection", src)
			}
			if !errors.Is(err, errGroupReference) {
				t.Errorf("Eval(%q) = %v, want it to wrap errGroupReference", src, err)
			}
		})
	}
}

// TestReSub_DollarIsLiteralOtherwise is the other half: Go's own
// ReplaceAllString would expand "$name", so re_sub must use the LITERAL
// variant. A dollar that is not a group reference has to survive.
func TestReSub_DollarIsLiteralOtherwise(t *testing.T) {
	v, err := Eval(`re_sub('a1b', '\d', '$')`, MapSymbols{}, TAny)
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "a$b" {
		t.Errorf(`re_sub('a1b', '\d', '$') = %q, want "a$b"`, got)
	}
}
