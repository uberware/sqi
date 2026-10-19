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

// TestBlenderDetector_FindsAnInstallerInstallAndDefaultsTheCommand covers the
// gap that left a Blender installed by its MSI or winget unseen by the worker:
// neither writes the registry key the detector looked for, and neither puts
// blender on PATH. The Windows executable is checked against a real Blender 5.2;
// the macOS one follows Blender's documented bundle layout and is not.
func TestBlenderDetector_FindsAnInstallerInstallAndDefaultsTheCommand(t *testing.T) {
	d := builtinDetector(t, "blender")
	if d.Command == nil || d.Command.Name != "blender" {
		t.Fatalf("the blender detector has no command named blender: %+v", d.Command)
	}
	norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	cases := []struct {
		goos string
		exe  string
		not  []string
		want string // version
	}{
		{
			"windows", `C:\Program Files\Blender Foundation\Blender 5.2\blender.exe`,
			[]string{
				`C:\Program Files\Blender Foundation\Blender 5.2\blender-launcher.exe`,
				`C:\Program Files\Blender Foundation\Blender 5.2\BlendThumb.dll`,
			},
			"5.2",
		},
		{
			"darwin", "/Applications/Blender.app/Contents/MacOS/Blender",
			[]string{"/Applications/Blender.app/Contents/MacOS/Blender Launcher"},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			env := fakeEnv{goos: tc.goos, globs: map[string][]string{}}
			for _, g := range d.Command.Globs {
				if g.OS != "" && g.OS != tc.goos {
					continue
				}
				ok, err := path.Match(norm(g.PathGlob), norm(tc.exe))
				if err != nil {
					t.Fatalf("bad pattern %q: %v", g.PathGlob, err)
				}
				if ok {
					env.globs[g.PathGlob] = []string{tc.exe}
				}
				for _, other := range tc.not {
					matched, err := path.Match(norm(g.PathGlob), norm(other))
					if err != nil {
						t.Fatalf("bad pattern %q: %v", g.PathGlob, err)
					}
					if matched {
						t.Errorf("glob %q matched %s, which is not the Blender executable", g.PathGlob, other)
					}
				}
			}
			got, found := d.FindCommand(env)
			if !found || got.Name != "blender" || got.Path != tc.exe || got.Version != tc.want {
				t.Errorf("FindCommand = %+v (found %v), want blender %q at %s", got, found, tc.want, tc.exe)
			}
		})
	}
}

// Blender's tag has always been presence only. Giving the command a version, to
// pick the newest install, must not start emitting blender_<version> tags.
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
