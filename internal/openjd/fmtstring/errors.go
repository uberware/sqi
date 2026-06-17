// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtstring

import (
	"fmt"
	"strings"
)

// MalformedError is returned when an input contains a syntactically invalid
// format-string reference: an unclosed "{{", an empty or whitespace-only name,
// or a name that is not a valid dotted OpenJD identifier.
type MalformedError struct {
	// Ref is the offending reference text, including the surrounding braces
	// when they are present (for an unclosed reference it is the text from
	// "{{" to the end of the input).
	Ref string
	// Reason describes why the reference is malformed.
	Reason string
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("malformed format-string reference %q: %s", e.Ref, e.Reason)
}

// UnresolvedError is returned by [Resolve] when one or more references name
// variables that the [Scope] does not provide. Names lists every distinct
// unknown variable in order of first appearance.
type UnresolvedError struct {
	Names []string
}

func (e *UnresolvedError) Error() string {
	if len(e.Names) == 1 {
		return fmt.Sprintf("unresolved format-string variable %q", e.Names[0])
	}
	return "unresolved format-string variables: " + strings.Join(quoteAll(e.Names), ", ")
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}
