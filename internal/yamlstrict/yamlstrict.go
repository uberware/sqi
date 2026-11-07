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
// data decodes to nothing and is not an error.
func Decode(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(v)
	if err == nil || errors.Is(err, io.EOF) { // io.EOF: an empty file
		return nil
	}
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

// Root returns the top-level node of data, or nil when data does not parse
// or is empty.
func Root(data []byte) *yaml.Node {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) == nil && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return nil
}

// yamlKeyErr matches the two shapes of yaml.v3 decode error that point at a
// key: "line N: field NAME not found in type T" (unknown key) and
// "line N: cannot unmarshal ..." (wrong shape).
var yamlKeyErr = regexp.MustCompile(`^line (\d+): (?:field (\S+) not found in type .*|(.*))$`)

// nameKey rewrites one yaml decode error to say which key it is about.
func nameKey(root *yaml.Node, msg string) string {
	m := yamlKeyErr.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	line, err := strconv.Atoi(m[1])
	if err != nil {
		return msg
	}
	path, ok := keyPathAtLine(root, line, m[2], nil)
	switch {
	case !ok:
		return msg
	case m[2] != "":
		return fmt.Sprintf("line %d: unknown key %s", line, path)
	default:
		return fmt.Sprintf("line %d: %s: %s", line, path, m[3])
	}
}

// keyPathAtLine returns the dotted path of the deepest key in n that sits on
// line, a list item's index written after its key (detect[0].checks[1].env).
// With name set only a key of that name counts (an unknown key); with it empty
// a key whose value starts on that line counts too (a value of the wrong
// shape).
func keyPathAtLine(n *yaml.Node, line int, name string, prefix []string) (string, bool) {
	if n == nil {
		return "", false
	}
	if n.Kind == yaml.SequenceNode {
		for i, item := range n.Content {
			if p, ok := keyPathAtLine(item, line, name, withIndex(prefix, i)); ok {
				return p, true
			}
		}
		return "", false
	}
	if n.Kind != yaml.MappingNode {
		return "", false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		path := append(slices.Clone(prefix), k.Value)
		if p, ok := keyPathAtLine(v, line, name, path); ok {
			return p, true
		}
		if name != "" && k.Value == name && k.Line == line || name == "" && (k.Line == line || v.Line == line) {
			return strings.Join(path, "."), true
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

// KnownKeys fails on a key of mapping n other than keys, worded as yaml's own
// decoder words one so [Decode] names it by dotted path. A custom
// UnmarshalYAML that decodes a mapping with n.Decode calls it, because
// n.Decode does not inherit the outer decoder's strictness.
func KnownKeys(n *yaml.Node, typ string, keys ...string) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	var errs []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; !slices.Contains(keys, k.Value) {
			errs = append(errs, fmt.Sprintf("line %d: field %s not found in type %s", k.Line, k.Value, typ))
		}
	}
	if errs == nil {
		return nil
	}
	return &yaml.TypeError{Errors: errs}
}
