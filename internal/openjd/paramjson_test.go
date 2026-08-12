// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

func TestEncodeListDefault(t *testing.T) {
	tests := []struct {
		name string
		typ  JobParamType
		in   any
		want string
	}{
		{"strings", JobParamTypeListString, []any{"a", "b"}, `["a","b"]`},
		{"ints", JobParamTypeListInt, []any{1, 2}, `[1,2]`},
		{"floats", JobParamTypeListFloat, []any{1.0, 2.5}, `[1,2.5]`},
		{"bools", JobParamTypeListBool, []any{true, false}, `[true,false]`},
		{"paths", JobParamTypeListPath, []any{"/tmp/a", "/tmp/b"}, `["/tmp/a","/tmp/b"]`},
		{"empty", JobParamTypeListString, []any{}, `[]`},
		{"nested", JobParamTypeListListInt, []any{[]any{1, 2}, []any{3}}, `[[1,2],[3]]`},
		{"nested empty inner", JobParamTypeListListInt, []any{[]any{}}, `[[]]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodeListDefault(tt.in, tt.typ)
			if err != nil {
				t.Fatalf("encodeListDefault: %v", err)
			}
			if got != tt.want {
				t.Errorf("encodeListDefault = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestEncodeListDefault_Rejects pins the narrowness of the widening. The
// existing scalar reader rejects non-scalars because a coerced value "would
// otherwise be substituted verbatim into a running task's command, script, or
// environment" (scalarFieldStrict's own doc comment) — so list acceptance must
// admit sequences of scalars and nothing else, at every level.
func TestEncodeListDefault_Rejects(t *testing.T) {
	tests := []struct {
		name string
		typ  JobParamType
		in   any
	}{
		{"scalar for a list type", JobParamTypeListString, "a"},
		{"mapping as the default", JobParamTypeListString, map[string]any{"k": "v"}},
		{"mapping as an element", JobParamTypeListString, []any{map[string]any{"k": "v"}}},
		{"nested list for a flat type", JobParamTypeListInt, []any{[]any{1}}},
		{"mapping in an inner list", JobParamTypeListListInt, []any{[]any{map[string]any{"k": 1}}}},
		{"scalar in the outer list", JobParamTypeListListInt, []any{1}},
		{"triple nesting", JobParamTypeListListInt, []any{[]any{[]any{1}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := encodeListDefault(tt.in, tt.typ); err == nil {
				t.Fatalf("encodeListDefault(%v) accepted a value it must reject", tt.in)
			}
		})
	}
}

// TestEncodeListDefault_DoesNotEscapeHTML pins a deliberate departure from
// encoding/json's default. json.Marshal escapes the three characters <, > and
// & into their \u form so the output is safe to embed in HTML — irrelevant
// here, and it would mean a template author who writes ["a<b"] reads back an
// escaped form from the API in sub-project F2. Both are valid JSON and decode
// identically; the unescaped one is what a human recognizes as what they
// typed.
func TestEncodeListDefault_DoesNotEscapeHTML(t *testing.T) {
	got, err := encodeListDefault([]any{"a<b&c>d"}, JobParamTypeListString)
	if err != nil {
		t.Fatalf("encodeListDefault: %v", err)
	}
	if want := `["a<b&c>d"]`; got != want {
		t.Errorf("encodeListDefault = %s, want %s", got, want)
	}
}

// TestEncodeListDefault_NoTrailingNewline guards the seam the non-escaping
// decision introduced: json.Encoder (unlike json.Marshal) terminates every
// value with a newline, and a stored default carrying one would compare
// unequal to the same list written by hand.
func TestEncodeListDefault_NoTrailingNewline(t *testing.T) {
	got, err := encodeListDefault([]any{"a"}, JobParamTypeListString)
	if err != nil {
		t.Fatalf("encodeListDefault: %v", err)
	}
	if got != `["a"]` {
		t.Errorf("encodeListDefault = %q, want %q", got, `["a"]`)
	}
}

func TestDecodeListDefault_RoundTrips(t *testing.T) {
	in := []any{"a", "b"}
	s, err := encodeListDefault(in, JobParamTypeListString)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeListDefault(s, JobParamTypeListString)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 || out[0] != "a" || out[1] != "b" {
		t.Errorf("round trip = %v, want [a b]", out)
	}
}

// TestListDefault_ParsedFromTemplate drives the real decoder, which is where
// the type dispatch lives.
func TestListDefault_ParsedFromTemplate(t *testing.T) {
	p, err := decodeJobParameter(map[string]any{
		"name": "Cameras", "type": "list[string]",
		"default": []any{"main", "closeup"},
	}, true)
	if err != nil {
		t.Fatalf("decodeJobParameter: %v", err)
	}
	if p.Default == nil {
		t.Fatal("Default is nil; a list default must be stored as canonical JSON")
	}
	if *p.Default != `["main","closeup"]` {
		t.Errorf("Default = %s, want [\"main\",\"closeup\"]", *p.Default)
	}
}

// TestListDefault_ScalarTypeUnchanged pins that the widening is type-directed:
// a STRING parameter given a sequence must still be rejected by the existing
// scalar reader.
func TestListDefault_ScalarTypeUnchanged(t *testing.T) {
	_, err := decodeJobParameter(map[string]any{
		"name": "S", "type": "string",
		"default": []any{"a"},
	}, true)
	if err == nil {
		t.Fatal("a sequence default on a STRING parameter was accepted")
	}
}

// TestListDefault_ListTypeWithoutEXPRStillRejected pins that the gate holds at
// the parse layer too. Without EXPR the type stays the verbatim "LIST[STRING]",
// which isListParamType does not recognize, so the sequence default falls to
// the scalar reader and is rejected — the same outcome as before RFC 0007, by
// the same code path.
func TestListDefault_ListTypeWithoutEXPRStillRejected(t *testing.T) {
	_, err := decodeJobParameter(map[string]any{
		"name": "L", "type": "LIST[STRING]",
		"default": []any{"a"},
	}, false)
	if err == nil {
		t.Fatal("a list default was accepted without extensions: [EXPR]")
	}
}

// TestListDefault_JSONIsNotValueString pins the divergence paramjson.go's
// header describes, so it is a documented fact rather than a latent surprise:
// the STORED form is JSON, the INTERPOLATED form is expr.Value.String() (Go
// syntax, via strconv.Quote). They agree on ordinary text and separate on
// control characters — JSON requires a six-character \u0000 escape where Go
// writes \x00, and \x00 is not valid JSON at all.
//
// Neither is wrong. They are two renderings for two purposes, and sub-project
// F2 must not assume a value read from the store can be compared against an
// interpolated one as text.
func TestListDefault_JSONIsNotValueString(t *testing.T) {
	const nul = "a\x00b"

	stored, err := encodeListDefault([]any{nul}, JobParamTypeListString)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rendered := expr.List(expr.TString, []expr.Value{expr.String(nul)}).String()

	if stored == rendered {
		t.Fatalf("stored and interpolated renderings agree (%s); this test exists "+
			"because they do not, and something has changed", stored)
	}
	t.Logf("stored=%q interpolated=%q — two forms, two purposes", stored, rendered)

	// The stored form must be valid JSON that round-trips; that is the whole
	// point of choosing it for storage.
	out, err := decodeListDefault(stored, JobParamTypeListString)
	if err != nil {
		t.Fatalf("stored form does not round-trip: %v", err)
	}
	if len(out) != 1 || out[0] != nul {
		t.Errorf("round trip = %q, want %q", out, nul)
	}
}
