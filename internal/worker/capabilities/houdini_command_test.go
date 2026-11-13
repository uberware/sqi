// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"strings"
	"testing"
)

// macHoudiniBin is $HFS/bin of a macOS Houdini install, as Houdini 21.0.631's
// installer lays it out ("Current" is a link to the version folder).
const macHoudiniBin = "/Applications/Houdini/Houdini21.0.631/Frameworks/Houdini.framework/Versions/Current/Resources/bin/"

// TestHoudiniDetector_DefaultsTheHythonCommand checks that the houdini detector
// supplies the hython command houdini-rop-render runs, which no installer puts
// on PATH. The Windows path matches a Houdini 22.0.466 (Apprentice) install and
// the macOS path a Houdini 21.0.631 (Apprentice) install; the Linux path,
// $HFS/bin under /opt/hfs<version>, is unverified.
func TestHoudiniDetector_DefaultsTheHythonCommand(t *testing.T) {
	d := builtinDetector(t, "houdini")
	if d.Command == nil || d.Command.Name != "hython" {
		t.Fatalf("the houdini detector has no command named hython: %+v", d.Command)
	}
	for _, tc := range []struct {
		goos string
		exe  string
		not  []string
		want string // version
	}{
		{
			"windows", `C:\Program Files\Side Effects Software\Houdini 22.0.466\bin\hython.exe`,
			[]string{
				`C:\Program Files\Side Effects Software\Houdini 22.0.466\bin\hython3.13.exe`,
				`C:\Program Files\Side Effects Software\Houdini 22.0.466\bin\houdini.exe`,
			},
			"22.0",
		},
		{
			"linux", "/opt/hfs21.0.440/bin/hython",
			[]string{"/opt/hfs21.0.440/bin/houdini"},
			"21.0",
		},
		{
			"darwin", macHoudiniBin + "hython",
			[]string{
				macHoudiniBin + "hython3.11",
				"/Applications/Houdini/Current/Frameworks/Houdini.framework/Versions/Current/Resources/bin/hython",
			},
			"21.0",
		},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			assertCommandGlobsPick(t, d, tc.goos, tc.exe, tc.not, "the hython executable")
			got, found := d.FindCommand(envWithCommandAt(d, tc.goos, tc.exe))
			if !found || got.Name != "hython" || got.Path != tc.exe || got.Version != tc.want {
				t.Errorf("FindCommand = %+v (found %v), want hython %q at %s", got, found, tc.want, tc.exe)
			}
		})
	}
}

// The command default does not change the tags: the houdini detector emits its
// version tag beside the bare tag.
func TestHoudiniDetector_CommandLeavesTagsAlone(t *testing.T) {
	d := builtinDetector(t, "houdini")
	env := fakeEnv{goos: "windows", globs: map[string][]string{}}
	for _, c := range d.Checks {
		if c.PathGlob != "" && c.OS == "windows" {
			env.globs[c.PathGlob] = []string{`C:\Program Files\Side Effects Software\Houdini 22.0.466\bin\houdini.exe`}
		}
	}

	if got := strings.Join(d.Evaluate(env), ","); got != "houdini,houdini_22_0" {
		t.Errorf("Evaluate = %q, want houdini,houdini_22_0", got)
	}
}
