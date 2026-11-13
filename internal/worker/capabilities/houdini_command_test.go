// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"strings"
	"testing"
)

// macHoudiniBin is $HFS/bin of a macOS Houdini install, as Houdini 21.0.631's
// installer lays it out ("Current" is a link to the version folder).
const macHoudiniBin = "/Applications/Houdini/Houdini21.0.631/Frameworks/Houdini.framework/Versions/Current/Resources/bin/"

// TestHoudiniDetector_DefaultsTheHythonCommand covers the gap that left a
// worker with Houdini installed tagged but unable to run houdini-rop-render:
// no installer puts hython on PATH. The Windows executable is checked against
// a real Houdini 22.0.466 (Apprentice) and the macOS one against a real
// Houdini 21.0.631 (Apprentice); the Linux layout is $HFS/bin under
// /opt/hfs<version> and was not checked.
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

// The default command must not change what the tag says: the houdini tag keeps
// its version tag.
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
