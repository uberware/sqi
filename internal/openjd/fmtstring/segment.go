// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtstring

// Segment is one piece of a format string: either a run of literal text or a
// reference body.
//
// The body is reported VERBATIM, trimmed of surrounding whitespace but
// otherwise unjudged. This package deliberately does not know what a reference
// body means: under the base specification it is a dotted identifier
// (validName), and with the EXPR extension it is an arbitrary expression, and
// keeping that decision in the caller is what lets one scanner serve both.
type Segment struct {
	// Literal is the text, when IsRef is false.
	Literal string
	// Ref is the trimmed reference body, when IsRef is true.
	Ref string
	// IsRef distinguishes the two. A zero Segment is an empty literal.
	IsRef bool
}

// Segments splits input into literal runs and reference bodies, in order.
//
// It returns a *MalformedError on an unclosed or empty reference, sharing the
// underlying scanner with Resolve and References (parse and parseRaw both
// call scan), so all three agree on where a reference starts and ends. They
// do NOT agree on whether a given body is well-formed: Segments reports a
// body like "Param.1bad" unjudged, where Resolve and References would reject
// it via parse's validName check -- that judgment is deliberately left to the
// caller, per the package doc comment.
func Segments(input string) ([]Segment, error) {
	refs, trailing, err := parseRaw(input)
	if err != nil {
		return nil, err
	}
	var out []Segment
	for _, r := range refs {
		if r.literal != "" {
			out = append(out, Segment{Literal: r.literal})
		}
		out = append(out, Segment{Ref: r.name, IsRef: true})
	}
	if trailing != "" {
		out = append(out, Segment{Literal: trailing})
	}
	return out, nil
}

// LoneRef reports the body when input is EXACTLY one reference with no
// surrounding text, which is section 1.3.2's condition for a format string to
// inherit its field's target type rather than being converted to a string.
//
// Surrounding whitespace counts as text: " {{X}}" is a string whose value
// begins with a space, not a transparent reference.
func LoneRef(input string) (string, bool) {
	segs, err := Segments(input)
	if err != nil || len(segs) != 1 || !segs[0].IsRef {
		return "", false
	}
	return segs[0].Ref, true
}
