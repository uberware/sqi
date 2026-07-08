// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDetector_UnmarshalAndValidate(t *testing.T) {
	const y = `
tag: houdini
checks:
  - exe: hython
  - path_glob: "/opt/hfs*/bin/houdini"
  - env: HFS
  - env: { name: HOUDINI_ROOT, matches: "hfs[0-9.]+" }
  - registry: 'HKLM\SOFTWARE\Side Effects Software\Houdini'
    os: windows
version:
  from: "hfs(?P<v>[0-9.]+)"
`
	var d Detector
	if err := yaml.Unmarshal([]byte(y), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Tag != "houdini" || len(d.Checks) != 5 {
		t.Fatalf("parsed detector wrong: %+v", d)
	}
	if d.Checks[2].Env.Name != "HFS" {
		t.Errorf("bare env form: got %q, want HFS", d.Checks[2].Env.Name)
	}
	if d.Checks[3].Env.Name != "HOUDINI_ROOT" || d.Checks[3].Env.Matches != "hfs[0-9.]+" {
		t.Errorf("map env form parsed wrong: %+v", d.Checks[3].Env)
	}
	if err := d.Validate(); err != nil {
		t.Errorf("valid detector rejected: %v", err)
	}
}

func TestDetector_Validate_Rejects(t *testing.T) {
	cases := map[string]Detector{
		"no tag":        {Checks: []Check{{Exe: "x"}}},
		"no checks":     {Tag: "x"},
		"empty check":   {Tag: "x", Checks: []Check{{}}},
		"two-in-check":  {Tag: "x", Checks: []Check{{Exe: "a", PathGlob: "b"}}},
		"bad version":   {Tag: "x", Checks: []Check{{Exe: "a"}}, Version: VersionSpec{From: "("}},
		"bad os":        {Tag: "x", Checks: []Check{{Exe: "a", OS: "solaris"}}},
		"bad env regex": {Tag: "x", Checks: []Check{{Env: EnvCheck{Name: "A", Matches: "("}}}},
	}
	for name, d := range cases {
		if err := d.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

// fakeEnv implements CheckEnv from in-memory maps.
type fakeEnv struct {
	goos  string
	paths map[string]string   // exe name -> resolved path
	globs map[string][]string // glob -> matches
	envs  map[string]string   // var -> value
	reg   map[string]bool     // registry key -> exists
}

func (f fakeEnv) LookPath(n string) (string, bool) { p, ok := f.paths[n]; return p, ok }
func (f fakeEnv) Glob(p string) []string           { return f.globs[p] }
func (f fakeEnv) Getenv(k string) (string, bool)   { v, ok := f.envs[k]; return v, ok }
func (f fakeEnv) RegistryExists(k string) bool     { return f.reg[k] }
func (f fakeEnv) GOOS() string                     { return f.goos }

func TestDetector_Evaluate_MultiVersionAndBare(t *testing.T) {
	d := Detector{
		Tag:     "houdini",
		Checks:  []Check{{PathGlob: "/opt/hfs*/bin/houdini"}, {Exe: "hython"}},
		Version: VersionSpec{From: "hfs(?P<v>[0-9.]+)"},
	}
	env := fakeEnv{
		goos:  "linux",
		globs: map[string][]string{"/opt/hfs*/bin/houdini": {"/opt/hfs20.0/bin/houdini", "/opt/hfs20.5/bin/houdini"}},
		paths: map[string]string{"hython": "/opt/hfs20.5/bin/hython"},
	}
	got := d.Evaluate(env)
	want := []string{"houdini", "houdini-20.0", "houdini-20.5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDetector_Evaluate_OSGateAndNoMatch(t *testing.T) {
	d := Detector{Tag: "maya", Checks: []Check{{Registry: `HKLM\X`, OS: "windows"}}}
	// On linux the windows-gated check is skipped -> no tags.
	if got := d.Evaluate(fakeEnv{goos: "linux", reg: map[string]bool{`HKLM\X`: true}}); len(got) != 0 {
		t.Errorf("os gate not honored: %v", got)
	}
	// On windows it matches -> bare tag only (no version spec).
	if got := d.Evaluate(fakeEnv{goos: "windows", reg: map[string]bool{`HKLM\X`: true}}); strings.Join(got, ",") != "maya" {
		t.Errorf("got %v, want [maya]", got)
	}
}
