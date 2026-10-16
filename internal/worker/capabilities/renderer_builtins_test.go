// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"strings"
	"testing"
)

// The standalone-renderer detectors pair with the presets in presets/sqi/ that
// invoke a renderer directly rather than through a DCC. Each one probes PATH
// for the exact command its preset runs, and where the vendor documents a fixed
// install location, a path_glob for it too.
//
// This table restates the vendor install layouts (sources are cited in each
// detector's YAML), so the tests below check the shipped globs against the
// documented layout rather than against a copy of themselves.
type rendererLayout struct {
	tag string
	// exe is the bare command the paired preset invokes; the detector must
	// probe PATH for it.
	exe string
	// installs are vendor install paths some path_glob check must match.
	installs []string
	// versioned is a path carrying a version and version the version the
	// detector must extract from it; both empty when no version is expected.
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
}

// builtinDetector returns the built-in detector emitting tag.
func builtinDetector(t *testing.T, tag string) Detector {
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

// TestRendererDetectors_MatchVendorInstallLayout checks every documented
// install path against the shipped globs, with path.Match on slash-normalized
// strings for the reason TestMistikaDetectors_MatchVendorInstallLayout gives.
func TestRendererDetectors_MatchVendorInstallLayout(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	for _, layout := range rendererLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := builtinDetector(t, layout.tag)
			for _, install := range layout.installs {
				var matched bool
				for _, c := range d.Checks {
					if c.PathGlob == "" {
						continue
					}
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

// TestRendererDetectors_ProbePATHForTheCommand pins the PATH probe: a worker
// with the renderer on PATH but installed somewhere no glob knows must still be
// tagged, because the preset runs the bare command.
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

// TestRendererDetectors_WindowsCheckIsGated guards the backslash globs, which
// are ErrBadPattern (silently no matches) on POSIX when ungated.
func TestRendererDetectors_WindowsCheckIsGated(t *testing.T) {
	for _, layout := range rendererLayouts {
		t.Run(layout.tag, func(t *testing.T) {
			d := builtinDetector(t, layout.tag)
			for i, c := range d.Checks {
				if strings.Contains(c.PathGlob, `\`) && c.OS != "windows" {
					t.Errorf("check %d has a backslash pattern %q but os = %q, want %q",
						i, c.PathGlob, c.OS, "windows")
				}
			}
		})
	}
}

// TestRendererDetectors_ExtractVersion checks the version regex against a
// versioned install path, and that a detector expected to emit no version
// extracts none from any of its install paths.
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

// TestRendererDetectors_HoudiniUtilitiesVersionOnEveryOS checks that the
// detectors for the renderers Houdini installs in $HFS/bin extract the same
// Houdini version from the Linux and Windows install layouts, which spell it
// differently (hfs21.0.440 against "Houdini 21.0.440").
func TestRendererDetectors_HoudiniUtilitiesVersionOnEveryOS(t *testing.T) {
	for _, tag := range []string{"husk"} {
		t.Run(tag, func(t *testing.T) {
			d := builtinDetector(t, tag)
			for _, p := range []string{
				"/opt/hfs21.0.440/bin/" + tag,
				`C:\Program Files\Side Effects Software\Houdini 21.0.440\bin\` + tag + ".exe",
			} {
				if got := d.extractVersion(p); got != "21.0" {
					t.Errorf("extractVersion(%q) = %q, want %q", p, got, "21.0")
				}
			}
		})
	}
}
