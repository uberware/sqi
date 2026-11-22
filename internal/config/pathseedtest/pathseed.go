// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pathseedtest checks that a configuration's NamedPaths covers every
// path-valued field of its struct, for the server's and the worker's tests.
package pathseedtest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// CheckCoverage seeds every path-valued field below root, an addressable
// struct of type typeName, with a unique sentinel, then fails t for each one
// whose sentinel namedPaths, called after seeding, does not return, unless
// exclusions names the field (by field path, with the reason). It also fails
// an exclusion whose field namedPaths does return, or that names no
// path-valued field.
//
// A field is path-valued by name: File, or a name ending in File, Dir, Path
// or Glob, or Exe; and a string or []string ending in Command, whose program
// is the path. Each slice or map of structs is given one element.
func CheckCoverage(t testing.TB, root reflect.Value, typeName string, exclusions map[string]string, namedPaths func() []string) {
	t.Helper()
	s := &seeder{want: map[string]string{}}
	s.walk(root, "")
	got := map[string]bool{}
	for _, p := range namedPaths() {
		got[p] = true
	}
	for field, sentinel := range s.want {
		_, excluded := exclusions[field]
		switch {
		case excluded && got[sentinel]:
			t.Errorf("%s is excluded but NamedPaths names it; remove the exclusion", field)
		case !excluded && !got[sentinel]:
			t.Errorf("%s holds a path and NamedPaths does not name it: add it to NamedPaths, "+
				"or to pathFieldExclusions with the reason it need not be checked", field)
		}
	}
	for field := range exclusions {
		if _, ok := s.want[field]; !ok {
			t.Errorf("exclusion %s is not a path-valued %s field; remove it", field, typeName)
		}
	}
}

// seeder sets every path-valued field it walks to a unique sentinel and
// records, by field path, the sentinel NamedPaths must return for it.
type seeder struct {
	n    int
	want map[string]string
}

func (s *seeder) sentinel(field string) string {
	s.n++
	v := fmt.Sprintf("/sentinel/%d", s.n)
	s.want[field] = v
	return v
}

// walk descends into v, giving each slice or map of structs one element.
func (s *seeder) walk(v reflect.Value, path string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		s.walk(v.Elem(), path)
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				s.field(v.Field(i), f.Name, strings.TrimPrefix(path+"."+f.Name, "."))
			}
		}
	case reflect.Slice:
		if isStructLike(v.Type().Elem()) {
			e := reflect.New(v.Type().Elem()).Elem()
			s.walk(e, path+"[]")
			v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), e))
		}
	case reflect.Map:
		if isStructLike(v.Type().Elem()) {
			e := reflect.New(v.Type().Elem()).Elem()
			s.walk(e, path+"{}")
			m := reflect.MakeMap(v.Type())
			m.SetMapIndex(reflect.ValueOf("k").Convert(v.Type().Key()), e)
			v.Set(m)
		}
	default:
	}
}

func (s *seeder) field(v reflect.Value, name, path string) {
	isString := v.Kind() == reflect.String
	isCommand := strings.HasSuffix(name, "Command")
	switch {
	case isString && isPathName(name):
		v.SetString(s.sentinel(path))
	case isString && isCommand:
		v.SetString(s.sentinel(path) + " {src} {dest}")
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String && isCommand:
		v.Set(reflect.ValueOf([]string{s.sentinel(path), "--arg"}).Convert(v.Type()))
	default:
		s.walk(v, path)
	}
}

func isPathName(name string) bool {
	if name == "File" || name == "Exe" {
		return true
	}
	for _, suffix := range []string{"File", "Dir", "Path", "Glob"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func isStructLike(t reflect.Type) bool {
	return t.Kind() == reflect.Struct || (t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct)
}
