// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "testing"

func TestParseEnvDirective(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantKind EnvOpKind
		wantName string
		wantVal  string
		wantRed  bool
	}{
		{
			name:     "valid set",
			line:     "openjd_env: FOO=bar",
			wantOK:   true,
			wantKind: EnvOpSet,
			wantName: "FOO",
			wantVal:  "bar",
		},
		{
			name:     "valid redacted set",
			line:     "openjd_redacted_env: SECRET=shh",
			wantOK:   true,
			wantKind: EnvOpSet,
			wantName: "SECRET",
			wantVal:  "shh",
			wantRed:  true,
		},
		{
			name:     "valid unset",
			line:     "openjd_unset_env: FOO",
			wantOK:   true,
			wantKind: EnvOpUnset,
			wantName: "FOO",
		},
		{
			name:     "value contains equals (split on first)",
			line:     "openjd_env: URL=a=b=c",
			wantOK:   true,
			wantKind: EnvOpSet,
			wantName: "URL",
			wantVal:  "a=b=c",
		},
		{
			name:     "leading and trailing whitespace",
			line:     "  openjd_env:  FOO=bar  ",
			wantOK:   true,
			wantKind: EnvOpSet,
			wantName: "FOO",
			wantVal:  "bar",
		},
		{
			name:     "underscore-prefixed name is valid",
			line:     "openjd_env: _PRIVATE=1",
			wantOK:   true,
			wantKind: EnvOpSet,
			wantName: "_PRIVATE",
			wantVal:  "1",
		},
		{
			name:   "name starting with digit is invalid",
			line:   "openjd_env: 1FOO=bar",
			wantOK: false,
		},
		{
			name:   "name with dash is invalid",
			line:   "openjd_env: FO-O=bar",
			wantOK: false,
		},
		{
			name:   "empty name is invalid",
			line:   "openjd_env: =bar",
			wantOK: false,
		},
		{
			name:   "set without equals is invalid",
			line:   "openjd_env: FOO",
			wantOK: false,
		},
		{
			name:   "unset with invalid name",
			line:   "openjd_unset_env: 1BAD",
			wantOK: false,
		},
		{
			name:   "unset with empty name",
			line:   "openjd_unset_env: ",
			wantOK: false,
		},
		{
			name:   "non-directive line",
			line:   "hello world",
			wantOK: false,
		},
		{
			name:   "unrecognized openjd prefix",
			line:   "openjd_progress: 50",
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, ok := ParseEnvDirective(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ParseEnvDirective(%q) ok = %v; want %v", tc.line, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if op.Kind != tc.wantKind {
				t.Errorf("Kind = %v; want %v", op.Kind, tc.wantKind)
			}
			if op.Name != tc.wantName {
				t.Errorf("Name = %q; want %q", op.Name, tc.wantName)
			}
			if op.Value != tc.wantVal {
				t.Errorf("Value = %q; want %q", op.Value, tc.wantVal)
			}
			if op.Redacted != tc.wantRed {
				t.Errorf("Redacted = %v; want %v", op.Redacted, tc.wantRed)
			}
		})
	}
}
