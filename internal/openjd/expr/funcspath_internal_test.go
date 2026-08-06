// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"strings"
	"testing"
)

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

// TestPathRoundTrip_PartsProperty is fix-round 1's property test: RFC 0006
// states path(p.parts) == p, and the review that found the "C:" + "a" bug did
// so with a generated corpus, not a single table row, because a second
// formula beside parsedPath.String() agreed with the first on almost every
// input and disagreed on exactly one shape. A single regression row proves
// that ONE shape is fixed; it does not prove no OTHER shape still disagrees.
//
// The corpus is built by taking a raw path TEXT, parsing it once to get the
// canonical parsedPath p, taking p.parts(), handing those parts BACK through
// path() exactly as the specification's equation does, and comparing p's own
// canonical rendering to what came back. This is exercised through Eval and
// the real "path" registry entry — not by calling pathFromParts directly —
// so the property covers the whole path from FnCtx dispatch down, not just
// the helper in isolation.
//
// Three shapes, matching the fix-round report's "all three flavors":
//   - POSIX roots ("", "/", "//") crossed with bodies covering plain
//     components, ".", "..", and doubled separators.
//   - Windows roots covering every anchor shape pathval.go names by name:
//     none, a bare drive ("C:", the exact shape the bug was in), a
//     drive-with-root ("C:\"), a root with no drive ("\"), and a UNC root
//     both with and without its own trailing separator (the
//     synthesizeUNCRoot case) — crossed with bodies including one
//     ("a:b") that looks like a second drive to make sure only the head is
//     ever classified as a root.
//   - URI roots (several schemes, including one exercising the full
//     scheme-byte grammar) crossed with bodies that reintroduce EMPTY
//     components ("a//b", "//a") — URIs keep those, filesystem paths do
//     not, and this is what the brief called out by name — run under BOTH
//     PathPOSIX and PathWindows to confirm URI opacity does not depend on
//     the chosen flavor.
func TestPathRoundTrip_PartsProperty(t *testing.T) {
	type kase struct {
		text   string
		flavor PathFormat
	}
	var corpus []kase

	posixRoots := []string{"", "/", "//"}
	posixBodies := []string{
		"", "a", "a/b", "a/b/c", "a/./b", "a//b", "a/b/",
		"..", "a/../b", "x/y/z/w/v", "a/b/c/d/e", ".",
	}
	for _, root := range posixRoots {
		for _, body := range posixBodies {
			corpus = append(corpus, kase{root + body, PathPOSIX})
		}
	}

	// windowsText joins a root and a body the way a real Windows path text
	// would: most roots already end in a separator (or are empty, or are a
	// bare drive that attaches with NONE by design — the exact case the bug
	// was in), so those concatenate directly; the one root pathval.go's
	// synthesizeUNCRoot documents as lacking its own trailing separator
	// needs one inserted to stay a valid two-component UNC share rather than
	// merging into the body.
	windowsText := func(root, body string) string {
		switch {
		case root == "" || body == "":
			return root + body
		case strings.HasSuffix(root, `\`):
			return root + body
		case root == "C:":
			return root + body
		default:
			return root + `\` + body
		}
	}
	windowsRoots := []string{
		"", "C:", `C:\`, `\`, `\\srv\share\`, `\\srv\share`,
	}
	windowsBodies := []string{
		"", "a", `a\b`, `a\b\c`, `a\.\b`, "a:b", `a\b\`,
		"..", `a\..\b`, `x\y\z`,
	}
	for _, root := range windowsRoots {
		for _, body := range windowsBodies {
			corpus = append(corpus, kase{windowsText(root, body), PathWindows})
		}
	}

	uriText := func(root, body string) string {
		if body == "" {
			return root
		}
		return root + "/" + body
	}
	uriRoots := []string{
		"s3://bucket", "s3://bucket:1234", "file://host", "x+y-z.1://auth", "s3://",
	}
	uriBodies := []string{
		"", "a", "a/b", "a//b", "//a", "a/", "a//", "a/b//c", ".", "..", "a/./b",
	}
	for _, root := range uriRoots {
		for _, body := range uriBodies {
			text := uriText(root, body)
			corpus = append(corpus, kase{text, PathPOSIX}, kase{text, PathWindows})
		}
	}

	failures := 0
	for _, c := range corpus {
		orig := parsePath(c.text, c.flavor)
		want := orig.String()

		partStrs := orig.parts()
		elems := make([]Value, len(partStrs))
		for i, s := range partStrs {
			elems[i] = String(s)
		}
		syms := MapSymbols{"Param.Parts": List(TString, elems)}
		got, err := Eval("path(Param.Parts)", syms, TAny, WithPathFormat(c.flavor))
		if err != nil {
			t.Errorf("path(parts of %q under flavor %v) failed: %v", c.text, c.flavor, err)
			failures++
			continue
		}
		if got.String() != want {
			t.Errorf("path(p.parts) != p for text %q (flavor %v): parts=%v, got %q, want %q",
				c.text, c.flavor, partStrs, got.String(), want)
			failures++
		}
	}

	t.Logf("path(p.parts) == p round-trip property: %d cases, %d failures", len(corpus), failures)
}

// TestPathProperties pins Python's pathlib semantics, including the cases that
// surprise people. Every expectation came from running python3 during design.
//
// ".hidden" has NO suffix — a leading dot is not an extension. "a." has stem
// "a" and suffix "." , which is where the reference implementation disagrees
// with Python; RFC 0006 says these properties match pathlib, so Python wins and
// the difference is baselined in test/oracle/baseline.txt.
func TestPathProperties(t *testing.T) {
	tests := []struct{ src, want string }{
		{`path('/projects/shot01/render.exr').name`, "render.exr"},
		{`path('/projects/shot01/render.exr').stem`, "render"},
		{`path('/projects/shot01/render.exr').suffix`, ".exr"},
		{`path('/projects/shot01/render.exr').parent`, "/projects/shot01"},
		{`path('/data/backup.tar.gz').suffix`, ".gz"},
		{`path('/data/backup.tar.gz').stem`, "backup.tar"},
		{`path('.hidden').name`, ".hidden"},
		{`path('.hidden').stem`, ".hidden"},
		{`path('.hidden').suffix`, ""},
		{`path('a.').stem`, "a"},
		{`path('a.').suffix`, "."},
		{`path('..').suffix`, ""},
		{`path('noext').suffix`, ""},
		{`path('/a/b').parent`, "/a"},
		{`path('/a').parent`, "/"},
		{`path('/').parent`, "/"},
		{`path('a').parent`, "."},
		{`path('s3://bucket/dir/file.obj').name`, "file.obj"},
		{`path('s3://bucket/dir/file.obj').parent`, "s3://bucket/dir"},
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

func TestPathListProperties(t *testing.T) {
	tests := []struct{ src, want string }{
		{`join(path('/data/backup.tar.gz').suffixes, '|')`, ".tar|.gz"},
		{`len(path('/data/backup.tar.gz').suffixes)`, "2"},
		{`len(path('/a/b').suffixes)`, "0"},
		{`join(path('/a/b').parts, '|')`, "/|a|b"},
		{`join(path('a/b').parts, '|')`, "a|b"},
		{`len(path('.').parts)`, "0"},
		{`join(path('s3://bucket/dir/file.obj').parts, '|')`, "s3://bucket|dir|file.obj"},
		{`join(path('s3://bucket/a//b/c').parts, '|')`, "s3://bucket|a||b|c"},
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

// TestPathParts_Roundtrip pins the specification's own claim that
// path(p.parts) == p, over the same generated corpus the engine differential
// uses — so it is a property over many inputs, not three hand-picked ones.
//
// DEVIATION 1 from the brief's literal test body: the brief calls a helper
// named joinParts(parts, flavor) that does not exist in this codebase.
// task-5's fix round DELETED joinParts outright — it was a second formula for
// "how does a root join its components" that disagreed with
// parsedPath.String() on a bare Windows drive ("C:" + "a"; see
// funcspath.go's pathFromParts doc comment) — and replaced every caller with
// pathFromParts, which defers to String() instead of re-deriving the rule.
// Writing a fresh joinParts here to satisfy the brief literally would be
// exactly the second copy of that formula the fix round removed. This test
// therefore calls pathFromParts(parts, flavor).String() in joinParts's place;
// the expected VALUES (parts, roundtrip equality) are unchanged from the
// brief, only the helper name is current.
//
// DEVIATION 2: this stays POSIX-only (pathCorpusPOSIX, matching the brief)
// rather than also folding in pathCorpusWindows. That corpus was built for a
// DIFFERENT property (TestParsePath_WindowsMatchesPython's parse/render/
// is_absolute differential) and several of its deliberately adversarial
// leads — "./c:", ".\a:b" — produce a component whose text is itself
// drive-shaped ("c:.", "a:b") only because a separator hid it from
// splitRootWindows on the ORIGINAL parse. Feeding that component back through
// parts() and pathFromParts reparses it in ISOLATION, where the same text now
// reads as a real drive, and the round trip genuinely does not hold — not a
// bug here, since python3 was run to confirm PureWindowsPath itself fails
// the identical way: W(*W("./c:.").parts) renders "c:", not ".\c:.". C4 Task
// 7 (with_* and relative_to) or a later fix round is where any additional
// classification would need to be reconsidered, not this task, and reusing
// pathCorpusWindows here would pin an equality RFC 0006 does not actually
// hold. Three-flavor coverage of the property THAT DOES hold is already
// delivered by TestPathRoundTrip_PartsProperty above (206 cases spanning
// POSIX, Windows and URI, curated to avoid this exact known limitation) —
// this test adds POSIX-differential-corpus breadth on top, per the brief.
func TestPathParts_Roundtrip(t *testing.T) {
	for _, in := range pathCorpusPOSIX() {
		p := parsePath(in, PathPOSIX)
		rebuilt := parsePath(pathFromParts(p.parts(), PathPOSIX).String(), PathPOSIX)
		if rebuilt.String() != p.String() {
			t.Errorf("roundtrip %q: parts %q rebuilt to %q, want %q",
				in, p.parts(), rebuilt.String(), p.String())
		}
	}
}

// TestPathProperty_ReceiverIsNotCoerced pins section 1.2.4 for a PROPERTY: a
// string receiver must not silently become a path.
func TestPathProperty_ReceiverIsNotCoerced(t *testing.T) {
	_, err := Eval(`'/a/b.txt'.stem`, MapSymbols{}, TAny)
	if err == nil {
		t.Fatal("'/a/b.txt'.stem succeeded; a string receiver must not coerce to path")
	}
}

func TestPathWithFunctions(t *testing.T) {
	tests := []struct{ src, want string }{
		{`path('/projects/shot01/render.exr').with_name('output.png')`, "/projects/shot01/output.png"},
		{`path('/projects/shot01/render.exr').with_stem('final')`, "/projects/shot01/final.exr"},
		{`path('/projects/shot01/render.exr').with_suffix('.png')`, "/projects/shot01/render.png"},
		{`path('/projects/shot01/render.exr').with_suffix('')`, "/projects/shot01/render"},
		{`path('/a/b').relative_to(path('/a'))`, "b"},
		{`path('/a/b/c').relative_to(path('/a'))`, "b/c"},
		{`path('/a/b').is_relative_to(path('/a'))`, "true"},
		{`path('/a/b').is_relative_to(path('/x'))`, "false"},
		{`path('s3://b/x/y').is_relative_to(path('s3://b/x'))`, "true"},
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

// TestPathWithFunctions_Reject pins the three error conditions. All three
// behave the same way in Python and in the reference — measured during design —
// so any divergence here is a bug rather than an adjudication.
func TestPathWithFunctions_Reject(t *testing.T) {
	tests := []struct {
		src     string
		wantErr error
	}{
		{`path('/').with_name('x')`, errEmptyName},
		{`path('/').with_stem('x')`, errEmptyName},
		{`path('/a/b.txt').with_suffix('png')`, errInvalidSuffix},
		{`path('/a/b').relative_to(path('/x'))`, errNotRelative},
		// with_number's path row runs its result through the SAME withName
		// as with_name and with_stem, so a receiver with no final component
		// fails the ordinary errEmptyName rather than a second check — even
		// though withNumber itself never inspects the receiver at all.
		// Measured against the reference during design.
		{`path('/').with_number(3)`, errEmptyName},
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

// TestPathWithFunctions_ArgumentValidation is fix round 1: with_name,
// with_stem and with_suffix originally accepted ANY replacement text,
// including one containing a separator, fabricating a path component that
// did not exist in the input. Every expectation here is the reference's
// actual output at openjd-model 0.11.1, measured by the reviewer during fix
// round 1 — not adjudicated, not derived from Python alone.
func TestPathWithFunctions_ArgumentValidation(t *testing.T) {
	tests := []struct{ src, want string }{
		// CRITICAL 1: with_name validates its argument, but ".." is legal —
		// only exact equality to "." is rejected, not a leading dot run.
		{`path('/a/b.txt').with_name('..')`, "/a/.."},
		// CRITICAL 2 (positive branch): with_stem on a receiver whose own
		// suffix is EMPTY is exactly with_name — no suffix to preserve.
		{`path('/data/backup.tar.gz').with_stem('x')`, "/data/x.gz"},
		// Separators are flavor-dependent: backslash is not a POSIX
		// separator, so it survives untouched under the default (POSIX)
		// flavor — including when the receiver happens to look
		// drive-rooted, because POSIX parsing never treats ':' specially,
		// so the drive does not switch the flavor. Measured directly
		// against the reference.
		{`path('a/b').with_name('c\\d')`, `a/c\d`},
		{`path('C:/a/b').with_name('c\\d')`, `C:/a/c\d`},
		// Cross-validation extras (reasoned from the rule, not the
		// reference): a leading-dot-RUN replacement is legal, a
		// multi-extension replacement name is ordinary, and a non-ASCII
		// replacement round-trips untouched.
		{`path('/a/b.txt').with_name('..hidden')`, "/a/..hidden"},
		{`path('/a/b.txt').with_name('archive.tar.gz')`, "/a/archive.tar.gz"},
		{`path('/a/b.txt').with_name('café')`, "/a/café"},
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

// TestPathWithFunctions_ArgumentValidation_Reject is fix round 1's error
// side, including the two ordering cases the review's Important finding
// raised and the reference then REFUTED: suffix-format validation runs
// BEFORE the receiver's empty-name check, opposite of Python 3.14, and that
// is deliberate — see the ordering comment on the with_suffix registration.
func TestPathWithFunctions_ArgumentValidation_Reject(t *testing.T) {
	tests := []struct {
		src     string
		wantErr error
	}{
		// CRITICAL 1.
		{`path('/a/b.txt').with_name('a/b')`, errInvalidName},
		{`path('/a/b.txt').with_name('')`, errInvalidName},
		{`path('/a/b.txt').with_name('.')`, errInvalidName},
		// CRITICAL 2.
		{`path('/a/b').with_stem('')`, errInvalidName},
		{`path('/a/b').with_stem('.')`, errInvalidName},
		{`path('/a/b.txt').with_stem('')`, errEmptyStemHasSuffix},
		{`path('/a/b.txt').with_stem('a/b')`, errInvalidName},
		// CRITICAL 3.
		{`path('/a/b.txt').with_suffix('.')`, errInvalidSuffix},
		{`path('/a/b.txt').with_suffix('.a/b')`, errInvalidSuffix},
		// Ordering, refuted by the reference: suffix format wins over the
		// receiver's empty name, not the other way around.
		{`path('/').with_suffix('png')`, errInvalidSuffix},
		{`path('/').with_suffix('.png')`, errEmptyName},
		// Cross-validation extra: a replacement that is only separators.
		{`path('/a/b.txt').with_name('/')`, errInvalidName},
		{`path('/a/b.txt').with_name('//')`, errInvalidName},
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

// TestPathWithFunctions_WindowsSeparator has NO reference answer — the
// reference is POSIX-only for these functions (measured: it accepts a
// backslash outright, see TestPathWithFunctions_ArgumentValidation above).
// Under the Windows flavor this package parses BOTH "/" and "\" as
// separators (parseWindows itself normalizes "/" to "\" before splitting on
// it), so a replacement name containing either must be rejected there even
// though the identical text is legal under POSIX.
func TestPathWithFunctions_WindowsSeparator(t *testing.T) {
	_, err := Eval(`path('a/b').with_name('c\\d')`, MapSymbols{}, TAny, WithPathFormat(PathWindows))
	if err == nil {
		t.Fatal(`with_name('c\d') under the Windows flavor succeeded; want errInvalidName`)
	}
	if !errors.Is(err, errInvalidName) {
		t.Errorf(`with_name('c\d') under the Windows flavor = %v, want it to wrap errInvalidName`, err)
	}
}
