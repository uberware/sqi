// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strings"
	"testing"
)

func TestParseLetBinding(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantName, wantExpr string
		wantErr                      string
	}{
		{name: "simple", in: "x = 1", wantName: "x", wantExpr: "1"},
		{name: "underscore start", in: "_x = 1", wantName: "_x", wantExpr: "1"},
		{name: "digits and case after first", in: "a1B_c = 2", wantName: "a1B_c", wantExpr: "2"},
		{name: "no spaces", in: "x=1", wantName: "x", wantExpr: "1"},
		{name: "tabs around equals", in: "x\t=\t1", wantName: "x", wantExpr: "1"},
		{name: "expression keeps its own equals", in: "x = a == b", wantName: "x", wantExpr: "a == b"},
		{name: "expression keeps inner spacing", in: "x = a + b", wantName: "x", wantExpr: "a + b"},

		{name: "missing equals", in: "x 1", wantErr: "must be"},
		{name: "uppercase first", in: "Foo = 1", wantErr: "must start with"},
		{name: "digit first", in: "1x = 1", wantErr: "must start with"},
		{name: "empty name", in: "= 1", wantErr: "must start with"},
		{name: "invalid character after first", in: "a-b = 1", wantErr: "must be a letter, digit, or underscore"},
		{name: "empty expression", in: "x = ", wantErr: "expression"},
		{name: "name too long", in: strings.Repeat("a", 513) + " = 1", wantErr: "512"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, src, err := parseLetBinding(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseLetBinding(%q) = (%q, %q, nil), want an error", tc.in, name, src)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLetBinding(%q): %v", tc.in, err)
			}
			if name != tc.wantName || src != tc.wantExpr {
				t.Errorf("parseLetBinding(%q) = (%q, %q), want (%q, %q)", tc.in, name, src, tc.wantName, tc.wantExpr)
			}
		})
	}
}

func TestParseLetBinding_NameLengthBoundary(t *testing.T) {
	// 512 is legal, 513 is not -- the boundary itself, not a value near it.
	if _, _, err := parseLetBinding(strings.Repeat("a", 512) + " = 1"); err != nil {
		t.Errorf("512-character name rejected: %v", err)
	}
	if _, _, err := parseLetBinding(strings.Repeat("a", 513) + " = 1"); err == nil {
		t.Error("513-character name accepted, want an error")
	}
}
