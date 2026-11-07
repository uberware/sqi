// SPDX-License-Identifier: AGPL-3.0-or-later

package yamlstrict_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/yamlstrict"
)

type item struct {
	ID string `yaml:"id"`
}

type shape struct {
	Name    string `yaml:"name"`
	Section struct {
		Enabled bool   `yaml:"enabled"`
		Mode    string `yaml:"mode"`
		Items   []item `yaml:"items"`
	} `yaml:"section"`
}

// TestDecode pins that an unknown key or a value of the wrong shape fails
// naming its dotted path, every one is reported, and valid or empty data
// decodes.
func TestDecode(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string // each must appear in the error; none: the data is valid
	}{
		{"valid", "name: a\nsection:\n  enabled: true\n", nil},
		{"empty", "", nil},
		{"comments only", "# nothing set\n", nil},
		{"unknown nested key", "section:\n  enabeld: true\n", []string{"line 2: unknown key section.enabeld"}},
		{"unknown top-level key", "bogus: 1\n", []string{"line 1: unknown key bogus"}},
		{"every unknown key is named", "bogus: 1\nsection:\n  nope: 2\n", []string{"unknown key bogus", "unknown key section.nope"}},
		{"section where a value belongs", "section:\n  mode:\n    x: 1\n", []string{"section.mode"}},
		{"value where a section belongs", "section: true\n", []string{"section"}},
		{"json", `{"section": {"enabeld": true}}`, []string{"unknown key section.enabeld"}},
		{"unknown key in a list item", "section:\n  items:\n    - id: a\n    - idd: b\n", []string{"line 4: unknown key section.items[1].idd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v shape
			err := yamlstrict.Decode([]byte(tt.body), &v)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Decode accepted %q", tt.body)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

// picky decodes a mapping with node.Decode, which does not inherit the outer
// decoder's strictness, so it checks its keys with KnownKeys.
type picky struct {
	Name string `yaml:"name"`
}

func (p *picky) UnmarshalYAML(n *yaml.Node) error {
	if err := yamlstrict.KnownKeys(n, "picky", "name"); err != nil {
		return err
	}
	type raw picky
	return n.Decode((*raw)(p))
}

// TestKnownKeys pins that an unknown key below a custom unmarshaler is named
// by dotted path like any other, and a known one decodes.
func TestKnownKeys(t *testing.T) {
	var v struct {
		List []picky `yaml:"list"`
	}
	if err := yamlstrict.Decode([]byte("list:\n  - name: a\n"), &v); err != nil || v.List[0].Name != "a" {
		t.Fatalf("Decode = %v, %+v", err, v)
	}
	err := yamlstrict.Decode([]byte("list:\n  - name: a\n  - label: b\n"), &v)
	if want := "line 3: unknown key list[1].label"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %v does not contain %q", err, want)
	}
}
