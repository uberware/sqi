// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"strings"
	"testing"
)

// The three Mistika detectors are translated from Smedge's own executable
// discovery -- the [FindLatestExecutable] blocks in
// Mistika_Ultima_And_Boutique.psx / MistikaVR.psx and the FindExecutable*
// parameter defaults in MistikaWorkflows.json. Smedge searches
// <Root>/<Base>*/<Exe>, appending the trailing "*" itself when the configured
// base carries no glob character (ProcessJob.cpp _FindInRoot).
//
// This table restates the install layouts those sources describe, so the test
// below checks the shipped globs against the vendor layout rather than against
// a copy of themselves.
var mistikaLayouts = []struct {
	tag string
	// installs are real paths the detector MUST match, one per platform.
	installs []string
	// version is the version this detector should extract from a versioned
	// install directory, and versioned is the path carrying it.
	versioned string
	version   string
}{
	{
		tag: "mistika",
		installs: []string{
			"/home/mistika/SGO Apps/Mistika Ultima 10.10/bin/mistika",
			"/Applications/SGO Apps/Mistika Boutique.app/Contents/MacOS/mistika",
			`C:\Program Files\SGO Apps\Mistika Boutique 10.10\bin\mistika.exe`,
		},
		versioned: `C:\Program Files\SGO Apps\Mistika Boutique 10.10\bin\mistika.exe`,
		version:   "10.10",
	},
	{
		tag: "mistikavr",
		installs: []string{
			"/home/mistika/SGO Apps/Mistika VR 10.10/bin/vr",
			"/Applications/SGO Apps/Mistika VR.app/Contents/MacOS/vr",
			`C:\Program Files\SGO Apps\Mistika VR 10.10\bin\vr.exe`,
		},
		versioned: "/home/mistika/SGO Apps/Mistika VR 10.10/bin/vr",
		version:   "10.10",
	},
	{
		tag: "mistikaworkflows",
		installs: []string{
			"/home/mistika/SGO Apps/Mistika Workflows 10.10/bin/workflows",
			"/Applications/SGO Apps/Mistika Workflows.app/Contents/MacOS/workflows",
			`C:\Program Files\SGO Apps\Mistika Workflows 10.10\bin\workflows.exe`,
		},
		versioned: "/Applications/SGO Apps/Mistika Workflows.app/Contents/MacOS/workflows",
		version:   "",
	},
}

// mistikaDetector returns the built-in detector emitting tag.
func mistikaDetector(t *testing.T, tag string) Detector {
	t.Helper()
	ds, err := BuiltinDetectors()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Tag == tag {
			return d
		}
	}
	t.Fatalf("no built-in detector emits the %q tag", tag)
	return Detector{}
}

// TestMistikaDetectors_MatchVendorInstallLayout is the substantive check: each
// shipped glob must actually match the SGO install path Smedge looks for.
//
// It matches with path.Match on slash-normalized strings rather than calling
// filepath.Glob, for two reasons: the install paths are absolute and do not
// exist on a test runner, and the Windows pattern's backslashes are separators
// on Windows but escape characters to filepath.Match everywhere else, so a
// host-native match would silently test something different per platform.
// path.Match on normalized input is the same per-component wildcard semantics
// filepath.Glob applies, checked identically on every runner.
//
// This is what catches the subtle failure: the .psx files spell the macOS
// bundle directory "Contents/MACOS", which works for Smedge only because macOS
// volumes are case-insensitive by default. filepath.Match is case-sensitive, so
// copying that spelling would produce a detector that never fires on a real
// install while looking perfectly correct.
func TestMistikaDetectors_MatchVendorInstallLayout(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	for _, layout := range mistikaLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := mistikaDetector(t, layout.tag)
			for _, install := range layout.installs {
				var matched bool
				for _, c := range d.Checks {
					ok, err := path.Match(norm(c.PathGlob), norm(install))
					if err != nil {
						t.Fatalf("bad pattern %q: %v", c.PathGlob, err)
					}
					if ok {
						matched = true
						break
					}
				}
				if !matched {
					t.Errorf("no check matches the vendor install path %s", install)
				}
			}
		})
	}
}

// TestMistikaDetectors_NoBareExeProbe pins the deliberate omission. Mistika's
// binaries are `mistika`, `vr` and `workflows`; the presets invoke them by bare
// name, so an `exe:` check is the obvious shortcut. It is wrong here: "vr" and
// "workflows" are generic enough to exist on a machine with no Mistika, and a
// falsely tagged worker gets handed render work it cannot run. Detection is by
// install location only.
func TestMistikaDetectors_NoBareExeProbe(t *testing.T) {
	for _, layout := range mistikaLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := mistikaDetector(t, layout.tag)
			if len(d.Checks) == 0 {
				t.Fatal("no checks")
			}
			for i, c := range d.Checks {
				if c.Exe != "" {
					t.Errorf("check %d probes PATH for %q; detection must be by install location", i, c.Exe)
				}
			}
		})
	}
}

// TestMistikaDetectors_WindowsCheckIsGated guards the one check whose pattern
// is not portable: backslashes are separators on Windows and escapes to
// filepath.Match elsewhere, so an ungated Windows glob would be an ErrBadPattern
// (silently no matches) on POSIX rather than the no-op it looks like.
func TestMistikaDetectors_WindowsCheckIsGated(t *testing.T) {
	for _, layout := range mistikaLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := mistikaDetector(t, layout.tag)
			for i, c := range d.Checks {
				if strings.Contains(c.PathGlob, `\`) && c.OS != "windows" {
					t.Errorf("check %d has a backslash pattern %q but os = %q, want %q",
						i, c.PathGlob, c.OS, "windows")
				}
			}
		})
	}
}

// TestMistikaDetectors_ExtractVersion checks the version regexes against a
// versioned install directory, and confirms an unversioned one (the macOS
// bundle, which carries no version in its name) yields no version tag rather
// than a bogus one.
func TestMistikaDetectors_ExtractVersion(t *testing.T) {
	for _, layout := range mistikaLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := mistikaDetector(t, layout.tag)
			if got := d.extractVersion(layout.versioned); got != layout.version {
				t.Errorf("extractVersion(%q) = %q, want %q", layout.versioned, got, layout.version)
			}
		})
	}
}

// TestMistikaDetectors_EvaluateEmitsTags drives the real Evaluate path: a
// worker whose glob hits must advertise the bare tag, and the versioned variant
// when the install directory names a version.
func TestMistikaDetectors_EvaluateEmitsTags(t *testing.T) {
	d := mistikaDetector(t, "mistika")
	hit := "/home/mistika/SGO Apps/Mistika Ultima 10.10/bin/mistika"
	env := fakeEnv{
		goos:  "linux",
		globs: map[string][]string{"/home/mistika/SGO Apps/Mistika Ultima*/bin/mistika": {hit}},
	}
	got := strings.Join(d.Evaluate(env), ",")
	if got != "mistika,mistika-10.10" {
		t.Errorf("Evaluate = %q, want %q", got, "mistika,mistika-10.10")
	}
	if got := d.Evaluate(fakeEnv{goos: "linux"}); len(got) != 0 {
		t.Errorf("a worker with no Mistika installed advertised %v", got)
	}
}
