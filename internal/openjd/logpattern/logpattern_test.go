// SPDX-License-Identifier: AGPL-3.0-or-later

package logpattern

import (
	"errors"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
)

// progSize measures p the way Compile does.
func progSize(t *testing.T, p string) int {
	t.Helper()
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		t.Fatalf("parse %q: %v", p, err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		t.Fatalf("compile %q: %v", p, err)
	}
	return len(prog.Inst)
}

// hostilePattern is the review's reproduction: 905 bytes, under the 1024-byte
// pattern cap, compiling to ~226,000 instructions.
var hostilePattern = strings.Repeat(".{0,999}", 113) + "x"

func TestCompile(t *testing.T) {
	atLimit := "a{998}a{1000}"
	overLimit := "a{999}a{1000}"
	if got := progSize(t, atLimit); got != MaxProgramInsts {
		t.Fatalf("boundary pattern %q compiles to %d instructions, want exactly %d", atLimit, got, MaxProgramInsts)
	}
	if got := progSize(t, overLimit); got != MaxProgramInsts+1 {
		t.Fatalf("boundary pattern %q compiles to %d instructions, want exactly %d", overLimit, got, MaxProgramInsts+1)
	}

	tests := []struct {
		name      string
		pattern   string
		wantLarge bool
		wantErr   bool
	}{
		{name: "realistic failOn", pattern: `(?i)^\s*(error|fatal)\b.*(failed with|exception)`},
		{name: "realistic marker", pattern: `SQI_RENDER: FAILED:`},
		{name: "exactly at the limit", pattern: atLimit},
		{name: "one over the limit", pattern: overLimit, wantLarge: true, wantErr: true},
		{name: "concatenated bounded repeats", pattern: hostilePattern, wantLarge: true, wantErr: true},
		{name: "syntax error", pattern: "(unclosed", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			re, err := Compile(tc.pattern)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Compile(%q) error = %v, wantErr %v", tc.pattern, err, tc.wantErr)
			}
			var tooLarge *TooLargeError
			if errors.As(err, &tooLarge) != tc.wantLarge {
				t.Fatalf("Compile(%q) error = %v, want TooLargeError %v", tc.pattern, err, tc.wantLarge)
			}
			if tooLarge != nil && !strings.HasSuffix(err.Error(), "at most 2000 are allowed") {
				t.Errorf("error %q does not name the limit", err)
			}
			if err == nil && re == nil {
				t.Fatal("Compile returned neither a regexp nor an error")
			}
		})
	}
}

// TestCompile_SyntaxErrorMatchesRegexp pins that a syntax error reads exactly as
// regexp.Compile reports it, so the server's existing message is unchanged.
func TestCompile_SyntaxErrorMatchesRegexp(t *testing.T) {
	for _, p := range []string{"(unclosed", "a**", `\8`, "x{1001}"} {
		_, want := regexp.Compile(p)
		_, got := Compile(p)
		if want == nil || got == nil || got.Error() != want.Error() {
			t.Errorf("Compile(%q) error = %v, regexp.Compile error = %v", p, got, want)
		}
	}
}
