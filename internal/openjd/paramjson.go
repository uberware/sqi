// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// This file defines the ONE canonical text form of a LIST[*] parameter value.
//
// [JobParameter.Default] stays a *string for every type, and a list default is
// stored as canonical JSON in it. The reason is the runtime stack below this
// package: store.Job.Parameters and the worker protocol's
// AssignMsg.JobParameters are both map[string]string with a JSON wire format,
// so a typed model representation would have to serialize at every boundary
// anyway. One representation, defined once, at the top.
//
// THE STORED FORM IS NOT THE INTERPOLATED FORM. A list rendered into a command
// line goes through expr.Value.String(), which quotes elements with
// strconv.Quote -- Go syntax, identical to JSON for ordinary text and
// different for control characters (Go writes \x00 where JSON requires a
//  escape, and \x00 is not valid JSON at all). Section 1.3.2 governs
// interpolation; this file governs storage. They are two renderings for two
// purposes and must not be assumed interchangeable --
// TestListDefault_JSONIsNotValueString pins the difference so it stays a
// documented fact rather than a latent surprise for sub-project F2.

// isListParamType reports whether t is one of RFC 0007's list types.
func isListParamType(t JobParamType) bool {
	switch t {
	case JobParamTypeListString, JobParamTypeListPath, JobParamTypeListInt,
		JobParamTypeListFloat, JobParamTypeListBool, JobParamTypeListListInt:
		return true
	}
	return false
}

// encodeListDefault converts a YAML-decoded default for a list type into
// canonical JSON.
//
// It accepts a sequence of scalars -- and, for LIST[LIST[INT]] only, a sequence
// of sequences of scalars -- and NOTHING else. A mapping is rejected at every
// level, preserving the rule scalarFieldStrict states for scalar fields: a
// coerced non-scalar would otherwise be substituted verbatim into a running
// task's command, script, or environment.
//
// Element VALUES are not type-checked here; that is
// validateListParamConstraints' job, which can attach a JSON pointer to the
// offending index. This function's only concern is SHAPE, so that a default
// which is structurally a list reaches validation and is reported there with a
// precise pointer, rather than failing at parse with a whole-document error.
func encodeListDefault(v any, t JobParamType) (string, error) {
	items, ok := toAnySlice(v)
	if !ok {
		return "", fmt.Errorf("openjd: default for %s must be a list", t)
	}

	out := make([]any, 0, len(items))
	for i, item := range items {
		if t == JobParamTypeListListInt {
			inner, err := encodeInnerList(item, i, t)
			if err != nil {
				return "", err
			}
			out = append(out, inner)
			continue
		}
		if !isScalarValue(item) {
			return "", fmt.Errorf(
				"openjd: default[%d] for %s must be a string, number, or boolean", i, t,
			)
		}
		out = append(out, item)
	}

	return marshalCanonical(out, t)
}

// encodeInnerList validates and returns one inner list of a LIST[LIST[INT]]
// default. A scalar where a list belongs, or a mapping anywhere inside, is an
// error -- the shape must be exactly two levels deep.
func encodeInnerList(item any, i int, t JobParamType) ([]any, error) {
	inner, ok := toAnySlice(item)
	if !ok {
		return nil, fmt.Errorf("openjd: default[%d] for %s must be a list", i, t)
	}
	out := make([]any, 0, len(inner))
	for j, elem := range inner {
		if !isScalarValue(elem) {
			return nil, fmt.Errorf(
				"openjd: default[%d][%d] for %s must be a string, number, or boolean", i, j, t,
			)
		}
		out = append(out, elem)
	}
	return out, nil
}

// marshalCanonical renders v as the canonical JSON text for a list default.
//
// It uses json.Encoder with HTML escaping OFF rather than json.Marshal, which
// escapes <, > and & into \u form so its output is safe to embed in HTML. That
// is irrelevant here and actively unhelpful: a template author who writes
// ["a<b"] should read the same text back from the API in sub-project F2, not
// an escaped form. Both are valid JSON and decode identically.
//
// json.Encoder terminates every value with a newline, which json.Marshal does
// not; it is trimmed so a stored default compares equal to the same list
// written by hand.
func marshalCanonical(v any, t JobParamType) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("openjd: encode default for %s: %w", t, err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// decodeListDefault parses canonical JSON produced by encodeListDefault back
// into its elements. For LIST[LIST[INT]] each returned element is itself a
// []any.
//
// It is the decode half of the codec, used by validation to check elements
// against their declared type and item: constraints.
func decodeListDefault(s string, t JobParamType) ([]any, error) {
	var out []any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("openjd: decode default for %s: %w", t, err)
	}
	return out, nil
}

// isScalarValue reports whether v is a YAML scalar (string, number, or
// boolean) as opposed to a sequence or mapping.
//
// The integer cases are listed separately from float64 because a YAML decoder
// yields int for a whole number while a JSON decoder yields float64, and this
// helper runs against YAML-decoded input.
func isScalarValue(v any) bool {
	switch v.(type) {
	case string, bool, int, int64, float64:
		return true
	}
	return false
}
