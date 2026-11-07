// SPDX-License-Identifier: AGPL-3.0-or-later

// Package yamlstrict decodes a YAML (or JSON) config file and fails on any key
// the target type does not have, naming the key by its dotted path.
// sqi-server (internal/config) and sqi-worker (internal/worker/config) both
// read their config files through it, so a misspelled key is an error on both
// rather than a silently dropped line that leaves a setting on its default.
//
// It exists as a leaf because internal/config pulls internal/auth/oidc, so the
// worker binary can never import it. This package imports nothing internal.
package yamlstrict

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Decode decodes data into v and fails on a key v's shape does not have, or a
// value of the wrong shape (a section where a value belongs, or the reverse).
// yaml reports such a key by line and bare name only, so each failure is
// rewritten to name the key's dotted path, and every one is reported. Empty
// data decodes to nothing and is not an error. A second YAML document with
// anything in it is an error, since only the first would be read.
func Decode(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) { // an empty file
		return nil
	}
	if err != nil {
		return nameErrors(data, err)
	}
	return noFurtherDocuments(dec)
}

// nameErrors rewrites each key error in err to name its key's dotted path.
func nameErrors(data []byte, err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return err
	}
	root := Root(data)
	msgs := make([]string, len(typeErr.Errors))
	for i, m := range typeErr.Errors {
		msgs[i] = nameKey(root, m)
	}
	return errors.New(strings.Join(msgs, "; "))
}

// noFurtherDocuments fails when dec holds another document with anything in
// it. A bare trailing "---" is an empty document and is accepted.
func noFurtherDocuments(dec *yaml.Decoder) error {
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(doc.Content) > 0 && !isEmpty(doc.Content[0]) {
			return fmt.Errorf("line %d: a second YAML document (after \"---\") is not read; a config file holds one document", doc.Content[0].Line)
		}
	}
}

// isEmpty reports whether n is the null an empty document decodes to.
func isEmpty(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" && n.Value == ""
}

// Root returns the top-level node of data, or nil when data does not parse
// or is empty.
func Root(data []byte) *yaml.Node {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) == nil && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return nil
}

var (
	// unknownKeyErr is yaml.v3's "line N: field NAME not found in type T".
	unknownKeyErr = regexp.MustCompile(`^line (\d+): field (\S+) not found in type .*$`)
	// wrongShapeErr is yaml.v3's "line N: cannot unmarshal TAG[ `VALUE`] into
	// T", where the line and tag are the offending value's own.
	wrongShapeErr = regexp.MustCompile(`^line (\d+): (cannot unmarshal (\S+).*)$`)
)

// nameKey rewrites one yaml decode error to say which key it is about.
func nameKey(root *yaml.Node, msg string) string {
	if m := unknownKeyErr.FindStringSubmatch(msg); m != nil {
		line, _ := strconv.Atoi(m[1]) //nolint:errcheck // the regexp matched digits
		if path, ok := keyPathAtLine(root, line, m[2], nil); ok {
			return fmt.Sprintf("line %d: unknown key %s", line, path)
		}
		return msg
	}
	if m := wrongShapeErr.FindStringSubmatch(msg); m != nil {
		line, _ := strconv.Atoi(m[1]) //nolint:errcheck // the regexp matched digits
		if path, ok := valuePathAtLine(root, line, m[3], nil); ok {
			return fmt.Sprintf("line %d: %s: %s", line, path, m[2])
		}
	}
	return msg
}

// keyPathAtLine returns the dotted path of the deepest key named name in n
// that sits on line, a list item's index written after its key
// (detect[0].checks[1].env).
func keyPathAtLine(n *yaml.Node, line int, name string, prefix []string) (string, bool) {
	if n == nil {
		return "", false
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for i, item := range n.Content {
			if p, ok := keyPathAtLine(item, line, name, withIndex(prefix, i)); ok {
				return p, true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			path := append(slices.Clone(prefix), k.Value)
			if p, ok := keyPathAtLine(v, line, name, path); ok {
				return p, true
			}
			if k.Value == name && k.Line == line {
				return strings.Join(path, "."), true
			}
		}
	}
	return "", false
}

// valuePathAtLine returns the dotted path of the deepest value in n (a map
// value or a list item) that starts on line and has tag, the offending value
// of a wrong-shape error. Matching the tag as well as the line names the value
// itself rather than the first key inside it, and picks the right one of
// several values on one line in flow style.
func valuePathAtLine(n *yaml.Node, line int, tag string, prefix []string) (string, bool) {
	if n == nil {
		return "", false
	}
	visit := func(v *yaml.Node, path []string) (string, bool) {
		if p, ok := valuePathAtLine(v, line, tag, path); ok {
			return p, true
		}
		if v.Line == line && v.ShortTag() == tag {
			return strings.Join(path, "."), true
		}
		return "", false
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for i, item := range n.Content {
			if p, ok := visit(item, withIndex(prefix, i)); ok {
				return p, true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if p, ok := visit(n.Content[i+1], append(slices.Clone(prefix), n.Content[i].Value)); ok {
				return p, true
			}
		}
	}
	return "", false
}

// withIndex returns prefix with the list index i appended to its last segment.
func withIndex(prefix []string, i int) []string {
	idx := fmt.Sprintf("[%d]", i)
	if len(prefix) == 0 {
		return []string{idx}
	}
	out := slices.Clone(prefix)
	out[len(out)-1] += idx
	return out
}

// DecodeNode decodes n into v, a pointer to a struct, as strictly as [Decode]
// decodes a file: a key v's struct does not have is an error, worded as yaml's
// own decoder words one so [Decode] names it by dotted path, and it is
// reported together with every type error in the rest of n. A custom
// UnmarshalYAML that decodes a mapping calls it in place of n.Decode, which
// does not inherit the outer decoder's strictness.
//
// The accepted keys are read from v's yaml struct tags, so they cannot drift
// from the fields. A YAML merge key (<<) is accepted and the keys it merges in
// are checked too.
func DecodeNode(n *yaml.Node, v any) error {
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("yamlstrict.DecodeNode: %T is not a pointer to a struct", v)
	}
	keys, anyKey := structKeys(t.Elem())
	var errs []string
	if !anyKey {
		errs = unknownKeys(n, t.Elem().String(), keys)
	}
	err := n.Decode(v)
	var typeErr *yaml.TypeError
	switch {
	case err == nil && errs == nil:
		return nil
	case err == nil:
		return &yaml.TypeError{Errors: errs}
	case errors.As(err, &typeErr):
		return &yaml.TypeError{Errors: append(errs, typeErr.Errors...)}
	default:
		return err
	}
}

// unknownKeys returns an error message for each key of mapping n not in keys,
// following merge keys into the mappings they merge in.
func unknownKeys(n *yaml.Node, typ string, keys []string) []string {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	var errs []string
	switch n.Kind {
	case yaml.SequenceNode: // a merge key's list of mappings
		for _, item := range n.Content {
			errs = append(errs, unknownKeys(item, typ, keys)...)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch k := n.Content[i]; {
			case isMergeKey(k):
				errs = append(errs, unknownKeys(n.Content[i+1], typ, keys)...)
			case !slices.Contains(keys, k.Value):
				errs = append(errs, fmt.Sprintf("line %d: field %s not found in type %s", k.Line, k.Value, typ))
			}
		}
	}
	return errs
}

// isMergeKey reports whether k is a YAML merge key, as yaml.v3 decides it.
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && k.ShortTag() == "!!merge"
}

// structKeys returns the keys yaml.v3 decodes into struct t, following its
// tag rules (a missing name is the lowercased field name, "-" is skipped,
// inline structs contribute their keys). anyKey is true when an inline map
// accepts every key.
func structKeys(t reflect.Type) (keys []string, anyKey bool) {
	for f := range t.Fields() {
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		name, flags, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if slices.Contains(strings.Split(flags, ","), "inline") {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Map {
				return nil, true
			}
			inner, innerAny := structKeys(ft)
			if innerAny {
				return nil, true
			}
			keys = append(keys, inner...)
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		keys = append(keys, name)
	}
	return keys, false
}

// Unmarshalers returns every type reachable from t, through struct fields,
// pointers, slices, arrays and map values, that implements yaml.Unmarshaler,
// each once, in the order first reached. A test of a config type calls it so a
// new custom UnmarshalYAML cannot silently bypass the strict decode: each one
// found must be reviewed (a mapping must go through [DecodeNode]) and listed.
func Unmarshalers(t reflect.Type) []reflect.Type {
	var out []reflect.Type
	seen := map[reflect.Type]bool{}
	unmarshaler := reflect.TypeFor[yaml.Unmarshaler]()
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		if seen[t] {
			return
		}
		seen[t] = true
		if t.Kind() != reflect.Pointer && (t.Implements(unmarshaler) || reflect.PointerTo(t).Implements(unmarshaler)) {
			out = append(out, t)
		}
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(t.Elem())
		case reflect.Map:
			walk(t.Key())
			walk(t.Elem())
		case reflect.Struct:
			for f := range t.Fields() {
				if (f.PkgPath == "" || f.Anonymous) && f.Tag.Get("yaml") != "-" {
					walk(f.Type)
				}
			}
		}
	}
	walk(t)
	return out
}
