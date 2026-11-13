// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"strings"
	"testing"
)

// A command may carry its own version regex, for choosing the newest install,
// without the detector emitting version tags: Blender's tag is presence only.
func TestFindCommand_UsesTheCommandsOwnVersionRegex(t *testing.T) {
	d := Detector{
		Tag:    "tool",
		Checks: []Check{{Exe: "tool"}},
		Command: &CommandSpec{
			Name:    "tool",
			Globs:   []CommandGlob{{PathGlob: "/opt/Tool*/tool"}},
			Version: VersionSpec{From: `Tool ([0-9]+\.[0-9]+)`},
		},
	}
	env := fakeEnv{goos: "linux", globs: map[string][]string{
		"/opt/Tool*/tool": {"/opt/Tool 9.0/tool", "/opt/Tool 10.2/tool", "/opt/Tool 4.5/tool"},
	}}

	got, ok := d.FindCommand(env)

	if !ok || got.Path != "/opt/Tool 10.2/tool" || got.Version != "10.2" {
		t.Errorf("FindCommand = %+v (found %v), want the 10.2 install", got, ok)
	}
	// The detector itself has no version regex, so no version tag comes out.
	if tags := d.Evaluate(fakeEnv{goos: "linux", paths: map[string]string{"tool": "/usr/bin/tool"}}); strings.Join(tags, ",") != "tool" {
		t.Errorf("Evaluate = %v, want only the bare tag", tags)
	}
}

func TestDetectorValidate_RejectsABadCommandVersionRegex(t *testing.T) {
	d := nukeLikeDetector()
	d.Command.Version = VersionSpec{From: "(["}

	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("Validate = %v, want an error about the command's version regex", err)
	}
}

// TestBlenderDetector_FindsAnInstallerInstallAndDefaultsTheCommand checks that a
// Blender installed by its MSI or winget is tagged and supplies the blender
// command, although neither installer writes the registry key the registry check
// reads or puts blender on PATH. The Windows path matches a Blender 5.2 install;
// the macOS one follows Blender's documented bundle layout and is unverified.
func TestBlenderDetector_FindsAnInstallerInstallAndDefaultsTheCommand(t *testing.T) {
	d := builtinDetector(t, "blender")
	if d.Command == nil || d.Command.Name != "blender" {
		t.Fatalf("the blender detector has no command named blender: %+v", d.Command)
	}
	cases := []struct {
		name string
		goos string
		exe  string
		not  []string
		want string // version
	}{
		{
			"windows", "windows", `C:\Program Files\Blender Foundation\Blender 5.2\blender.exe`,
			[]string{
				`C:\Program Files\Blender Foundation\Blender 5.2\blender-launcher.exe`,
				`C:\Program Files\Blender Foundation\Blender 5.2\BlendThumb.dll`,
			},
			"5.2",
		},
		{
			"darwin", "darwin", "/Applications/Blender.app/Contents/MacOS/Blender",
			[]string{"/Applications/Blender.app/Contents/MacOS/Blender Launcher"},
			"",
		},
		{
			"darwin versioned bundle", "darwin", "/Applications/Blender 5.2.app/Contents/MacOS/Blender",
			[]string{"/Applications/Blender 5.2.app/Contents/MacOS/Blender Launcher"},
			"5.2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertCommandGlobsPick(t, d, tc.goos, tc.exe, tc.not, "the Blender executable")
			env := envWithCommandAt(d, tc.goos, tc.exe)
			got, found := d.FindCommand(env)
			if !found || got.Name != "blender" || got.Path != tc.exe || got.Version != tc.want {
				t.Errorf("FindCommand = %+v (found %v), want blender %q at %s", got, found, tc.want, tc.exe)
			}
		})
	}
}

// Blender's tag is presence only: the command's version, which picks the newest
// install, must not produce blender_<version> tags.
func TestBlenderDetector_StillEmitsNoVersionTags(t *testing.T) {
	d := builtinDetector(t, "blender")
	env := fakeEnv{goos: "windows", globs: map[string][]string{}}
	for _, c := range d.Checks {
		if c.PathGlob != "" && c.OS == "windows" {
			env.globs[c.PathGlob] = []string{`C:\Program Files\Blender Foundation\Blender 5.2\blender.exe`}
		}
	}

	if got := strings.Join(d.Evaluate(env), ","); got != "blender" {
		t.Errorf("Evaluate = %q, want only the bare blender tag", got)
	}
}

// TestBlenderDetector_PrefersAVersionedMacBundle covers a Mac with two Blenders
// side by side, the second renamed to carry its version, as Blender's own
// instructions for keeping several releases suggest. Only the bundle name
// carries a version, so the versioned bundle is chosen over a plain
// Blender.app, whatever release that one is.
func TestBlenderDetector_PrefersAVersionedMacBundle(t *testing.T) {
	d := builtinDetector(t, "blender")
	plain := "/Applications/Blender.app/Contents/MacOS/Blender"
	versioned := "/Applications/Blender 5.2.app/Contents/MacOS/Blender"
	got, found := d.FindCommand(envWithCommandAt(d, "darwin", plain, versioned))
	if !found || got.Path != versioned || got.Version != "5.2" {
		t.Errorf("FindCommand = %+v (found %v), want blender 5.2 at %s", got, found, versioned)
	}
}

// TestBlenderDetector_IgnoresOtherBundles: the macOS check matches a bundle
// that holds Blender's executable, not any bundle whose name starts with
// "Blender". A tag set for a bundle with no Blender in it would send Blender
// jobs to a worker whose command default finds nothing to run.
func TestBlenderDetector_IgnoresOtherBundles(t *testing.T) {
	d := builtinDetector(t, "blender")
	files := []string{
		"/Applications/Blender Launcher.app",
		"/Applications/Blender Launcher.app/Contents/MacOS/Blender Launcher",
	}
	env := fakeEnv{goos: "darwin", globs: map[string][]string{}}
	for _, c := range d.Checks {
		if c.PathGlob == "" || (c.OS != "" && c.OS != "darwin") {
			continue
		}
		for _, f := range files {
			if ok, err := path.Match(c.PathGlob, f); err == nil && ok {
				env.globs[c.PathGlob] = append(env.globs[c.PathGlob], f)
			}
		}
	}

	if got := d.Evaluate(env); len(got) != 0 {
		t.Errorf("Evaluate = %q, want no tag for a bundle that holds no Blender", got)
	}
}
