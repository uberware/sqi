// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"os/exec"
	"strings"
	"testing"
)

// TestParsePath_POSIX pins Python's PurePosixPath normalization. Every
// expectation was produced by running python3 during design.
//
// The rules are not uniform and that is the point: "//" collapses, "." is
// dropped, a trailing "/" is dropped, but ".." is KEPT — pathlib does not
// resolve it because doing so is wrong in the presence of symlinks.
func TestParsePath_POSIX(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		parts []string
	}{
		{"/a/b", "/a/b", []string{"/", "a", "b"}},
		{"a/b", "a/b", []string{"a", "b"}},
		{"a//b", "a/b", []string{"a", "b"}},
		{"a/./b", "a/b", []string{"a", "b"}},
		{"a/../b", "a/../b", []string{"a", "..", "b"}},
		{"/a/b/", "/a/b", []string{"/", "a", "b"}},
		{"a/b//", "a/b", []string{"a", "b"}},
		{"/", "/", []string{"/"}},
		{".", ".", []string{}},
		{"", ".", []string{}},
		{"a", "a", []string{"a"}},
		{"a/b/..", "a/b/..", []string{"a", "b", ".."}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			p := parsePath(tc.in, PathPOSIX)
			if got := p.String(); got != tc.want {
				t.Errorf("parsePath(%q).String() = %q, want %q", tc.in, got, tc.want)
			}
			got := p.parts()
			if len(got) != len(tc.parts) {
				t.Fatalf("parsePath(%q).parts() = %q, want %q", tc.in, got, tc.parts)
			}
			for i := range got {
				if got[i] != tc.parts[i] {
					t.Fatalf("parsePath(%q).parts() = %q, want %q", tc.in, got, tc.parts)
				}
			}
		})
	}
}

// TestParsePath_POSIXMatchesPython is the test that actually proves the
// flavor. The table above encodes what we believe; this compares against the
// real thing over a generated corpus, so a rule we got subtly wrong shows up
// even where we did not think to write a row.
func TestParsePath_POSIXMatchesPython(t *testing.T) {
	corpus := pathCorpusPOSIX()
	// The script does NOT skip blank input lines: strings.Join(corpus, "\n")
	// never appends a trailing separator, so split("\n") always yields exactly
	// len(corpus) elements — and the corpus legitimately contains one empty
	// string (to exercise "" -> "." against Python too). A defensive
	// "if not line: continue" would silently swallow that one real input and
	// desync every line thereafter, which is a bug in the harness, not in the
	// path engine: it produces the same off-by-one for any implementation.
	script := `
import sys
from pathlib import PurePosixPath as P
for line in sys.stdin.read().split("\n"):
    p = P(line)
    print(str(p) + "\t" + "\x1f".join(p.parts))
`
	out := runPython(t, script, strings.Join(corpus, "\n"))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(corpus) {
		t.Fatalf("python returned %d lines for %d inputs", len(lines), len(corpus))
	}
	for i, in := range corpus {
		fields := strings.SplitN(lines[i], "\t", 2)
		wantStr := fields[0]
		var wantParts []string
		if fields[1] != "" {
			wantParts = strings.Split(fields[1], "\x1f")
		}
		p := parsePath(in, PathPOSIX)
		if got := p.String(); got != wantStr {
			t.Errorf("parsePath(%q).String() = %q, python %q", in, got, wantStr)
		}
		got := p.parts()
		if strings.Join(got, "\x1f") != strings.Join(wantParts, "\x1f") {
			t.Errorf("parsePath(%q).parts() = %q, python %q", in, got, wantParts)
		}
	}
}

// pathCorpusPOSIX generates the inputs both the table and the differential run
// over: roots, separators, dot segments and trailing slashes in combination.
func pathCorpusPOSIX() []string {
	segs := []string{"a", "b", ".", "..", "", "x.txt", ".hidden", "a.tar.gz"}
	var out []string
	for _, lead := range []string{"", "/"} {
		for _, s1 := range segs {
			out = append(out, lead+s1)
			for _, s2 := range segs {
				out = append(out, lead+s1+"/"+s2)
				out = append(out, lead+s1+"//"+s2)
				out = append(out, lead+s1+"/"+s2+"/")
			}
		}
	}
	return out
}

// runPython runs a script with stdin and returns stdout, skipping the test when
// python3 is unavailable. A SKIP here verifies nothing, so it says so loudly.
func runPython(t *testing.T, script, stdin string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable — this differential verifies NOTHING when skipped")
	}
	cmd := exec.CommandContext(t.Context(), "python3", "-c", script)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python3 failed: %v", err)
	}
	return string(out)
}
