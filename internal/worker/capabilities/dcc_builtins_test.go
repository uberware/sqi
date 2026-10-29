// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

// The DCC detectors pair with the presets in presets/sqi/ that drive a DCC's
// own command-line renderer. They share rendererLayout and the
// TestRendererDetectors_* tests in renderer_builtins_test.go, which check
// detectorLayouts; a layout with an empty exe asserts the detector has no exe
// check, for a command too generic to probe PATH for.
//
// dccLayouts lists each detector's expected install paths independently of its
// YAML, so the tests check the shipped globs against them.
var dccLayouts = []rendererLayout{
	{
		// Maya has no exe check: its command-line renderer is Render, which is
		// too generic to probe PATH for. The Windows glob is the Autodesk
		// installer's default location. It is unverified: the Windows rows for
		// Maya and Houdini were added without a machine to check them on.
		tag: "maya",
		exe: "",
		installs: []string{
			"/usr/autodesk/maya2026/bin/maya",
			"/Applications/Autodesk/maya2026/Maya.app",
			`C:\Program Files\Autodesk\Maya2026\bin\maya.exe`,
		},
		versioned: `C:\Program Files\Autodesk\Maya2026\bin\maya.exe`,
		version:   "2026",
	},
	{
		// The Windows glob is the folder the husk and mantra detectors already
		// assume, with the same version format.
		tag: "houdini",
		exe: "hython",
		installs: []string{
			"/opt/hfs21.0.440/bin/houdini",
			"/Applications/Houdini/Houdini21.0.440/Frameworks/Houdini.framework",
			`C:\Program Files\Side Effects Software\Houdini 21.0.440\bin\houdini.exe`,
		},
		versioned: `C:\Program Files\Side Effects Software\Houdini 21.0.440\bin\houdini.exe`,
		version:   "21.0",
	},
	{
		// Blender carries no version tags (presence only), so there is no
		// versioned path. The Windows glob is the folder the MSI and winget
		// install to, checked against a real Blender 5.2; neither writes the
		// registry key the detector looked for.
		tag: "blender",
		exe: "blender",
		installs: []string{
			"/Applications/Blender.app",
			`C:\Program Files\Blender Foundation\Blender 5.2\blender.exe`,
		},
	},
	{
		// Commandline is generic enough to collide with unrelated software,
		// so there is no exe check, as in mistikavr.yaml. The Linux folder
		// name does not match the version regex, so only the Windows path
		// is checked for a version.
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
		// 3ds Max is Windows-only. The registry key carries no year, so
		// only the install path yields a version.
		tag: "threedsmax",
		exe: "3dsmaxcmd",
		installs: []string{
			`C:\Program Files\Autodesk\3ds Max 2026\3dsmaxcmd.exe`,
		},
		versioned: `C:\Program Files\Autodesk\3ds Max 2026\3dsmaxcmd.exe`,
		version:   "2026",
	},
	{
		// aerender ships inside the application folder on both platforms.
		// The versioned folder names are unverified (see aftereffects.yaml).
		// The Windows path stands for the version check.
		tag: "aftereffects",
		exe: "aerender",
		installs: []string{
			`C:\Program Files\Adobe\Adobe After Effects 2026\Support Files\aerender.exe`,
			"/Applications/Adobe After Effects 2026/aerender",
		},
		versioned: `C:\Program Files\Adobe\Adobe After Effects 2026\Support Files\aerender.exe`,
		version:   "2026",
	},
	{
		// Katana runs on Linux and Windows only. Every path is unverified (see
		// katana.yaml): the two Linux layouts are the installer's documented
		// example and Deadline's default, the Windows one the design's reading
		// of the default. The version is read from the install folder, in either
		// letter case.
		tag: "katana",
		exe: "katana",
		installs: []string{
			"/opt/Foundry/Katana7.0v1/katana",
			"/opt/foundry/katana7.0v1/katana",
			`C:\Program Files\Katana7.0v1\bin\katanaBin.exe`,
		},
		versioned: "/opt/foundry/katana7.0v1/katana",
		version:   "7.0v1",
	},
	{
		// Fusion Render Node ships inside Fusion Studio on all three platforms.
		// Every path is the pattern the design inferred for Fusion 18 to 21 and
		// is unverified (see fusion.yaml). The version is read from the install
		// folder, whose name differs per platform; the Windows path stands for
		// the version check.
		tag: "fusion",
		exe: "FusionRenderNode",
		installs: []string{
			`C:\Program Files\Blackmagic Design\Fusion Render Node 19\FusionRenderNode.exe`,
			"/Applications/Blackmagic Fusion 19 Render Node/Fusion Render Node.app/Contents/MacOS/Fusion Render Node",
			"/opt/BlackmagicDesign/FusionRenderNode19/FusionRenderNode",
		},
		versioned: `C:\Program Files\Blackmagic Design\Fusion Render Node 19\FusionRenderNode.exe`,
		version:   "19",
	},
	{
		// Silhouette's sfxcmd runs on all three platforms. The Windows and macOS
		// folders are the ones Boris FX's Command-Line page documents
		// (SilhouetteFX) and the BorisFX root is the newer layout the design
		// lists; neither is observed (see silhouette.yaml). The macOS path is the
		// sfxcmd inside the .app bundle. The version is the year in the folder.
		tag: "silhouette",
		exe: "sfxcmd",
		installs: []string{
			`C:\Program Files\BorisFX\Silhouette 2026\sfxcmd.exe`,
			`C:\Program Files\SilhouetteFX\Silhouette 2026\sfxcmd.exe`,
			"/Applications/BorisFX/Silhouette 2026/Silhouette.app/Contents/MacOS/sfxcmd",
			"/Applications/SilhouetteFX/Silhouette 2026/Silhouette.app/Contents/MacOS/sfxcmd",
			"/opt/BorisFX/Silhouette 2026/sfxcmd",
			"/opt/SilhouetteFX/Silhouette 2026/sfxcmd",
		},
		versioned: `C:\Program Files\SilhouetteFX\Silhouette 2026\sfxcmd.exe`,
		version:   "2026",
	},
	{
		// VRED Core runs on Windows and Linux, VRED Pro on Windows. The Windows
		// VRED Core folder is the one Autodesk's VRED Core page gives; the VRED
		// Pro folder and both Linux roots are unverified (see vred.yaml). The
		// version is VRED's internal one (18.0 is VRED 2026).
		tag: "vred",
		exe: "VREDCore",
		installs: []string{
			`C:\Program Files\Autodesk\VREDCore-18.0\bin\WIN64\VREDCore.exe`,
			`C:\Program Files\Autodesk\VREDPro-18.0\bin\WIN64\VREDPro.exe`,
			"/opt/Autodesk/VREDCore-18.0/bin/VREDCore",
			"/opt/Autodesk/VREDCluster-18.0/bin/VREDCore",
		},
		versioned: `C:\Program Files\Autodesk\VREDCore-18.0\bin\WIN64\VREDCore.exe`,
		version:   "18.0",
	},
	{
		// KeyShot has Windows and macOS desktop builds. Neither layout is
		// verified (see keyshot.yaml); the version is the number in the install
		// folder, which is KeyShot12 for 12 and a year from 2023 on.
		tag: "keyshot",
		exe: "keyshot_headless",
		installs: []string{
			`C:\Program Files\KeyShot12\bin\keyshot_headless.exe`,
			`C:\Program Files\KeyShot 2024\bin\keyshot_headless.exe`,
			"/Applications/KeyShot12.app/Contents/MacOS/keyshot",
			"/Applications/KeyShot12.app/Contents/MacOS/keyshot_headless",
		},
		versioned: `C:\Program Files\KeyShot12\bin\keyshot_headless.exe`,
		version:   "12",
	},
	{
		// Modo is discontinued (17.1 final). Foundry documents two Windows
		// layouts (Modo<ver> for 17.1, Foundry\Modo\<ver> for 13.2) and the macOS
		// bundle's modo_cl; Linux has no fixed path, only PATH (see modo.yaml).
		// The version is Modo's own 17.1v1 form.
		tag: "modo",
		exe: "modo_cl",
		installs: []string{
			`C:\Program Files\Modo17.1v1\modo_cl.exe`,
			`C:\Program Files\Foundry\Modo\13.2v1\modo_cl.exe`,
			"/Applications/Modo17.1v1.app/Contents/MacOS/modo_cl",
		},
		versioned: `C:\Program Files\Modo17.1v1\modo_cl.exe`,
		version:   "17.1v1",
	},
}
