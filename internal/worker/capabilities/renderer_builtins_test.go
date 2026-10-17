// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"slices"
	"strings"
	"testing"
)

// The standalone-renderer detectors pair with the presets in presets/sqi/ that
// invoke a renderer directly rather than through a DCC. Each probes PATH for
// the command its preset runs and, where one exists, the default install
// location.
//
// rendererLayout lists each detector's expected install paths independently of
// its YAML, so the tests check the shipped globs against them.
type rendererLayout struct {
	tag string
	// exe is the bare command the paired preset invokes; the detector must
	// probe PATH for it.
	exe string
	// installs are install paths some path_glob check must match.
	installs []string
	// versioned is an install path that carries a version, and version is the
	// version the detector must extract from it. Both are empty when the
	// detector extracts no version.
	versioned string
	version   string
}

var rendererLayouts = []rendererLayout{
	{
		tag: "arnold",
		exe: "kick",
		installs: []string{
			"/usr/autodesk/arnold/maya2026/bin/kick",
			`C:\Program Files\Autodesk\Arnold\maya2026\bin\kick.exe`,
		},
	},
	{
		tag: "vray",
		exe: "vray",
		installs: []string{
			"/usr/Chaos/V-Ray/Standalone_for_centos7/bin/vray",
			`C:\Program Files\Chaos\V-Ray\Standalone for x64\bin\vray.exe`,
		},
	},
	{
		tag: "husk",
		exe: "husk",
		installs: []string{
			"/opt/hfs21.0.440/bin/husk",
			`C:\Program Files\Side Effects Software\Houdini 21.0.440\bin\husk.exe`,
		},
		versioned: "/opt/hfs21.0.440/bin/husk",
		version:   "21.0",
	},
	{
		tag: "redshift",
		exe: "redshiftCmdLine",
		installs: []string{
			"/usr/redshift/bin/redshiftCmdLine",
			"/Applications/Maxon Redshift 2026/bin/redshiftCmdLine",
			`C:\Program Files\Maxon Redshift 2026\bin\redshiftCmdLine.exe`,
		},
		versioned: `C:\Program Files\Maxon Redshift 2026\bin\redshiftCmdLine.exe`,
		version:   "2026",
	},
	{
		tag: "renderman",
		exe: "prman",
		installs: []string{
			"/opt/pixar/RenderManProServer-27.2/bin/prman",
			"/Applications/Pixar/RenderManProServer-27.2/bin/prman",
			`C:\Program Files\Pixar\RenderManProServer-27.2\bin\prman.exe`,
		},
		versioned: "/opt/pixar/RenderManProServer-27.2/bin/prman",
		version:   "27.2",
	},
	{
		tag: "3delight",
		exe: "renderdl",
		installs: []string{
			"/Applications/3Delight/bin/renderdl",
			`C:\Program Files\3Delight\bin\renderdl.exe`,
		},
	},
	{
		tag: "mantra",
		exe: "mantra",
		installs: []string{
			"/opt/hfs21.0.440/bin/mantra",
			`C:\Program Files\Side Effects Software\Houdini 21.0.440\bin\mantra.exe`,
		},
		versioned: "/opt/hfs21.0.440/bin/mantra",
		version:   "21.0",
	},
}

// TestRendererDetectors_MatchVendorInstallLayout checks that a shipped glob
// matches every install path in rendererLayouts.
func TestRendererDetectors_MatchVendorInstallLayout(t *testing.T) {
	for _, layout := range rendererLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			assertInstallsMatchGlobs(t, builtinDetector(t, layout.tag), layout.installs)
		})
	}
}

// TestRendererDetectors_ProbePATHForTheCommand checks that each detector has an
// ungated exe check for the bare command its preset runs.
func TestRendererDetectors_ProbePATHForTheCommand(t *testing.T) {
	for _, layout := range rendererLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := builtinDetector(t, layout.tag)
			for _, c := range d.Checks {
				if c.Exe == layout.exe && c.OS == "" {
					return
				}
			}
			t.Errorf("no ungated exe check for %q", layout.exe)
		})
	}
}

// TestRendererDetectors_WindowsCheckIsGated guards the backslash globs (see
// assertWindowsGlobsGated).
func TestRendererDetectors_WindowsCheckIsGated(t *testing.T) {
	for _, layout := range rendererLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			assertWindowsGlobsGated(t, builtinDetector(t, layout.tag))
		})
	}
}

// TestRendererDetectors_ExtractVersion checks the version extracted from a
// versioned install path, and that a detector with no version extracts none.
func TestRendererDetectors_ExtractVersion(t *testing.T) {
	for _, layout := range rendererLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := builtinDetector(t, layout.tag)
			if layout.versioned == "" {
				for _, install := range layout.installs {
					if got := d.extractVersion(install); got != "" {
						t.Errorf("extractVersion(%q) = %q, want no version", install, got)
					}
				}
				return
			}
			if got := d.extractVersion(layout.versioned); got != layout.version {
				t.Errorf("extractVersion(%q) = %q, want %q", layout.versioned, got, layout.version)
			}
		})
	}
}

// TestRendererDetectors_HoudiniUtilitiesVersionOnEveryOS checks that husk,
// mantra and houdini extract the same version from each platform's install
// path (hfs21.0.440, Houdini21.0.440, "Houdini 21.0.440"). houdini is included
// because its hython path on Windows is inside the spaced directory.
func TestRendererDetectors_HoudiniUtilitiesVersionOnEveryOS(t *testing.T) {
	for _, tag := range []string{"husk", "mantra", "houdini"} {
		t.Run(tag, func(t *testing.T) {
			d := builtinDetector(t, tag)
			for _, p := range []string{
				"/opt/hfs21.0.440/bin/" + tag,
				"/Applications/Houdini/Houdini21.0.440/Frameworks/Houdini.framework",
				`C:\Program Files\Side Effects Software\Houdini 21.0.440\bin\` + tag + ".exe",
			} {
				if got := d.extractVersion(p); got != "21.0" {
					t.Errorf("extractVersion(%q) = %q, want %q", p, got, "21.0")
				}
			}
		})
	}
}

// TestRendererDetectors_RenderManReadsRMANTREE checks that RMANTREE alone tags
// the worker, with the version read from the directory name it holds.
func TestRendererDetectors_RenderManReadsRMANTREE(t *testing.T) {
	d := builtinDetector(t, "renderman")
	env := fakeEnv{goos: "linux", envs: map[string]string{"RMANTREE": "/opt/pixar/RenderManProServer-27.2"}}
	if got := strings.Join(d.Evaluate(env), ","); got != "renderman,renderman-27.2" {
		t.Errorf("Evaluate = %q, want %q", got, "renderman,renderman-27.2")
	}
}

// TestRendererDetectors_3DelightReadsDELIGHT checks that DELIGHT alone tags the
// worker, and that no version is read from it even when its value looks
// versioned.
func TestRendererDetectors_3DelightReadsDELIGHT(t *testing.T) {
	d := builtinDetector(t, "3delight")
	env := fakeEnv{goos: "linux", envs: map[string]string{"DELIGHT": "/opt/3delight-2.9.208/Linux-x86_64"}}
	if got := strings.Join(d.Evaluate(env), ","); got != "3delight" {
		t.Errorf("Evaluate = %q, want %q", got, "3delight")
	}
}

// TestRendererDetectors_EvaluateOnEveryPlatform runs Evaluate on each install's
// platform: a matching glob tags the worker (plus the versioned tag where
// expected), a Windows-only glob stays silent off Windows, nothing installed
// advertises nothing, and the command on PATH alone is enough.
func TestRendererDetectors_EvaluateOnEveryPlatform(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	goosOf := func(install string) string {
		switch {
		case strings.Contains(install, `\`):
			return "windows"
		case strings.HasPrefix(install, "/Applications/"):
			return "darwin"
		default:
			return "linux"
		}
	}
	for _, layout := range rendererLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := builtinDetector(t, layout.tag)
			for _, install := range layout.installs {
				globs := map[string][]string{}
				for _, c := range d.Checks {
					if c.PathGlob == "" {
						continue
					}
					ok, err := path.Match(norm(c.PathGlob), norm(install))
					if err != nil {
						t.Fatalf("bad pattern %q: %v", c.PathGlob, err)
					}
					if ok {
						globs[c.PathGlob] = []string{install}
					}
				}
				goos := goosOf(install)
				want := []string{layout.tag}
				if install == layout.versioned {
					want = append(want, layout.tag+"-"+layout.version)
				}
				got := d.Evaluate(fakeEnv{goos: goos, globs: globs})
				for _, w := range want {
					if !slices.Contains(got, w) {
						t.Errorf("%s install %s: Evaluate = %v, want it to include %q", goos, install, got, w)
					}
				}
				if goos == "windows" {
					if got := d.Evaluate(fakeEnv{goos: "linux", globs: globs}); len(got) != 0 {
						t.Errorf("a Windows-only glob fired on linux for %s: %v", install, got)
					}
				}
			}
			for _, goos := range []string{"linux", "darwin", "windows"} {
				if got := d.Evaluate(fakeEnv{goos: goos}); len(got) != 0 {
					t.Errorf("%s worker with nothing installed advertised %v", goos, got)
				}
				onPath := fakeEnv{goos: goos, paths: map[string]string{layout.exe: "/usr/local/bin/" + layout.exe}}
				if got := d.Evaluate(onPath); !slices.Contains(got, layout.tag) {
					t.Errorf("%s worker with %s on PATH: Evaluate = %v, want %q", goos, layout.exe, got, layout.tag)
				}
			}
		})
	}
}
