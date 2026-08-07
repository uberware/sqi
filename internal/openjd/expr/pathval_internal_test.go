// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestParsePath_POSIX pins Python's PurePosixPath normalization. Every
// expectation was produced by running python3 during design.
//
// The rules are not uniform and that is the point: "//" collapses, "." is
// dropped, a trailing "/" is dropped, but ".." is KEPT — pathlib does not
// resolve it because doing so is wrong in the presence of symlinks.
//
// THE abs COLUMN IS NOT DECORATION, and its absence is the exact asymmetry that
// hid a bug for three tasks: TestParsePath_Windows below has always asserted
// is_absolute and this table did not, so the POSIX "//" root — POSIX.1-2017
// section 4.13's own, which CPython keeps rather than collapsing — reported
// itself RELATIVE with nothing to notice. The differential below gained the
// same column at the same time, and pathCorpusPOSIX gained the "//", "///" and
// "////" leads that make the shape reachable on purpose rather than by
// accident.
func TestParsePath_POSIX(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		parts []string
		abs   bool
	}{
		{"/a/b", "/a/b", []string{"/", "a", "b"}, true},
		{"a/b", "a/b", []string{"a", "b"}, false},
		{"a//b", "a/b", []string{"a", "b"}, false},
		{"a/./b", "a/b", []string{"a", "b"}, false},
		{"a/../b", "a/../b", []string{"a", "..", "b"}, false},
		{"/a/b/", "/a/b", []string{"/", "a", "b"}, true},
		{"a/b//", "a/b", []string{"a", "b"}, false},
		{"/", "/", []string{"/"}, true},
		{".", ".", []string{}, false},
		{"", ".", []string{}, false},
		{"a", "a", []string{"a"}, false},
		{"a/b/..", "a/b/..", []string{"a", "b", ".."}, false},
		// The three anchor shapes the table never named. Exactly two leading
		// slashes is its OWN root and is preserved; three or more collapse to
		// one; all of them are absolute.
		{"//a/b", "//a/b", []string{"//", "a", "b"}, true},
		{"///a/b", "/a/b", []string{"/", "a", "b"}, true},
		{"////a/b", "/a/b", []string{"/", "a", "b"}, true},
		{"//", "//", []string{"//"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			p := parsePath(tc.in, PathPOSIX)
			if got := p.String(); got != tc.want {
				t.Errorf("parsePath(%q).String() = %q, want %q", tc.in, got, tc.want)
			}
			if got := p.isAbsolute(); got != tc.abs {
				t.Errorf("parsePath(%q).isAbsolute() = %v, want %v", tc.in, got, tc.abs)
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
	// is_absolute is compared as well as str and parts, and its ABSENCE here
	// is what let the POSIX "//" root report itself relative for three tasks
	// (isAbsolute tested the parsed root against "/" while its doc comment
	// claimed pathlib's raw startswith("/") rule, so the exactly-two-slashes
	// root fell through). The Windows differential below always compared it;
	// this one did not, and the corpus has contained "//b" — via an empty
	// first segment — the whole time.
	script := `
import sys
from pathlib import PurePosixPath as P
for line in sys.stdin.read().split("\n"):
    p = P(line)
    print(str(p) + "\t" + "\x1f".join(p.parts) + "\t" + str(p.is_absolute()))
`
	out := runPython(t, script, strings.Join(corpus, "\n"))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(corpus) {
		t.Fatalf("python returned %d lines for %d inputs", len(lines), len(corpus))
	}
	for i, in := range corpus {
		fields := strings.SplitN(lines[i], "\t", 3)
		wantStr := fields[0]
		var wantParts []string
		if fields[1] != "" {
			wantParts = strings.Split(fields[1], "\x1f")
		}
		wantAbs := fields[2] == "True"
		p := parsePath(in, PathPOSIX)
		if got := p.String(); got != wantStr {
			t.Errorf("parsePath(%q).String() = %q, python %q", in, got, wantStr)
		}
		got := p.parts()
		if strings.Join(got, "\x1f") != strings.Join(wantParts, "\x1f") {
			t.Errorf("parsePath(%q).parts() = %q, python %q", in, got, wantParts)
		}
		if gotAbs := p.isAbsolute(); gotAbs != wantAbs {
			t.Errorf("parsePath(%q).isAbsolute() = %v, python %v", in, gotAbs, wantAbs)
		}
	}
}

// TestParsePath_POSIXDoubleSlashIsAbsolute pins the POSIX.1-2017 section 4.13
// root — exactly two leading slashes, which CPython keeps as its own "//" root
// rather than collapsing — as ABSOLUTE.
//
// Every one of these reported itself relative until fix round 1: isAbsolute's
// POSIX branch compared the parsed root against "/" exactly, so the one root
// shape that is neither "" nor "/" fell through to false. CPython
// (PurePosixPath("//a").is_absolute()) and the reference implementation
// (is_absolute(path("//a")), probed at openjd-model 0.11.1) both answer true.
// The negatives are here for the same reason the positives are: a predicate
// that answered true for everything would satisfy the list above on its own.
func TestParsePath_POSIXDoubleSlashIsAbsolute(t *testing.T) {
	absolute := []string{
		"//", "//a", "//a/b", "//a/", "//.", "//..", "//./a", "//../a", "//?/a",
		"//a//b", "//a///b", "/", "/a", "///a",
	}
	for _, in := range absolute {
		t.Run("absolute "+in, func(t *testing.T) {
			if !parsePath(in, PathPOSIX).isAbsolute() {
				t.Errorf("parsePath(%q, POSIX).isAbsolute() = false, want true", in)
			}
		})
	}
	for _, in := range []string{"", ".", "..", "a", "a/b", "./a", "../a", "a//b"} {
		t.Run("relative "+in, func(t *testing.T) {
			if parsePath(in, PathPOSIX).isAbsolute() {
				t.Errorf("parsePath(%q, POSIX).isAbsolute() = true, want false", in)
			}
		})
	}
}

// pathCorpusPOSIX generates the inputs both the table and the differential run
// over: roots, separators, dot segments and trailing slashes in combination.
//
// THE ANCHOR LEADS ARE EXPLICIT, and the reason is the asymmetry with
// pathCorpusWindows that hid the "//" is_absolute bug for three tasks. That
// corpus carries 25 hand-added leads covering every Windows anchor shape by
// name; this one carried two ("" and "/"), so "//", "///" and "///a" were
// reachable only INCIDENTALLY — through an empty first segment, which is a
// coincidence of the segment list rather than a decision — and "////a" was not
// reachable at all. The multi-slash leads below make POSIX's three anchor
// shapes (none, "/", and POSIX.1-2017 section 4.13's own "//") deliberate
// inputs, crossed with every segment combination the same way the Windows
// corpus crosses its own.
func pathCorpusPOSIX() []string {
	segs := []string{"a", "b", ".", "..", "", "x.txt", ".hidden", "a.tar.gz"}
	var out []string
	for _, lead := range []string{"", "/", "//", "///", "////"} {
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

// TestPathName_StemSuffixMatchesPython is the differential
// TestPathProperties's hand-picked rows should have been backed by from the
// start: stem, suffix and suffixes operate on the final path COMPONENT only
// ("name" below), independent of any root or separator, so this drives
// splitStemSuffix and parsedPath.suffixes directly over a generated corpus of
// names rather than routing through parsePath/Eval.
//
// fix-round 1 found 13 mismatches in a 147-name version of this corpus: a
// name with TWO OR MORE leading dots followed by dot-free content
// ("..a", "...a", "....txt") kept every leading dot as part of the stem
// correctly, but a name with two or more leading dots followed by ANOTHER
// dot ("..a.b") did not — splitStemSuffix took strings.LastIndex over the
// UNSTRIPPED name and guarded only i <= 0, which catches a single leading
// dot at index 0 but not a run of two or more, where the last dot sits at
// index >= 1 and slips past the guard. suffixes() (three lines below in
// pathval.go) already had the right pattern — strings.TrimLeft the ENTIRE
// leading run before splitting — and splitStemSuffix now shares it via
// splitLeadingDots rather than deciding independently, which is what a
// previous task in this wave had to fix for exactly this failure shape (two
// formulas that agree on most inputs and diverge on one).
func TestPathName_StemSuffixMatchesPython(t *testing.T) {
	requirePython313(t)
	corpus := pathNameCorpus()
	script := `
import sys
from pathlib import PurePosixPath as P
for line in sys.stdin.read().split("\n"):
    p = P(line)
    print(p.stem + "\t" + p.suffix + "\t" + "\x1f".join(p.suffixes))
`
	out := runPython(t, script, strings.Join(corpus, "\n"))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(corpus) {
		t.Fatalf("python returned %d lines for %d inputs", len(lines), len(corpus))
	}
	mismatches := 0
	for i, name := range corpus {
		fields := strings.SplitN(lines[i], "\t", 3)
		wantStem, wantSuffix := fields[0], fields[1]
		var wantSuffixes []string
		if fields[2] != "" {
			wantSuffixes = strings.Split(fields[2], "\x1f")
		}
		stem, suffix := splitStemSuffix(name)
		if stem != wantStem || suffix != wantSuffix {
			t.Errorf("splitStemSuffix(%q) = (%q, %q), python (%q, %q)", name, stem, suffix, wantStem, wantSuffix)
			mismatches++
		}
		gotSuffixes := parsedPath{comps: []string{name}}.suffixes()
		if strings.Join(gotSuffixes, "\x1f") != strings.Join(wantSuffixes, "\x1f") {
			t.Errorf("suffixes(%q) = %q, python %q", name, gotSuffixes, wantSuffixes)
			mismatches++
		}
	}
	t.Logf("name corpus: %d names, %d mismatches", len(corpus), mismatches)
}

// pathNameCorpus generates final path COMPONENTS (never containing "/") for
// TestPathName_StemSuffixMatchesPython: 0-4 leading dots crossed with
// dot-free content, single and multiple extensions, trailing dots, all-dot
// names (a lead crossed with empty content) and non-ASCII.
//
// The bare single-dot name "." is deliberately EXCLUDED, not merely
// untested: pathlib treats a lone "." as its "no name at all" sentinel
// (PurePosixPath(".").stem is "", not "."), which is a special case of
// name() being empty, not of splitStemSuffix's dot-counting — and this
// package's own parsePath already enforces that a single "." can never
// become a STORED component (it is dropped during normalization, unlike
// ".." or "..." which are kept), so splitStemSuffix can never actually be
// called with "." as an argument through any real property. Pinning an
// expectation for it here would test an input the function's contract does
// not cover, not a real gap.
func pathNameCorpus() []string {
	leads := []string{"", ".", "..", "...", "...."}
	bodies := []string{
		"", "a", "abc", "x1", "é",
		"a.txt", "a.tar.gz", "a.b.c.d", "café.tar.gz",
		"a.", "a..", "a...",
	}
	var out []string
	seen := map[string]bool{".": true}
	for _, lead := range leads {
		for _, body := range bodies {
			n := lead + body
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
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
	// Python's print() writes the platform line ending, so on Windows every
	// line comes back "...\r\n". The callers strip only "\n" and then compare
	// tab-separated fields, so an unstripped "\r" rides along on the LAST field
	// of every line — turning "True" into "True\r" and quietly failing every
	// is_absolute / final-field comparison while String() and parts() (earlier
	// fields) still match. Normalize CRLF to LF here, at the one point every
	// differential funnels through, so the comparison is line-ending agnostic.
	return strings.ReplaceAll(string(out), "\r\n", "\n")
}

// requirePython313 skips a differential unless the python3 on PATH is CPython
// 3.13 or newer.
//
// CPython 3.13 changed two pathlib behaviours these differentials exercise, and
// sqi implements the 3.13 side of BOTH deliberately (pinned by TestPathProperties
// and by isAbsolute's own doc comment):
//   - PureWindowsPath.is_absolute() now delegates to ntpath.isabs, so a
//     driveless-but-colon-rooted path like "\:\a" is absolute (3.12 answered
//     false, using bool(drive and root));
//   - stem/suffix/suffixes stopped treating a name's leading OR trailing dot
//     run as an extension, so "a." is all stem and "..a" keeps both dots (3.12
//     split them, e.g. "..a" -> stem ".", suffix ".a").
//
// Run against an older interpreter, the oracle would report sqi diverging from
// semantics sqi deliberately does not implement — a false mismatch, not a
// defect. A skip here is honest for the same reason runPython's missing-python3
// skip is: the differential verifies nothing it can trust, so it must not
// assert. The version-STABLE POSIX differential does not call this; the CRLF
// normalization in runPython is all it needed to pass on any interpreter.
func requirePython313(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable — this differential verifies NOTHING when skipped")
	}
	out, err := exec.CommandContext(t.Context(), "python3", "-c",
		"import sys; print('%d %d' % sys.version_info[:2])").Output()
	if err != nil {
		t.Fatalf("python3 version probe failed: %v", err)
	}
	majS, minS, ok := strings.Cut(strings.TrimSpace(string(out)), " ")
	maj, errMaj := strconv.Atoi(majS)
	min, errMin := strconv.Atoi(minS)
	if !ok || errMaj != nil || errMin != nil {
		t.Fatalf("could not parse python3 version %q", strings.TrimSpace(string(out)))
	}
	if maj < 3 || (maj == 3 && min < 13) {
		t.Skipf("python3 is %d.%d; this differential needs >= 3.13 "+
			"(sqi implements 3.13 pathlib path semantics — see requirePython313)", maj, min)
	}
}

// TestParsePath_Windows pins PureWindowsPath. Every expectation came from
// running python3 during design.
//
// The drive-RELATIVE row is the one that matters most: "C:a\b" is NOT absolute,
// and its anchor is "C:" with no separator. Getting that wrong makes
// is_absolute() lie about the most common Windows path shape after "C:\".
func TestParsePath_Windows(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		parts []string
		abs   bool
	}{
		{`C:\a\b`, `C:\a\b`, []string{`C:\`, "a", "b"}, true},
		{`C:/a/b`, `C:\a\b`, []string{`C:\`, "a", "b"}, true},
		{`C:a\b`, `C:a\b`, []string{`C:`, "a", "b"}, false},
		{`C:/`, `C:\`, []string{`C:\`}, true},
		{`\\srv\share\x`, `\\srv\share\x`, []string{`\\srv\share\`, "x"}, true},
		{`\a`, `\a`, []string{`\`, "a"}, false},
		{`a/b`, `a\b`, []string{"a", "b"}, false},
		{`a`, `a`, []string{"a"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			p := parsePath(tc.in, PathWindows)
			if got := p.String(); got != tc.want {
				t.Errorf("parsePath(%q, Windows).String() = %q, want %q", tc.in, got, tc.want)
			}
			if got := strings.Join(p.parts(), "|"); got != strings.Join(tc.parts, "|") {
				t.Errorf("parsePath(%q, Windows).parts() = %q, want %q", tc.in, p.parts(), tc.parts)
			}
			if got := p.isAbsolute(); got != tc.abs {
				t.Errorf("parsePath(%q, Windows).isAbsolute() = %v, want %v", tc.in, got, tc.abs)
			}
		})
	}
}

// TestParsePath_WindowsMatchesPython is the real proof for this flavor. The
// oracle cannot reach it — the reference evaluates with its own path_format —
// so this differential is the ONLY automated check on Windows semantics, and it
// runs on Linux against Python rather than on Windows against anything.
func TestParsePath_WindowsMatchesPython(t *testing.T) {
	requirePython313(t)
	corpus := pathCorpusWindows()
	// Unlike an earlier draft of this script, blank input lines are NOT
	// skipped: pathCorpusWindows's "" lead produces exactly one blank line,
	// and (as TestParsePath_POSIXMatchesPython's harness comment explains) a
	// "if not line: continue" swallows that one real input and desyncs every
	// line thereafter, turning every later row into a false mismatch.
	script := `
import sys
from pathlib import PureWindowsPath as W
for line in sys.stdin.read().split("\n"):
    p = W(line)
    print(str(p) + "\t" + "\x1f".join(p.parts) + "\t" + str(p.is_absolute()))
`
	out := runPython(t, script, strings.Join(corpus, "\n"))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(corpus) {
		t.Fatalf("python returned %d lines for %d inputs", len(lines), len(corpus))
	}
	skipped := 0
	for i, in := range corpus {
		if strings.HasPrefix(in, `\\?\UNC\`) {
			// DOCUMENTED, DELIBERATE, BOUNDED divergence — not a defect.
			// pathlib gives the literal prefix "\\?\UNC\" its own
			// start-at-offset-8 parsing (see parseWindows's doc comment);
			// this file does not implement that branch, by design, as part
			// of this wave's stated extended-length/device-path omission.
			// Every input under this prefix is EXPECTED to disagree with
			// Python, so it is excluded from the pass/fail assertions below
			// rather than producing 318 "failures" that would bury a real
			// regression in noise. It still ran through parsePath above
			// (nothing here skips construction, only comparison), and it
			// stays IN the corpus specifically so a change that widens or
			// narrows this divergence is visible in the "skipped" count
			// logged below, not silently absent from the input set.
			skipped++
			continue
		}
		f := strings.SplitN(lines[i], "\t", 3)
		wantStr, wantAbs := f[0], f[2] == "True"
		var wantParts []string
		if f[1] != "" {
			wantParts = strings.Split(f[1], "\x1f")
		}
		p := parsePath(in, PathWindows)
		if got := p.String(); got != wantStr {
			t.Errorf("parsePath(%q, Windows).String() = %q, python %q", in, got, wantStr)
		}
		if got := strings.Join(p.parts(), "\x1f"); got != strings.Join(wantParts, "\x1f") {
			t.Errorf("parsePath(%q, Windows).parts() = %q, python %q", in, p.parts(), wantParts)
		}
		if got := p.isAbsolute(); got != wantAbs {
			t.Errorf("parsePath(%q, Windows).isAbsolute() = %v, python %v", in, got, wantAbs)
		}
	}
	t.Logf("compared %d inputs (%d skipped as the documented \\\\?\\UNC\\ omission)", len(corpus)-skipped, skipped)
}

// TestParsePath_URI pins the opacity rule. Every expectation was produced by
// running the reference implementation during design.
//
// Each row is something the filesystem flavors would normalize away. If the
// URI branch is ever skipped, these are what catch it — and they catch it
// loudly, where a match-behavior test on a normalized path would not.
func TestParsePath_URI(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		parts []string
	}{
		{"s3://bucket/dir/file.obj", "s3://bucket/dir/file.obj", []string{"s3://bucket", "dir", "file.obj"}},
		{"s3://bucket/a//b/c", "s3://bucket/a//b/c", []string{"s3://bucket", "a", "", "b", "c"}},
		{"s3://bucket/a/./b", "s3://bucket/a/./b", []string{"s3://bucket", "a", ".", "b"}},
		{"s3://bucket/dir/", "s3://bucket/dir/", []string{"s3://bucket", "dir", ""}},
		{"s3://bucket", "s3://bucket", []string{"s3://bucket"}},
		{"https://h/x/y", "https://h/x/y", []string{"https://h", "x", "y"}},
		// Task 11: a trailing slash IMMEDIATELY after the authority is the same
		// empty component the "s3://bucket/dir/" row above already pins, and it
		// was being discarded — see TestParsePath_URITrailingSlashAfterAuthority
		// for what that cost.
		{"s3://bucket/", "s3://bucket/", []string{"s3://bucket", ""}},
		{"s3://bucket//", "s3://bucket//", []string{"s3://bucket", "", ""}},
		// The negative that keeps the fix honest: no separator after the
		// authority at all means no path portion and therefore no component,
		// however the authority itself ends.
		{"s3://", "s3://", []string{"s3://"}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			// The flavor argument must not matter for a URI.
			for _, f := range []PathFormat{PathPOSIX, PathWindows} {
				p := parsePath(tc.in, f)
				if !p.isURI {
					t.Fatalf("parsePath(%q, %v) did not detect a URI", tc.in, f)
				}
				if got := p.String(); got != tc.want {
					t.Errorf("parsePath(%q, %v).String() = %q, want %q", tc.in, f, got, tc.want)
				}
				if got := strings.Join(p.parts(), "|"); got != strings.Join(tc.parts, "|") {
					t.Errorf("parsePath(%q, %v).parts() = %q, want %q", tc.in, f, p.parts(), tc.parts)
				}
				if !p.isAbsolute() {
					t.Errorf("parsePath(%q, %v).isAbsolute() = false; a URI is always absolute", tc.in, f)
				}
			}
		})
	}
}

// TestParsePath_URITrailingSlashAfterAuthority pins the consequences of the
// empty component a bare "s3://bucket/" carries, which parsePath used to
// discard.
//
// splitURI reports the SAME rest ("") for two different inputs — "s3://bucket",
// where there is no separator after the authority at all, and "s3://bucket/",
// where there is one with nothing after it — and parsePath's `if rest != ""`
// guard then collapsed the two into one value. That is normalization, which
// the specification forbids for the path portion of a URI ("consecutive
// slashes, `.`, and `..` segments are preserved verbatim", Expression-Language
// section 1.2.1), and it made this file's own rules disagree with each other:
// "s3://bucket/dir/" kept its trailing empty component while "s3://bucket/"
// lost it, though a trailing separator produced both.
//
// It was not cosmetic. Path values are normalized by construction (Value.Path
// re-parses), so the loss happened at construction and every later operation
// saw the shortened text: appending an object key to a bucket URI produced
// "s3://bucketkey", a different and legitimate-looking bucket.
//
// The reference implementation renders "s3://bucket/" too, and disagrees only
// about that value's parts, where it contradicts itself — it reports
// ["s3://bucket"] for "s3://bucket/" but ["s3://bucket", "", ""] for
// "s3://bucket//", and its own parts therefore break the specification's
// path(p.parts) == p roundtrip for the first. See test/oracle/baseline.txt.
func TestParsePath_URITrailingSlashAfterAuthority(t *testing.T) {
	tests := []struct{ src, want string }{
		{`path('s3://bucket/')`, "s3://bucket/"},
		{`path('s3://bucket')`, "s3://bucket"},
		{`path('s3://bucket/') + 'key'`, "s3://bucket/key"},
		{`path('s3://bucket') + 'key'`, "s3://bucketkey"},
		{`path('s3://bucket/') / 'key'`, "s3://bucket/key"},
		{`path('s3://bucket/').name`, ""},
		{`path('s3://bucket/').parent`, "s3://bucket"},
		{`join(path('s3://bucket/').parts, '|')`, "s3://bucket|"},
		{`path(path('s3://bucket/').parts) == path('s3://bucket/')`, "true"},
		{`path('s3://bucket/') == path('s3://bucket')`, "false"},
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
		})
	}
}

// TestParsePath_URIDetection pins the specification's own regex boundary. A
// string that merely contains "://" is not a URI, and a scheme must start with
// a letter.
func TestParsePath_URIDetection(t *testing.T) {
	uris := []string{"s3://b", "https://h", "a+b.c-d://x", "S3://b"}
	notURIs := []string{"/a/b", "a/b", "://x", "1s://x", "a:/b", "a//b", "/x://y"}
	for _, s := range uris {
		if !parsePath(s, PathPOSIX).isURI {
			t.Errorf("parsePath(%q) should be a URI", s)
		}
	}
	for _, s := range notURIs {
		if parsePath(s, PathPOSIX).isURI {
			t.Errorf("parsePath(%q) should NOT be a URI", s)
		}
	}
}

func pathCorpusWindows() []string {
	leads := []string{
		"", `C:`, `C:\`, `C:/`, `\`, `/`, `\\srv\share\`, `\\srv\share`,
		// Fix-round-1 additions: a share-less UNC, a UNC with a bare trailing
		// separator (no share), and all-separator strings — none of these
		// shapes were reachable from the original 8 leads, and each exposed
		// a real defect the 848-input corpus could not catch: isAbsolute()
		// misclassifying a share-less UNC, and the UNC root builder
		// fabricating a separator that was never in the input.
		`\\srv`, `\\srv\`, `\\`, `\\\`, `\\\\`,
		// Fix-round-2 additions (the coordinator's own leads, reproduced
		// verbatim): a two-character "?." server (the root-synthesis
		// substring-exclusion bug — three inequalities let "?." itself
		// through); a raw ":\" root (isAbsolute's rendered-string prefix
		// test disagreeing with a shape-based proxy, independent of any
		// parsed root shape); a leading "." before a colon-bearing component
		// (String()'s missing "." disambiguation prefix, and its forward-
		// slash twin); and non-ASCII, digit, and punctuation drive letters
		// (byte- vs rune-indexed drive detection — Python's ntpath accepts
		// any single code point before ':', not just ASCII letters).
		`\\?.\`, `\:\a`, `.\a:b`, `./c:`, `é:`, `£:`, `1:`, `::`, ` :`,
		// An explicit "\\?\UNC\..." lead: the ONE shape this task
		// deliberately does not port (see parseWindows's doc comment) —
		// included so the differential can show this is the ONLY remaining
		// mismatch category, not merely untested.
		`\\?\UNC\srv\share\`, `\\?\UNC\srv\share`, `\\?\UNC\srv`,
	}
	segs := []string{"a", "b", ".", "..", "x.txt"}
	var out []string
	for _, lead := range leads {
		out = append(out, lead)
		for _, s1 := range segs {
			out = append(out, lead+s1)
			for _, s2 := range segs {
				out = append(out, lead+s1+`\`+s2)
				out = append(out, lead+s1+"/"+s2)
				out = append(out, lead+s1+`\\`+s2)
				out = append(out, lead+s1+`\`+s2+`\`)
			}
		}
	}
	return out
}
