// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import (
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
		"no tag":       {Checks: []Check{{Exe: "x"}}},
		"no checks":    {Tag: "x"},
		"empty check":  {Tag: "x", Checks: []Check{{}}},
		"two-in-check": {Tag: "x", Checks: []Check{{Exe: "a", PathGlob: "b"}}},
		"bad version":  {Tag: "x", Checks: []Check{{Exe: "a"}}, Version: VersionSpec{From: "("}},
		"bad os":       {Tag: "x", Checks: []Check{{Exe: "a", OS: "solaris"}}},
	}
	for name, d := range cases {
		if err := d.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}
