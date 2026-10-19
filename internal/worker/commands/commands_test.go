// SPDX-License-Identifier: AGPL-3.0-or-later

package commands

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestResolve_MapsANamedCommandToItsPathAndLeadingArgs(t *testing.T) {
	r := newResolver(Table{
		"nuke": {Path: "/opt/Nuke17.1v2/Nuke17.1", Args: []string{"--nc"}},
	}, "linux")

	path, args := r.Resolve("nuke", []string{"-x", "-X", "Write1", "-F", "1-4", "scene.nknc"})

	if path != "/opt/Nuke17.1v2/Nuke17.1" {
		t.Errorf("path = %q, want the mapped path", path)
	}
	want := []string{"--nc", "-x", "-X", "Write1", "-F", "1-4", "scene.nknc"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want the mapped args first, then the job's: %q", args, want)
	}
}

func TestResolve_LeavesAnUnmappedCommandAlone(t *testing.T) {
	r := newResolver(Table{"nuke": {Path: "/opt/nuke"}}, "linux")
	in := []string{"-c", "echo hi"}

	path, args := r.Resolve("sh", in)

	if path != "sh" || !slices.Equal(args, in) {
		t.Errorf("Resolve(sh) = %q %q, want it unchanged", path, args)
	}
}

func TestResolve_NilAndEmptyResolversChangeNothing(t *testing.T) {
	for name, r := range map[string]*Resolver{"nil": nil, "empty": NewResolver(nil)} {
		t.Run(name, func(t *testing.T) {
			path, args := r.Resolve("nuke", []string{"a"})
			if path != "nuke" || !slices.Equal(args, []string{"a"}) {
				t.Errorf("Resolve = %q %q, want it unchanged", path, args)
			}
		})
	}
}

// A job that names a path is naming a file, not asking for "the nuke command",
// so a mapping for "nuke" must not capture it.
func TestResolve_DoesNotCaptureACommandThatIsAPath(t *testing.T) {
	r := newResolver(Table{"nuke": {Path: "/opt/nuke"}}, "linux")
	for _, command := range []string{"/usr/bin/nuke", "./nuke", "bin/nuke"} {
		if path, _ := r.Resolve(command, nil); path != command {
			t.Errorf("Resolve(%q) = %q, want it unchanged", command, path)
		}
	}
}

func TestResolve_NamesAreCaseSensitiveOnPOSIXAndInsensitiveOnWindows(t *testing.T) {
	table := Table{"nuke": {Path: "/opt/nuke"}}

	if path, _ := newResolver(table, "linux").Resolve("Nuke", nil); path != "Nuke" {
		t.Errorf("linux Resolve(Nuke) = %q, want it unchanged", path)
	}
	if path, _ := newResolver(table, "darwin").Resolve("NUKE", nil); path != "NUKE" {
		t.Errorf("darwin Resolve(NUKE) = %q, want it unchanged", path)
	}
	if path, _ := newResolver(table, "windows").Resolve("Nuke", nil); path != "/opt/nuke" {
		t.Errorf("windows Resolve(Nuke) = %q, want the mapped path", path)
	}
}

// Resolve must hand back a fresh slice: the caller appends to and keeps its
// own, and the table's args are shared by every task.
func TestResolve_DoesNotAliasTheTableOrTheCallersArgs(t *testing.T) {
	table := Table{"nuke": {Path: "/opt/nuke", Args: make([]string, 1, 8)}}
	table["nuke"].Args[0] = "--nc"
	r := newResolver(table, "linux")

	_, first := r.Resolve("nuke", []string{"a"})
	_, second := r.Resolve("nuke", []string{"b"})
	first[0] = "mutated"

	if second[1] != "b" || second[0] != "--nc" {
		t.Errorf("second = %q, want [--nc b]", second)
	}
	if table["nuke"].Args[0] != "--nc" {
		t.Errorf("table args mutated: %q", table["nuke"].Args)
	}
}

func TestValidate_AcceptsAnAbsolutePathAndABareName(t *testing.T) {
	abs := hostAbs(t, "nuke")
	problems := Table{
		"nuke":     {Path: abs, Args: []string{"--nc"}},
		"nuke-alt": {Path: "nuke17"}, // resolved through the worker's PATH
	}.Validate()
	if len(problems) != 0 {
		t.Errorf("Validate = %v, want none", problems)
	}
}

func TestValidate_RejectsBadEntries(t *testing.T) {
	ok := hostAbs(t, "nuke") // a valid path, so each case has exactly one fault
	type badCase struct {
		name  string
		table Table
		key   string
		want  string
	}
	cases := []badCase{
		{"empty name", Table{"": {Path: ok}}, "", "name is empty"},
		{"name with a slash", Table{"bin/nuke": {Path: ok}}, "bin/nuke", "path separator"},
		{"padded name", Table{" nuke": {Path: ok}}, " nuke", "whitespace"},
		{"empty path", Table{"nuke": {}}, "nuke", "path is required"},
		{"relative path", Table{"nuke": {Path: "bin/nuke"}}, "nuke", "relative"},
	}
	if runtime.GOOS == "windows" {
		// A backslash separates path components on Windows only.
		backslashed := "bin" + string(filepath.Separator) + "nuke"
		cases = append(cases, badCase{"name with a backslash", Table{backslashed: {Path: ok}}, backslashed, "path separator"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := tc.table.Validate()
			if len(problems) != 1 {
				t.Fatalf("Validate = %v, want exactly one problem", problems)
			}
			if problems[0].Key != tc.key {
				t.Errorf("problem key = %q, want %q", problems[0].Key, tc.key)
			}
			if !strings.Contains(problems[0].Message, tc.want) {
				t.Errorf("message %q does not mention %q", problems[0].Message, tc.want)
			}
		})
	}
}

// hostAbs returns an absolute path valid on this host.
func hostAbs(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestValidate_ReportsProblemsInAStableOrder(t *testing.T) {
	table := Table{"b": {}, "a": {}, "c": {}}
	for range 20 {
		var keys []string
		for _, p := range table.Validate() {
			keys = append(keys, p.Key)
		}
		if !slices.Equal(keys, []string{"a", "b", "c"}) {
			t.Fatalf("keys = %v, want [a b c]", keys)
		}
	}
}

func TestContains_UsesTheHostsNameRules(t *testing.T) {
	table := Table{"nuke": {Path: "/opt/nuke"}}

	if !table.contains("nuke", "linux") {
		t.Error("linux: the exact name was not found")
	}
	if table.contains("Nuke", "linux") {
		t.Error("linux: names are case-sensitive")
	}
	if !table.contains("Nuke", "windows") {
		t.Error("windows: names are case-insensitive")
	}
	if table.contains("blender", "windows") {
		t.Error("windows: an absent name was found")
	}
}

func TestLookup_ReturnsTheEntryByTheHostsNameRules(t *testing.T) {
	table := Table{"nuke": {Path: "/opt/nuke", Args: []string{"--nc"}}}

	got, ok := table.lookup("nuke", "linux")
	if !ok || got.Path != "/opt/nuke" || len(got.Args) != 1 {
		t.Errorf("linux lookup(nuke) = %+v, %v", got, ok)
	}
	if _, ok := table.lookup("Nuke", "linux"); ok {
		t.Error("linux: names are case-sensitive")
	}
	if got, ok := table.lookup("NUKE", "windows"); !ok || got.Path != "/opt/nuke" {
		t.Errorf("windows lookup(NUKE) = %+v, %v; want the entry", got, ok)
	}
}
