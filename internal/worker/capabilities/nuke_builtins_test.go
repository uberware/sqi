// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"path"
	"slices"
	"strings"
	"testing"
)

// nukeInstalls are the default install paths of Nuke 17.1v2 on each platform,
// taken from real installs. Each one must tag the worker and yield "17.1".
var nukeInstalls = []struct {
	goos    string
	install string
}{
	{"linux", "/usr/local/Nuke17.1v2/Nuke17.1"},
	{"darwin", "/Applications/Nuke17.1v2/Nuke17.1v2.app"},
	{"windows", `C:\Program Files\Nuke17.1v2\Nuke17.1.exe`},
}

// TestNukeDetector_ProbesPATHForTheCommand checks the detector has an ungated
// exe check for "nuke", the command the Nuke presets run, so a worker that
// provides one is tagged wherever Nuke is installed.
func TestNukeDetector_ProbesPATHForTheCommand(t *testing.T) {
	d := builtinDetector(t, "nuke")
	for _, c := range d.Checks {
		if c.Exe == "nuke" && c.OS == "" {
			return
		}
	}
	t.Error("no ungated exe check for \"nuke\"")
}

// TestNukeDetector_WindowsGlobIsGated guards the backslash glob (see
// assertWindowsGlobsGated).
func TestNukeDetector_WindowsGlobIsGated(t *testing.T) {
	assertWindowsGlobsGated(t, builtinDetector(t, "nuke"))
}

// TestNukeDetector_FindsADefaultInstallOnEveryPlatform checks that the shipped
// globs match each platform's default install, that the version comes out as
// 17.1 on all of them, and that a Windows-only glob stays silent elsewhere.
// The Windows row is the one that failed on a real machine: the 17.1v2 MSI
// writes HKLM\SOFTWARE\Foundry\Nuke 17.1v2, not the key the detector once
// relied on, so only a glob finds it.
func TestNukeDetector_FindsADefaultInstallOnEveryPlatform(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	d := builtinDetector(t, "nuke")
	for _, in := range nukeInstalls {
		t.Run(in.goos, func(t *testing.T) {
			globs := map[string][]string{}
			for _, c := range d.Checks {
				if c.PathGlob == "" {
					continue
				}
				ok, err := path.Match(norm(c.PathGlob), norm(in.install))
				if err != nil {
					t.Fatalf("bad pattern %q: %v", c.PathGlob, err)
				}
				if ok {
					globs[c.PathGlob] = []string{in.install}
				}
			}
			got := d.Evaluate(fakeEnv{goos: in.goos, globs: globs})
			for _, want := range []string{"nuke", "nuke_17_1"} {
				if !slices.Contains(got, want) {
					t.Errorf("%s install %s: Evaluate = %v, want it to include %q", in.goos, in.install, got, want)
				}
			}
			if in.goos == "windows" {
				if got := d.Evaluate(fakeEnv{goos: "linux", globs: globs}); len(got) != 0 {
					t.Errorf("a Windows-only glob fired on linux for %s: %v", in.install, got)
				}
			}
		})
	}
}

// TestNukeDetector_WindowsGlobIgnoresTheInstallersHelperExecutables checks the
// Windows glob matches the Nuke executable and not the other executables the
// installer puts beside it (nukeCrashFeedback.exe, nanovdb_print.exe, ...).
func TestNukeDetector_WindowsGlobIgnoresTheInstallersHelperExecutables(t *testing.T) {
	d := builtinDetector(t, "nuke")
	for _, helper := range []string{
		`C:\Program Files\Nuke17.1v2\nukeCrashFeedback.exe`,
		`C:\Program Files\Nuke17.1v2\NukeIndieApp.ico`,
	} {
		for _, c := range d.Checks {
			if c.PathGlob == "" || c.OS != "windows" {
				continue
			}
			ok, err := path.Match(strings.ReplaceAll(c.PathGlob, `\`, "/"), strings.ReplaceAll(helper, `\`, "/"))
			if err != nil {
				t.Fatalf("bad pattern %q: %v", c.PathGlob, err)
			}
			if ok {
				t.Errorf("Windows glob %q matched %s", c.PathGlob, helper)
			}
		}
	}
}

// TestNukeDetector_NothingInstalledAdvertisesNothing checks a worker with no
// Nuke, and no nuke on PATH, gets no tag on any platform; and that nuke on PATH
// alone is enough, with no version because a bare command carries none.
func TestNukeDetector_NothingInstalledAdvertisesNothing(t *testing.T) {
	d := builtinDetector(t, "nuke")
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if got := d.Evaluate(fakeEnv{goos: goos}); len(got) != 0 {
			t.Errorf("%s worker with nothing installed advertised %v", goos, got)
		}
		onPath := fakeEnv{goos: goos, paths: map[string]string{"nuke": "/opt/studio/bin/nuke"}}
		if got := strings.Join(d.Evaluate(onPath), ","); got != "nuke" {
			t.Errorf("%s worker with nuke on PATH: Evaluate = %q, want %q", goos, got, "nuke")
		}
	}
}
