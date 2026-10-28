// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"regexp"
	"slices"
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

func lookupIn(mapped map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		p, ok := mapped[name]
		return p, ok
	}
}

func TestApplyConfiguredCommands_AdvertisesTheTagAndVersionOfAMappedCommand(t *testing.T) {
	c := Capabilities{Tags: map[string]string{}}

	added := c.ApplyConfiguredCommands([]Detector{nukeLikeDetector()},
		lookupIn(map[string]string{"tool": "/opt/Tool17.1v2/Tool17.1"}))

	if strings.Join(added, ",") != "tool,tool_17_1" {
		t.Errorf("added = %v, want [tool tool_17_1]", added)
	}
	if c.Tags["tool"] != "true" || c.Tags["tool_17_1"] != "true" {
		t.Errorf("tags = %v, want tool and tool_17_1 set to true", c.Tags)
	}
}

func TestApplyConfiguredCommands_AddsNoVersionWhenThePathCarriesNone(t *testing.T) {
	c := Capabilities{Tags: map[string]string{}}

	added := c.ApplyConfiguredCommands([]Detector{nukeLikeDetector()},
		lookupIn(map[string]string{"tool": "/studio/bin/tool-wrapper"}))

	if strings.Join(added, ",") != "tool" {
		t.Errorf("added = %v, want just the bare tag", added)
	}
}

// A tag the operator set by hand wins: capability_tags: ["tool=false"] is how
// an operator turns a tag off, and a mapped command must not turn it back on,
// version tag included.
func TestApplyConfiguredCommands_NeverOverridesATagAlreadyPresent(t *testing.T) {
	c := Capabilities{Tags: map[string]string{"tool": "false"}}

	added := c.ApplyConfiguredCommands([]Detector{nukeLikeDetector()},
		lookupIn(map[string]string{"tool": "/opt/Tool17.1v2/Tool17.1"}))

	if len(added) != 0 || c.Tags["tool"] != "false" || len(c.Tags) != 1 {
		t.Errorf("added = %v, tags = %v; want the existing tag left alone and nothing added", added, c.Tags)
	}
}

func TestApplyConfiguredCommands_IgnoresACommandTheOperatorDidNotMap(t *testing.T) {
	c := Capabilities{Tags: map[string]string{}}

	added := c.ApplyConfiguredCommands([]Detector{nukeLikeDetector()}, lookupIn(nil))

	if len(added) != 0 || len(c.Tags) != 0 {
		t.Errorf("added = %v, tags = %v; want nothing", added, c.Tags)
	}
}

func TestApplyConfiguredCommands_IgnoresADetectorWithoutACommand(t *testing.T) {
	c := Capabilities{Tags: map[string]string{}}
	plain := Detector{Tag: "plain", Checks: []Check{{Exe: "plain"}}}

	added := c.ApplyConfiguredCommands([]Detector{plain}, lookupIn(map[string]string{"plain": "/opt/plain"}))

	if len(added) != 0 {
		t.Errorf("added = %v, want nothing for a detector with no command stanza", added)
	}
}

// An OpenJD identifier allows only letters, digits and underscores, and a tag key
// becomes the last part of attr.worker.tag.<key>, so a tag a template cannot name
// is a tag nothing can require.
func TestVersionTag_IsAlwaysAValidOpenJDIdentifier(t *testing.T) {
	valid := regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")
	cases := map[string]string{
		"17.1":      "nuke_17_1",
		"2025":      "nuke_2025",
		"10.10":     "nuke_10_10",
		"2.9.208":   "nuke_2_9_208",
		"20.0-beta": "nuke_20_0_beta",
		"4.2 LTS":   "nuke_4_2_LTS",
	}
	for version, want := range cases {
		got := versionTag("nuke", version)
		if got != want {
			t.Errorf("versionTag(nuke, %q) = %q, want %q", version, got, want)
		}
		if !valid.MatchString(got) {
			t.Errorf("versionTag(nuke, %q) = %q is not a valid identifier", version, got)
		}
	}
}

// Every tag a built-in detector can emit must be a valid identifier, version tags
// included, or a preset could never require it.
func TestBuiltinDetectors_EmitOnlyValidIdentifiers(t *testing.T) {
	valid := regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")
	ds, err := BuiltinDetectors()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if !valid.MatchString(d.Tag) {
			t.Errorf("detector tag %q is not a valid identifier", d.Tag)
		}
		if tag := versionTag(d.Tag, "1.2.3"); !valid.MatchString(tag) {
			t.Errorf("version tag %q of detector %q is not a valid identifier", tag, d.Tag)
		}
	}
}

func TestDetectorValidate_CommandArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string][]string
		wantErr string
	}{
		{"valid", map[string][]string{"linux": {"-nogui"}, "windows": {"-nogui", "-x"}}, ""},
		{"all three platforms", map[string][]string{"linux": {"a"}, "darwin": {"a"}, "windows": {"a"}}, ""},
		{"unknown os", map[string][]string{"beos": {"-nogui"}}, "beos"},
		{"empty os key", map[string][]string{"": {"-nogui"}}, "os"},
		{"empty argument", map[string][]string{"linux": {"-nogui", ""}}, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := nukeLikeDetector()
			d.Command.Args = tc.args
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

// A detector whose application needs arguments on an OS reports them even when
// no install was found, so a command already on PATH still gets them.
func TestDetectCommands_ArgsWithoutInstall(t *testing.T) {
	cfg := CapabilitiesConfig{Detect: []Detector{{
		Tag:    "sfx",
		Checks: []Check{{Exe: "sfxcmd"}},
		Command: &CommandSpec{
			Name:  "sfxcmd",
			Globs: []CommandGlob{{PathGlob: "/opt/sfx*/sfxcmd", OS: "linux"}},
			Args:  map[string][]string{"linux": {"-nogui"}},
		},
	}}}

	got, err := DetectCommands(cfg, fakeEnv{goos: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "sfxcmd" || got[0].Path != "" || !slices.Equal(got[0].Args, []string{"-nogui"}) {
		t.Errorf("DetectCommands on linux = %+v, want one entry with no path and -nogui", got)
	}

	got, err = DetectCommands(cfg, fakeEnv{goos: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Name == "sfxcmd" {
			t.Errorf("DetectCommands on windows = %+v, want no sfxcmd (no args for windows, no install)", got)
		}
	}
}

func TestDetectCommands_ArgsAccompanyAFoundInstall(t *testing.T) {
	cfg := CapabilitiesConfig{Detect: []Detector{{
		Tag:    "sfx",
		Checks: []Check{{Exe: "sfxcmd"}},
		Command: &CommandSpec{
			Name:  "sfxcmd",
			Globs: []CommandGlob{{PathGlob: "/opt/sfx*/sfxcmd", OS: "linux"}},
			Args:  map[string][]string{"linux": {"-nogui"}},
		},
	}}}
	env := fakeEnv{goos: "linux", globs: map[string][]string{"/opt/sfx*/sfxcmd": {"/opt/sfx1/sfxcmd"}}}

	got, err := DetectCommands(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/opt/sfx1/sfxcmd" || !slices.Equal(got[0].Args, []string{"-nogui"}) {
		t.Errorf("DetectCommands = %+v, want the install path and -nogui", got)
	}
}
