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

// hostilePattern is 905 bytes, under the 1024-byte pattern cap, and compiles to
// ~226,000 instructions.
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

// TestEstimate_NeverOverstates asserts that the estimate never exceeds the real
// program size and falls short by at most the program's fixed overhead, so a
// pattern over EstimateCeiling is far over MaxProgramInsts and one within the
// limit is never refused unbuilt.
func TestEstimate_NeverOverstates(t *testing.T) {
	for _, p := range []string{
		`(?i)^\s*(error|fatal)\b.*(failed with|exception)`, `SQI_RENDER: FAILED:`, `ERROR +\|`,
		`^\[(\d+)/(\d+)\]`, `(a|b|c|d|e|f|g)+x*y?`, `[a-z]{3,7}`, `(?:abc){5,}`, `\bfoo\b`, `(?s).*`,
		`(?i)ÄÖÜ`, `\pL+`, `x{2,}`, `(?:)`, `^$`, `(?:){1000}`, `(?:a?){1000}`, `(a){1000}`,
		"a{998}a{1000}", "a{999}a{1000}", `.{0,500}foo.{0,500}`, strings.Repeat("(a|b)", 300),
		hostilePattern, strings.Repeat(".{999}", 170), strings.Repeat(`\pL{0,999}`, 102),
	} {
		est, err := Estimate(p)
		if err != nil {
			t.Fatalf("Estimate(%q): %v", p, err)
		}
		if got := progSize(t, p); est > got || got-est > 2 {
			t.Errorf("Estimate(%.40q) = %d, compiled size %d: want within 2 below it", p, est, got)
		}
	}
}

// TestCompile_RefusesUnbuiltOverTheCeiling asserts that a pattern whose
// estimate is over EstimateCeiling is refused on the estimate, without its
// program being built.
func TestCompile_RefusesUnbuiltOverTheCeiling(t *testing.T) {
	_, err := Compile(hostilePattern)
	var tooLarge *TooLargeError
	if !errors.As(err, &tooLarge) || !tooLarge.Estimated {
		t.Fatalf("Compile(hostile) error = %v, want an estimated TooLargeError", err)
	}
	if !strings.HasPrefix(err.Error(), "compiles to about ") {
		t.Errorf("error %q does not say the size is an estimate", err)
	}
	_, err = Compile("a{999}a{1000}")
	if !errors.As(err, &tooLarge) || tooLarge.Estimated || tooLarge.Insts != MaxProgramInsts+1 {
		t.Fatalf("Compile(one over) error = %v, want a measured TooLargeError of %d", err, MaxProgramInsts+1)
	}
}

// TestCompile_SyntaxErrorMatchesRegexp asserts that a syntax error reads exactly
// as regexp.Compile reports it.
func TestCompile_SyntaxErrorMatchesRegexp(t *testing.T) {
	for _, p := range []string{"(unclosed", "a**", `\8`, "x{1001}"} {
		_, want := regexp.Compile(p)
		_, got := Compile(p)
		if want == nil || got == nil || got.Error() != want.Error() {
			t.Errorf("Compile(%q) error = %v, regexp.Compile error = %v", p, got, want)
		}
	}
}
