// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import "strings"

// parseJobParamType canonicalizes a declared job-parameter type spelling.
//
// RFC 0007 (the EXPR extension's Extended Parameter Types) makes type names
// case-insensitive, including compound names like LIST[LIST[INT]]. It grants
// NOTHING ELSE: internal whitespace and nesting deeper than one level are not
// licensed, so "list [int]" and "list[list[list[int]]]" remain unknown types.
// Accepting more than the spec authorizes would widen what sqi admits, which
// is an acceptance change and not this function's to make.
//
// The canonical (uppercase) form is what callers store on the model, so every
// downstream comparison against a JobParamType constant keeps working and only
// the decoder ever sees the author's casing.
//
// Callers are responsible for the EXPR gate: this function does not know
// whether the template declares the extension, and canonicalizing an
// EXPR-only type for a base-spec template would silently widen the base
// specification. See canonicalJobParamType.
func parseJobParamType(raw string) (JobParamType, bool) {
	switch JobParamType(strings.ToUpper(raw)) {
	case JobParamTypeInt:
		return JobParamTypeInt, true
	case JobParamTypeFloat:
		return JobParamTypeFloat, true
	case JobParamTypeString:
		return JobParamTypeString, true
	case JobParamTypePath:
		return JobParamTypePath, true
	case JobParamTypeBool:
		return JobParamTypeBool, true
	case JobParamTypeRangeExpr:
		return JobParamTypeRangeExpr, true
	case JobParamTypeListString:
		return JobParamTypeListString, true
	case JobParamTypeListPath:
		return JobParamTypeListPath, true
	case JobParamTypeListInt:
		return JobParamTypeListInt, true
	case JobParamTypeListFloat:
		return JobParamTypeListFloat, true
	case JobParamTypeListBool:
		return JobParamTypeListBool, true
	case JobParamTypeListListInt:
		return JobParamTypeListListInt, true
	}
	return "", false
}

// parseTaskParamType is parseJobParamType's counterpart for task parameters.
//
// RFC 0007 makes task type names case-insensitive too, but adds no task
// types: the list types are job-only. sqi's own CHUNK[INT] is included because
// it is spelled like the compound types and an author writing "chunk[int]"
// alongside "list[int]" would reasonably expect both to work.
func parseTaskParamType(raw string) (TaskParamType, bool) {
	switch TaskParamType(strings.ToUpper(raw)) {
	case TaskParamTypeInt:
		return TaskParamTypeInt, true
	case TaskParamTypeFloat:
		return TaskParamTypeFloat, true
	case TaskParamTypeString:
		return TaskParamTypeString, true
	case TaskParamTypePath:
		return TaskParamTypePath, true
	case TaskParamTypeChunkInt:
		return TaskParamTypeChunkInt, true
	}
	return "", false
}

// canonicalJobParamType applies parseJobParamType under the EXPR gate.
//
// It is the single place the gate lives, so the rule reads in one spot rather
// than being re-derived at each decoder: with EXPR declared, a recognized
// spelling is canonicalized; without it -- or for a spelling nothing
// recognizes -- the author's text is stored verbatim, so validation reports an
// unknown type naming exactly what the template said.
//
// Storing the raw text on the no-match path matters for the error message. A
// template that says "map[string]" should be told that "map[string]" is
// unknown, not that "" is.
func canonicalJobParamType(raw string, exprDeclared bool) JobParamType {
	if exprDeclared {
		if canonical, ok := parseJobParamType(raw); ok {
			return canonical
		}
	}
	return JobParamType(raw)
}

// canonicalTaskParamType is canonicalJobParamType for task parameters.
func canonicalTaskParamType(raw string, exprDeclared bool) TaskParamType {
	if exprDeclared {
		if canonical, ok := parseTaskParamType(raw); ok {
			return canonical
		}
	}
	return TaskParamType(raw)
}
