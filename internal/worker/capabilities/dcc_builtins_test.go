// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import "testing"

// The DCC detectors pair with the presets in presets/sqi/ that drive a DCC's
// own command-line renderer. They share rendererLayout and the check helpers
// in renderer_builtins_test.go; a layout with an empty exe asserts the detector
// has no exe check, for a command too generic to probe PATH for.
//
// dccLayouts lists each detector's expected install paths independently of its
// YAML, so the tests check the shipped globs against them.
var dccLayouts = []rendererLayout{
	{
		// Commandline is generic enough to collide with unrelated software,
		// so there is no exe check (the mistikavr.yaml rule). The Linux
		// folder carries no year the version regex reads. Only the Windows
		// path is asserted for a version (versioned holds one path).
		tag: "cinema4d",
		exe: "",
		installs: []string{
			`C:\Program Files\Maxon Cinema 4D 2026\Commandline.exe`,
			"/Applications/Maxon Cinema 4D 2026/Commandline.app",
			"/opt/maxon/cinema4dr2026.000/bin/Commandline",
		},
		versioned: `C:\Program Files\Maxon Cinema 4D 2026\Commandline.exe`,
		version:   "2026",
	},
	{
		// 3ds Max is Windows-only. The registry key carries the product
		// version (2025 = 27.0) rather than the year, so only the install
		// path yields a version.
		tag: "3dsmax",
		exe: "3dsmaxcmd",
		installs: []string{
			`C:\Program Files\Autodesk\3ds Max 2026\3dsmaxcmd.exe`,
		},
		versioned: `C:\Program Files\Autodesk\3ds Max 2026\3dsmaxcmd.exe`,
		version:   "2026",
	},
	{
		// aerender ships inside the application folder on both platforms;
		// the versioned folder names are unverified (Adobe's page still
		// says "CC"). The Windows path stands for the version check.
		tag: "aftereffects",
		exe: "aerender",
		installs: []string{
			`C:\Program Files\Adobe\Adobe After Effects 2026\Support Files\aerender.exe`,
			"/Applications/Adobe After Effects 2026/aerender",
		},
		versioned: `C:\Program Files\Adobe\Adobe After Effects 2026\Support Files\aerender.exe`,
		version:   "2026",
	},
}

// TestDCCDetectors_MatchVendorInstallLayout checks that a shipped glob matches
// every install path in dccLayouts.
func TestDCCDetectors_MatchVendorInstallLayout(t *testing.T) {
	checkInstallLayout(t, dccLayouts)
}

// TestDCCDetectors_ProbePATH checks that each detector probes PATH for the
// command its preset runs, or has no exe check when the command is too generic.
func TestDCCDetectors_ProbePATH(t *testing.T) {
	checkPATHProbe(t, dccLayouts)
}

// TestDCCDetectors_WindowsCheckIsGated guards the backslash globs.
func TestDCCDetectors_WindowsCheckIsGated(t *testing.T) {
	checkWindowsGated(t, dccLayouts)
}

// TestDCCDetectors_ExtractVersion checks the version extracted from a versioned
// install path, and that a detector with no version extracts none.
func TestDCCDetectors_ExtractVersion(t *testing.T) {
	checkVersion(t, dccLayouts)
}

// TestDCCDetectors_EvaluateOnEveryPlatform runs Evaluate on each install's
// platform.
func TestDCCDetectors_EvaluateOnEveryPlatform(t *testing.T) {
	checkEvaluateEveryPlatform(t, dccLayouts)
}
