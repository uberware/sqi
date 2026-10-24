// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"strings"
	"testing"
)

// TestHoudiniDetector_DefaultsTheHythonCommand covers the gap that left a
// worker with Houdini installed tagged but unable to run houdini-rop-render:
// the Windows installer does not put hython on PATH. The Windows executable is
// checked against a real Houdini 22.0.466 (Apprentice); the Linux layout is
// $HFS/bin under /opt/hfs<version> and was not checked.
func TestHoudiniDetector_DefaultsTheHythonCommand(t *testing.T) {
	d := builtinDetector(t, "houdini")
	if d.Command == nil || d.Command.Name != "hython" {
		t.Fatalf("the houdini detector has no command named hython: %+v", d.Command)
	}
	norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	cases := []struct {
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
						t.Errorf("glob %q matched %s, which is not the hython executable", g.PathGlob, other)
					}
				}
			}
			got, found := d.FindCommand(env)
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
