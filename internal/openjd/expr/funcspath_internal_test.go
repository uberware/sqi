// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
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
