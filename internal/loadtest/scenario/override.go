// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Set overrides one field, for example Set("workers[0].count", "500") or
// Set("jobs[1].task.fail_rate", "0.05"). The path names fields by their YAML
// keys, indexes lists with [n], and for a map names the key after it
// ("workers[0].tags.gpu", "assert.p95_submit_to_first_lease"). The value is
// parsed as YAML into the field's type, so anything a scenario file can say
// about a field, Set can too. A failed Set leaves the scenario unchanged.
func (s *Scenario) Set(path, value string) error {
	segs, err := splitPath(path)
	if err != nil {
		return err
	}
	cp := s.Clone()
	if err := setField(reflect.ValueOf(cp).Elem(), segs, value, path); err != nil {
		return err
	}
	*s = *cp
	return nil
}

// deepCopy copies v so that nothing in the result aliases v.
func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		n := reflect.New(v.Type().Elem())
		n.Elem().Set(deepCopy(v.Elem()))
		return n
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		n := reflect.New(v.Type()).Elem()
		n.Set(deepCopy(v.Elem()))
		return n
	case reflect.Struct:
		n := reflect.New(v.Type()).Elem()
		for i := range v.NumField() {
			n.Field(i).Set(deepCopy(v.Field(i)))
		}
		return n
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			n.Index(i).Set(deepCopy(v.Index(i)))
		}
		return n
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			n.SetMapIndex(it.Key(), deepCopy(it.Value()))
		}
		return n
	default:
		return v
	}
}

type segment struct {
	name     string
	index    int
	hasIndex bool
}

var segmentRE = regexp.MustCompile(`^([^\[\]]+)(?:\[(\d+)\])?$`)

func splitPath(path string) ([]segment, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("set: empty path")
	}
	parts := strings.Split(path, ".")
	segs := make([]segment, 0, len(parts))
	for _, p := range parts {
		m := segmentRE.FindStringSubmatch(p)
		if m == nil {
			return nil, fmt.Errorf("%s: malformed path element %q", path, p)
		}
		seg := segment{name: m[1]}
		if m[2] != "" {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				return nil, fmt.Errorf("%s: malformed index in %q", path, p)
			}
			seg.index, seg.hasIndex = n, true
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

var (
	unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()
	anyMapType      = reflect.TypeFor[map[string]any]()
)

// setField resolves segs[0] as a field of the struct v and continues from there.
func setField(v reflect.Value, segs []segment, value, full string) error {
	seg := segs[0]
	fv, ok := fieldByYAMLName(v, seg.name)
	if !ok {
		return fmt.Errorf("%s: unknown field %q", full, seg.name)
	}
	switch {
	case seg.hasIndex && fv.Kind() != reflect.Slice:
		return fmt.Errorf("%s: %q is not a list, so it cannot take an index", full, seg.name)
	case seg.hasIndex && seg.index >= fv.Len():
		return fmt.Errorf("%s: index out of range: %q has %d element(s)", full, seg.name, fv.Len())
	case seg.hasIndex:
		fv = fv.Index(seg.index)
	case fv.Kind() == reflect.Slice && len(segs) > 1:
		return fmt.Errorf("%s: %q is a list; give it an index, like %s[0]", full, seg.name, seg.name)
	}
	return setBelow(fv, segs[1:], seg.name, value, full)
}

// setBelow sets fv itself when no segments remain, and otherwise descends.
func setBelow(fv reflect.Value, rest []segment, name, value, full string) error {
	if len(rest) == 0 {
		return setLeaf(fv, value, full)
	}
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		fv = fv.Elem()
	}
	switch {
	case fv.Kind() == reflect.Map:
		return setMapEntry(fv, rest, value, full)
	case fv.Kind() == reflect.Struct && !reflect.PointerTo(fv.Type()).Implements(unmarshalerType):
		return setField(fv, rest, value, full)
	}
	return fmt.Errorf("%s: %q is a single value and has no fields to set", full, name)
}

func fieldByYAMLName(v reflect.Value, want string) (reflect.Value, bool) {
	t := v.Type()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" && name == want {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func setLeaf(fv reflect.Value, value, full string) error {
	v, err := parseValue(fv.Type(), value)
	if err != nil {
		return fmt.Errorf("%s: %w", full, err)
	}
	fv.Set(v)
	return nil
}

// parseValue parses value as YAML into a new value of type t. An empty value
// gives the zero value.
func parseValue(t reflect.Type, value string) (reflect.Value, error) {
	p := reflect.New(t)
	dec := yaml.NewDecoder(strings.NewReader(value))
	dec.KnownFields(true)
	if err := dec.Decode(p.Interface()); err != nil && !errors.Is(err, io.EOF) {
		return reflect.Value{}, err
	}
	return p.Elem(), nil
}

// setMapEntry sets one entry of the map m. A map[string]any takes a nested
// path, creating the maps on the way; any other map takes the rest of the path
// as its key.
func setMapEntry(m reflect.Value, rest []segment, value, full string) error {
	if m.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("%s: only maps keyed by strings can be set", full)
	}
	for _, seg := range rest {
		if seg.hasIndex {
			return fmt.Errorf("%s: map keys cannot take an index", full)
		}
	}
	if m.IsNil() {
		m.Set(reflect.MakeMap(m.Type()))
	}
	if m.Type() == anyMapType {
		return setNestedAny(m.Interface().(map[string]any), rest, value, full) //nolint:errcheck,forcetypeassert // type checked on the line above
	}
	names := make([]string, len(rest))
	for i, seg := range rest {
		names[i] = seg.name
	}
	v, err := parseValue(m.Type().Elem(), value)
	if err != nil {
		return fmt.Errorf("%s: %w", full, err)
	}
	m.SetMapIndex(reflect.ValueOf(strings.Join(names, ".")).Convert(m.Type().Key()), v)
	return nil
}

func setNestedAny(cur map[string]any, rest []segment, value, full string) error {
	for i, seg := range rest {
		if i == len(rest)-1 {
			v, err := parseValue(reflect.TypeFor[any](), value)
			if err != nil {
				return fmt.Errorf("%s: %w", full, err)
			}
			cur[seg.name] = v.Interface()
			return nil
		}
		switch next := cur[seg.name].(type) {
		case nil:
			n := map[string]any{}
			cur[seg.name] = n
			cur = n
		case map[string]any:
			cur = next
		default:
			return fmt.Errorf("%s: %q holds %v, not a map", full, seg.name, next)
		}
	}
	return nil
}
