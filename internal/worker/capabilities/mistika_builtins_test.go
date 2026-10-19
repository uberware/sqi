// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
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

// TestMistikaDetectors_MatchVendorInstallLayout checks that each shipped glob
// matches the SGO install path (assertInstallsMatchGlobs says why it uses
// path.Match).
//
// This is what catches the subtle failure: the .psx files spell the macOS
// bundle directory "Contents/MACOS", which works for Smedge only because macOS
// volumes are case-insensitive by default. filepath.Match is case-sensitive, so
// copying that spelling would produce a detector that never fires on a real
// install while looking perfectly correct.
func TestMistikaDetectors_MatchVendorInstallLayout(t *testing.T) {
	for _, layout := range mistikaLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			assertInstallsMatchGlobs(t, builtinDetector(t, layout.tag), layout.installs)
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
			d := builtinDetector(t, layout.tag)
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
// is not portable (see assertWindowsGlobsGated).
func TestMistikaDetectors_WindowsCheckIsGated(t *testing.T) {
	for _, layout := range mistikaLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			assertWindowsGlobsGated(t, builtinDetector(t, layout.tag))
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
			d := builtinDetector(t, layout.tag)
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
	d := builtinDetector(t, "mistika")
	hit := "/home/mistika/SGO Apps/Mistika Ultima 10.10/bin/mistika"
	env := fakeEnv{
		goos:  "linux",
		globs: map[string][]string{"/home/mistika/SGO Apps/Mistika Ultima*/bin/mistika": {hit}},
	}
	got := strings.Join(d.Evaluate(env), ",")
	if got != "mistika,mistika_10_10" {
		t.Errorf("Evaluate = %q, want %q", got, "mistika,mistika_10_10")
	}
	if got := d.Evaluate(fakeEnv{goos: "linux"}); len(got) != 0 {
		t.Errorf("a worker with no Mistika installed advertised %v", got)
	}
}
