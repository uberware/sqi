// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"strings"
	"testing"
)

func nukeLikeDetector() Detector {
	return Detector{
		Tag:    "tool",
		Checks: []Check{{Exe: "tool"}},
		Command: &CommandSpec{
			Name: "tool",
			Globs: []CommandGlob{
				{PathGlob: "/opt/Tool*/Tool[0-9]*", OS: "linux"},
				{PathGlob: "C:/Program Files/Tool*/Tool[0-9]*.exe", OS: "windows"},
			},
		},
		Version: VersionSpec{From: `Tool([0-9]+\.[0-9]+)`},
	}
}

func TestFindCommand_PicksTheNewestVersionNumerically(t *testing.T) {
	d := nukeLikeDetector()
	env := fakeEnv{goos: "linux", globs: map[string][]string{
		"/opt/Tool*/Tool[0-9]*": {"/opt/Tool9.0v1/Tool9.0", "/opt/Tool16.0v2/Tool16.0", "/opt/Tool15.5v1/Tool15.5"},
	}}

	got, ok := d.FindCommand(env)

	if !ok {
		t.Fatal("FindCommand found nothing")
	}
	// 16.0 is newer than 15.5 and 9.0 — a string comparison would pick 9.0.
	if got.Path != "/opt/Tool16.0v2/Tool16.0" || got.Version != "16.0" || got.Name != "tool" {
		t.Errorf("FindCommand = %+v, want tool 16.0 at /opt/Tool16.0v2/Tool16.0", got)
	}
}

func TestFindCommand_AReleaseOfTheSameVersionPicksTheLaterDirectory(t *testing.T) {
	d := nukeLikeDetector()
	env := fakeEnv{goos: "linux", globs: map[string][]string{
		"/opt/Tool*/Tool[0-9]*": {"/opt/Tool17.1v1/Tool17.1", "/opt/Tool17.1v2/Tool17.1"},
	}}

	got, ok := d.FindCommand(env)

	if !ok || got.Path != "/opt/Tool17.1v2/Tool17.1" {
		t.Errorf("FindCommand = %+v (found %v), want the v2 install", got, ok)
	}
}

func TestFindCommand_IgnoresGlobsForOtherPlatforms(t *testing.T) {
	d := nukeLikeDetector()
	env := fakeEnv{goos: "linux", globs: map[string][]string{
		"C:/Program Files/Tool*/Tool[0-9]*.exe": {"C:/Program Files/Tool17.1v2/Tool17.1.exe"},
	}}

	if got, ok := d.FindCommand(env); ok {
		t.Errorf("a Windows glob produced %+v on linux", got)
	}
}

func TestFindCommand_NothingInstalledFindsNothing(t *testing.T) {
	if got, ok := nukeLikeDetector().FindCommand(fakeEnv{goos: "linux"}); ok {
		t.Errorf("FindCommand = %+v with nothing installed", got)
	}
}

func TestFindCommand_ADetectorWithoutACommandFindsNothing(t *testing.T) {
	d := Detector{Tag: "x", Checks: []Check{{Exe: "x"}}}
	if _, ok := d.FindCommand(fakeEnv{goos: "linux"}); ok {
		t.Error("a detector with no command stanza reported a command")
	}
}

func TestFindCommand_AnUnparsableVersionStillCountsAsOlderThanAParsedOne(t *testing.T) {
	d := nukeLikeDetector()
	env := fakeEnv{goos: "linux", globs: map[string][]string{
		"/opt/Tool*/Tool[0-9]*": {"/opt/ToolWeird/Tool1", "/opt/Tool1.0v1/Tool1.0"},
	}}

	got, ok := d.FindCommand(env)

	if !ok || got.Path != "/opt/Tool1.0v1/Tool1.0" {
		t.Errorf("FindCommand = %+v (found %v), want the install with a parseable version", got, ok)
	}
}

func TestDetectorValidate_Command(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Detector)
		wantErr string
	}{
		{"valid", func(*Detector) {}, ""},
		{"no name", func(d *Detector) { d.Command.Name = "" }, "name"},
		{"name is a path", func(d *Detector) { d.Command.Name = "bin/tool" }, "name"},
		{"no globs", func(d *Detector) { d.Command.Globs = nil }, "glob"},
		{"empty glob", func(d *Detector) { d.Command.Globs[0].PathGlob = "" }, "path_glob"},
		{"bad os", func(d *Detector) { d.Command.Globs[0].OS = "beos" }, "os"},
		{"bad glob syntax", func(d *Detector) { d.Command.Globs[0].PathGlob = "/opt/[" }, "path_glob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := nukeLikeDetector()
			tc.mutate(&d)
			err := d.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate = %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// TestNukeDetector_CommandFindsTheExecutableOnEveryPlatform checks the shipped
// command globs against the default install of Nuke 17.1v2 on each platform,
// laid out as Foundry documents it (Linux: /usr/local/Nuke17.1v2/Nuke17.1;
// macOS: inside the .app bundle, Contents/MacOS/Nuke17.1) and as the Windows
// MSI installs it. They must pick the executable and not the files beside it.
func TestNukeDetector_CommandFindsTheExecutableOnEveryPlatform(t *testing.T) {
	d := builtinDetector(t, "nuke")
	if d.Command == nil || d.Command.Name != "nuke" {
		t.Fatalf("the nuke detector has no command named nuke: %+v", d.Command)
	}
	norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	cases := []struct {
		goos string
		exe  string
		not  []string
	}{
		{"linux", "/usr/local/Nuke17.1v2/Nuke17.1", []string{"/usr/local/Nuke17.1v2/Nuke17.1v2", "/usr/local/Nuke17.1v2/NukeAssist"}},
		{"darwin", "/Applications/Nuke17.1v2/Nuke17.1v2.app/Contents/MacOS/Nuke17.1", []string{"/Applications/Nuke17.1v2/Nuke17.1v2.app/Contents/MacOS/crashpad_handler"}},
		{"windows", `C:\Program Files\Nuke17.1v2\Nuke17.1.exe`, []string{`C:\Program Files\Nuke17.1v2\nukeCrashFeedback.exe`, `C:\Program Files\Nuke17.1v2\py.exe`}},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			matches := func(file string) (matched bool, glob string) {
				for _, g := range d.Command.Globs {
					if g.OS != "" && g.OS != tc.goos {
						continue
					}
					ok, err := path.Match(norm(g.PathGlob), norm(file))
					if err != nil {
						t.Fatalf("bad pattern %q: %v", g.PathGlob, err)
					}
					if ok {
						return true, g.PathGlob
					}
				}
				return false, ""
			}
			if ok, _ := matches(tc.exe); !ok {
				t.Errorf("no %s command glob matches the executable %s", tc.goos, tc.exe)
			}
			for _, other := range tc.not {
				if ok, g := matches(other); ok {
					t.Errorf("%s glob %q matched %s, which is not the Nuke executable", tc.goos, g, other)
				}
			}
			env := fakeEnv{goos: tc.goos, globs: map[string][]string{}}
			for _, g := range d.Command.Globs {
				env.globs[g.PathGlob] = []string{tc.exe}
			}
			got, ok := d.FindCommand(env)
			if !ok || got.Name != "nuke" || got.Path != tc.exe || got.Version != "17.1" {
				t.Errorf("FindCommand = %+v (found %v), want nuke 17.1 at %s", got, ok, tc.exe)
			}
		})
	}
}

func TestDetectCommands_ComesFromBuiltinsAndCustomDetectorsAndHonoursDisable(t *testing.T) {
	custom := Detector{
		Tag: "inhouse", Checks: []Check{{Exe: "inhouse"}},
		Command: &CommandSpec{Name: "inhouse", Globs: []CommandGlob{{PathGlob: "/opt/inhouse/bin/inhouse"}}},
	}
	env := fakeEnv{goos: "linux", globs: map[string][]string{
		"/opt/inhouse/bin/inhouse": {"/opt/inhouse/bin/inhouse"},
	}}
	for _, g := range builtinDetector(t, "nuke").Command.Globs {
		env.globs[g.PathGlob] = []string{"/usr/local/Nuke17.1v2/Nuke17.1"}
	}

	got, err := DetectCommands(CapabilitiesConfig{Detect: []Detector{custom}}, env)
	if err != nil {
		t.Fatalf("DetectCommands: %v", err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "inhouse,nuke" {
		t.Errorf("commands = %v, want [inhouse nuke] sorted by name", names)
	}

	got, err = DetectCommands(CapabilitiesConfig{Disable: []string{"nuke"}}, env)
	if err != nil {
		t.Fatalf("DetectCommands: %v", err)
	}
	for _, c := range got {
		if c.Name == "nuke" {
			t.Errorf("a disabled detector still produced its command: %+v", c)
		}
	}
}
