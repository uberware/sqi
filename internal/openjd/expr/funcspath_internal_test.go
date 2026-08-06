// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestPathConstructor(t *testing.T) {
	tests := []struct{ src, want, wantType string }{
		{`path('/a/b')`, "/a/b", "path"},
		{`path('a//b')`, "a/b", "path"},
		{`path('s3://bucket/a//b')`, "s3://bucket/a//b", "path"},
		{`path(['/', 'a', 'b'])`, "/a/b", "path"},
		{`path(['a', 'b'])`, "a/b", "path"},
		{`path([])`, ".", "path"},
		{`as_posix(path('/a/b'))`, "/a/b", "string"},
		{`is_absolute(path('/a/b'))`, "true", "bool"},
		{`is_absolute(path('a/b'))`, "false", "bool"},
		{`is_absolute(path('s3://b/x'))`, "true", "bool"},
		{`path('/a/b').as_posix()`, "/a/b", "string"},
		{`path('/a/b').is_absolute()`, "true", "bool"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
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

// TestPathConstructor_HonoursTheOption is the only place the path_format option
// is observable end to end, and therefore the only thing that proves the
// plumbing from Task 1 actually reaches a function.
func TestPathConstructor_HonoursTheOption(t *testing.T) {
	v, err := Eval(`path('C:/a/b')`, MapSymbols{}, TAny, WithPathFormat(PathWindows))
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != `C:\a\b` {
		t.Errorf("under PathWindows, path('C:/a/b') = %q, want %q", got, `C:\a\b`)
	}
	d, err := Eval(`path('C:/a/b')`, MapSymbols{}, TAny)
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := d.String(); got != "C:/a/b" {
		t.Errorf("under the POSIX default, path('C:/a/b') = %q, want %q", got, "C:/a/b")
	}
}

// TestAsPosix_ConvertsWindowsSeparators is as_posix's whole reason to exist.
func TestAsPosix_ConvertsWindowsSeparators(t *testing.T) {
	v, err := Eval(`path('C:/renders/project').as_posix()`, MapSymbols{}, TAny, WithPathFormat(PathWindows))
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if got := v.String(); got != "C:/renders/project" {
		t.Errorf("as_posix = %q, want %q", got, "C:/renders/project")
	}
}
