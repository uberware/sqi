// SPDX-License-Identifier: AGPL-3.0-or-later

package yamlstrict_test

import (
	"reflect"
	"slices"
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
		Enabled bool     `yaml:"enabled"`
		Mode    string   `yaml:"mode"`
		Items   []item   `yaml:"items"`
		Tags    []string `yaml:"tags"`
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
		{"section where a value belongs", "section:\n  mode:\n    x: 1\n", []string{"line 3: section.mode: cannot unmarshal !!map into string"}},
		{"section where a value belongs, flow", "section: {mode: {x: 1}}\n", []string{"line 1: section.mode: cannot unmarshal !!map into string"}},
		{"value where a section belongs", "section: true\n", []string{"line 1: section: cannot unmarshal !!bool `true`"}},
		{"section in a list of values", "section:\n  tags:\n    - a\n    - b: 1\n", []string{"line 4: section.tags[1]: cannot unmarshal !!map into string"}},
		{"section in a list of values, flow", "section: {tags: [a, {b: 1}]}\n", []string{"line 1: section.tags[1]: cannot unmarshal !!map into string"}},
		{"json", `{"section": {"enabeld": true}}`, []string{"unknown key section.enabeld"}},
		{"unknown key in a list item", "section:\n  items:\n    - id: a\n    - idd: b\n", []string{"line 4: unknown key section.items[1].idd"}},
		{"merge key", "base: &b {enabled: true}\n", []string{"unknown key base"}},
		{"merged section", "section:\n  <<: {enabled: true}\n  mode: x\n", nil},
		{"leading document marker", "---\nname: a\n", nil},
		{"trailing document marker", "name: a\n---\n", nil},
		{"second document", "name: a\n---\nsection:\n  mode: x\n", []string{"line 3: a second YAML document"}},
		{"empty first document, second document", "---\n---\nname: a\n", []string{"line 3: a second YAML document"}},
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

// picky decodes a mapping through DecodeNode, as a custom unmarshaler must,
// since node.Decode does not inherit the outer decoder's strictness.
type picky struct {
	Name  string `yaml:"name"`
	Count int    `yaml:"count"`
}

func (p *picky) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		p.Name = n.Value
		return nil
	}
	type raw picky
	return yamlstrict.DecodeNode(n, (*raw)(p))
}

// TestDecodeNode pins that below a custom unmarshaler an unknown key is named
// by dotted path like any other, reported together with the type errors in the
// same mapping, that a merge key is accepted and the keys it merges in are
// checked, and that a known mapping decodes.
func TestDecodeNode(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"valid", "list:\n  - a\n  - name: b\n    count: 2\n", nil},
		{"unknown key", "list:\n  - name: a\n  - label: b\n", []string{"line 3: unknown key list[1].label"}},
		{"unknown key and a type error", "list:\n  - {name: [a], nme: b}\n", []string{"unknown key list[0].nme", "list[0].name: cannot unmarshal !!seq into string"}},
		{"every item is checked", "list:\n  - {lable: a}\n  - {nme: b}\n", []string{"unknown key list[0].lable", "unknown key list[1].nme"}},
		{"merge key", "base: &b {name: a}\nlist:\n  - {<<: *b, count: 2}\n", nil},
		{"merge list", "base: &b {name: a}\nlist:\n  - {<<: [*b, {count: 2}]}\n", nil},
		{"merged-in unknown key", "list:\n  - {<<: {nme: a}, count: 2}\n", []string{"line 2: unknown key list[0].<<.nme"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v struct {
				Base map[string]string `yaml:"base"`
				List []picky           `yaml:"list"`
			}
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

	var v struct {
		List []picky `yaml:"list"`
	}
	if err := yamlstrict.Decode([]byte("list:\n  - a\n  - {name: b, count: 2}\n"), &v); err != nil ||
		!reflect.DeepEqual(v.List, []picky{{Name: "a"}, {Name: "b", Count: 2}}) {
		t.Fatalf("Decode = %v, %+v", err, v.List)
	}
}

type inner struct {
	Mode string `yaml:"mode"`
}

type tagged struct {
	Plain   string
	Named   string `yaml:"named,omitempty"`
	Skipped string `yaml:"-"`
	Inline  inner  `yaml:",inline"`
	private string //nolint:unused // pins that an unexported field is no key
}

func (t *tagged) UnmarshalYAML(n *yaml.Node) error {
	type raw tagged
	return yamlstrict.DecodeNode(n, (*raw)(t))
}

// TestDecodeNode_KeysFollowYAMLTags pins that the accepted keys are the ones
// yaml.v3 decodes: a missing name is the lowercased field name, "-" is no key,
// and an inline struct's keys are accepted.
func TestDecodeNode_KeysFollowYAMLTags(t *testing.T) {
	var v struct {
		T tagged `yaml:"t"`
	}
	if err := yamlstrict.Decode([]byte("t: {plain: a, named: b, mode: c}\n"), &v); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if v.T.Plain != "a" || v.T.Named != "b" || v.T.Inline.Mode != "c" {
		t.Fatalf("decoded %+v", v.T)
	}
	for _, key := range []string{"skipped", "private", "inline"} {
		if err := yamlstrict.Decode([]byte("t: {"+key+": x}\n"), &v); err == nil || !strings.Contains(err.Error(), "unknown key t."+key) {
			t.Errorf("key %q: error %v", key, err)
		}
	}
	if err := yamlstrict.DecodeNode(&yaml.Node{Kind: yaml.MappingNode}, new(int)); err == nil {
		t.Error("DecodeNode accepted a non-struct target")
	}
}

type scalar string

func (s *scalar) UnmarshalYAML(n *yaml.Node) error { *s = scalar(n.Value); return nil }

// TestUnmarshalers pins that every custom unmarshaler reachable from a type is
// found, through pointers, slices, maps and nested structs, each once.
func TestUnmarshalers(t *testing.T) {
	type cfg struct {
		A *struct {
			B []picky `yaml:"b"`
		} `yaml:"a"`
		C map[string]scalar `yaml:"c"`
		D picky             `yaml:"d"`
		E scalar            `yaml:"-"`
		F string            `yaml:"f"`
	}
	got := yamlstrict.Unmarshalers(reflect.TypeFor[cfg]())
	want := []reflect.Type{reflect.TypeFor[picky](), reflect.TypeFor[scalar]()}
	if !slices.Equal(got, want) {
		t.Fatalf("Unmarshalers = %v, want %v", got, want)
	}
}
