// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtstring

import "testing"

func TestSegments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Segment
	}{
		{"plain literal", "hello", []Segment{{Literal: "hello"}}},
		{"lone reference", "{{ Param.X }}", []Segment{{Ref: "Param.X", IsRef: true}}},
		{"embedded reference", "a {{ Param.X }} b", []Segment{
			{Literal: "a "}, {Ref: "Param.X", IsRef: true}, {Literal: " b"},
		}},
		{"two references", "{{A}}{{B}}", []Segment{
			{Ref: "A", IsRef: true}, {Ref: "B", IsRef: true},
		}},
		{"empty input", "", nil},
		// The body is NOT validated here: an expression is not a dotted
		// identifier, and judging it is the caller's job.
		{"expression body passes through", "{{ Param.X * 2 }}", []Segment{
			{Ref: "Param.X * 2", IsRef: true},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Segments(tc.in)
			if err != nil {
				t.Fatalf("Segments(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Segments(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Segments(%q)[%d] = %#v, want %#v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSegments_UnclosedIsAnError pins that the segmenter agrees with parse on
// the case the conformance harness's own extractor diverges on.
func TestSegments_UnclosedIsAnError(t *testing.T) {
	if _, err := Segments("a {{ Param.X"); err == nil {
		t.Fatal("an unclosed reference was accepted; want a MalformedError")
	}
}

func TestLoneRef(t *testing.T) {
	tests := []struct {
		in       string
		wantBody string
		wantOK   bool
	}{
		{"{{ Param.X }}", "Param.X", true},
		{"{{Param.X}}", "Param.X", true},
		{" {{ Param.X }}", "", false},
		{"{{ Param.X }} ", "", false},
		{"{{A}}{{B}}", "", false},
		{"plain", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			body, ok := LoneRef(tc.in)
			if ok != tc.wantOK || body != tc.wantBody {
				t.Errorf("LoneRef(%q) = (%q, %v), want (%q, %v)",
					tc.in, body, ok, tc.wantBody, tc.wantOK)
			}
		})
	}
}
