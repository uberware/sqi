// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtstring

import "strings"

const (
	openDelim  = "{{"
	closeDelim = "}}"
)

// ref describes a single parsed reference and the literal text that precedes it.
type ref struct {
	literal string // literal text before this reference
	name    string // trimmed reference body
}

// Resolve replaces every "{{ name }}" reference in input with the value
// returned by scope.Lookup for the trimmed name. Literal text outside braces is
// copied verbatim.
//
// Resolve returns a *MalformedError if input contains a syntactically invalid
// reference, and a *UnresolvedError naming every distinct unknown variable if
// any reference names a variable the scope does not provide.
func Resolve(input string, scope Scope) (string, error) {
	refs, trailing, err := parse(input)
	if err != nil {
		return "", err
	}

	var (
		b       strings.Builder
		unknown []string
		seenBad map[string]bool
	)
	for _, r := range refs {
		b.WriteString(r.literal)
		value, ok := scope.Lookup(r.name)
		if !ok {
			if seenBad == nil {
				seenBad = make(map[string]bool)
			}
			if !seenBad[r.name] {
				seenBad[r.name] = true
				unknown = append(unknown, r.name)
			}
			continue
		}
		b.WriteString(value)
	}
	b.WriteString(trailing)

	if len(unknown) > 0 {
		return "", &UnresolvedError{Names: unknown}
	}
	return b.String(), nil
}

// References returns the distinct variable names referenced by input, in order
// of first appearance. It returns a *MalformedError if input contains a
// syntactically invalid reference.
func References(input string) ([]string, error) {
	refs, _, err := parse(input)
	if err != nil {
		return nil, err
	}
	var (
		names []string
		seen  = map[string]bool{}
	)
	for _, r := range refs {
		if seen[r.name] {
			continue
		}
		seen[r.name] = true
		names = append(names, r.name)
	}
	return names, nil
}

// parse scans input into a sequence of references (each carrying the literal
// text that precedes it) plus any trailing literal text after the final
// reference, validating each reference body as a dotted OpenJD identifier
// inline as it is found. It returns a *MalformedError on the first malformed
// reference -- whichever comes first in the input, whether that malformation
// is a bad identifier or a syntax error such as an unclosed "{{" -- matching
// the single-pass ordering this package has always had.
func parse(input string) (refs []ref, trailing string, err error) {
	return scan(input, validName)
}

// parseRaw scans input into a sequence of references (each carrying the
// literal text that precedes it) plus any trailing literal text after the
// final reference. It validates only reference *syntax* -- an unclosed "{{" or
// an empty/whitespace-only body -- and returns a *MalformedError on the first
// such case. It does not judge what a reference body means: that is the
// caller's job, since the base specification requires a dotted identifier
// while the EXPR extension allows an arbitrary expression.
func parseRaw(input string) (refs []ref, trailing string, err error) {
	return scan(input, nil)
}

// scan is the single scanner shared by parse and parseRaw. It walks input
// left to right, and when validate is non-nil, checks each reference body
// against it inline -- in the same pass that finds unclosed references and
// empty bodies -- so the first malformed reference in the input, of any
// kind, is the one reported. A nil validate skips the body check entirely,
// which is what lets parseRaw report only syntax errors.
func scan(input string, validate func(string) bool) (refs []ref, trailing string, err error) {
	rest := input
	for {
		open := strings.Index(rest, openDelim)
		if open < 0 {
			// No more references; everything left is literal.
			return refs, rest, nil
		}
		literal := rest[:open]
		afterOpen := rest[open+len(openDelim):]

		closeIdx := strings.Index(afterOpen, closeDelim)
		if closeIdx < 0 {
			return nil, "", &MalformedError{
				Ref:    rest[open:],
				Reason: "missing closing \"}}\"",
			}
		}

		inner := afterOpen[:closeIdx]
		name := strings.TrimSpace(inner)
		fullRef := rest[open : open+len(openDelim)+closeIdx+len(closeDelim)]
		if name == "" {
			return nil, "", &MalformedError{
				Ref:    fullRef,
				Reason: "empty variable name",
			}
		}
		if validate != nil && !validate(name) {
			return nil, "", &MalformedError{
				Ref:    fullRef,
				Reason: "not a valid dotted identifier",
			}
		}

		refs = append(refs, ref{literal: literal, name: name})
		rest = afterOpen[closeIdx+len(closeDelim):]
	}
}

// validName reports whether name is a non-empty dot-separated sequence of
// OpenJD identifiers, each matching [A-Za-z_][A-Za-z0-9_]*.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for segment := range strings.SplitSeq(name, ".") {
		if !validIdent(segment) {
			return false
		}
	}
	return true
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_':
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
