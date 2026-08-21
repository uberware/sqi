// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// JobParamTypes maps a declared OpenJD job-parameter type ("STRING", "PATH",
// "INT", "FLOAT", "LIST[PATH]", ...) to the expression types of Param.<name>
// and RawParam.<name>, per section 1.2.2's job-parameter table.
//
// PATH and LIST[PATH] are the two rows where the raw form differs from the
// resolved form, because the raw value may be a path for another operating
// system that cannot be parsed locally: Param.<name> is a path (or list of
// paths), RawParam.<name> stays a string (or list of strings). Every other
// declared type binds Param and RawParam to the SAME type. That rule is
// stated in as many words by Template Schemas §7.3.1's value-reference
// table — "This is the same as RawParam.<ParamName> for all parameter types
// except PATH" (wiki/2023-09-Template-Schemas.md:1993) — NOT by
// Expression-Language §1.2.2, which an earlier revision of this comment cited
// for it; the substance was right and the citation was wrong. §1.2.2's own
// contribution is the type table above plus the PATH prose ("Param.<name>
// has type path with path mapping rules applied, while RawParam.<name> has
// type string containing the original unmapped value"). So an INT job
// parameter's RawParam.<name> is int, not string. Do not special-case
// RawParam to string uniformly; that would contradict the specification for
// every non-PATH type.
//
// An unrecognized spelling floors to "any" rather than leaving the name
// unbound — an unbound name would make a valid template fail as "unknown
// symbol", which is worse than typing it loosely.
//
// This is the single, shared definition of the mapping. internal/openjd's
// phase-1/phase-2 symbol-table builder (exprcheck.go's jobParamTypes) and
// the worker's phase-3 builder (internal/worker/fmtres/exprsyms.go) both
// call this function rather than each keeping their own copy.
// internal/openjd itself cannot be imported by the worker binary — it pulls
// in internal/store, which the worker must never depend on — so the mapping
// lives here, in the one package both sides already import
// (internal/openjd/expr imports only internal/openjd/intrange). A second,
// independently maintained copy of this table is exactly the drift EXPR
// sub-project E4a's design spec section 3.1 warns a worker-side symbol
// table must not introduce.
func JobParamTypes(declared string) (paramType, rawType Type) {
	switch declared {
	case "PATH":
		return TPath, TString
	case "LIST[PATH]":
		return ListOf(TPath), ListOf(TString)
	}
	t, err := ParseType(strings.ToLower(declared))
	if err != nil {
		return TAny, TAny
	}
	return t, t
}

// TaskParamType maps a declared OpenJD task-parameter type per section
// 1.2.2's task table. CHUNK[INT] is range_expr, NOT list[int], so that a
// frame range need not be expanded. Task.Param.<name> and
// Task.RawParam.<name> share this same type for every declared task-
// parameter type, including PATH. §1.2.2's task table gives ONE expression
// type per declared type, with no Param/RawParam split at all; the
// value-versus-type distinction comes from Template Schemas' own task-
// parameter definitions, which describe Task.Param.<name> as "the value of
// the parameter with relevant path mapping rules applied to it" and
// Task.RawParam.<name> as "the value of the parameter as it was defined,
// with no path mapping rules applied"
// (wiki/2023-09-Template-Schemas.md:1248-1249). An earlier revision of this
// comment attributed those two quotes to §1.2.2, which does not contain
// them. It is a VALUE difference, not a type difference; both remain path.
//
// An unrecognized spelling floors to "any", for the same reason
// JobParamTypes does. See that function's doc comment for why this mapping
// is shared rather than duplicated per caller.
func TaskParamType(declared string) Type {
	if declared == "CHUNK[INT]" {
		return TRangeExpr
	}
	t, err := ParseType(strings.ToLower(declared))
	if err != nil {
		return TAny
	}
	return t
}

// ValueFromText converts a parameter's submitted raw text into a concrete
// Value of type t. It returns Unresolved(t) in two distinct situations,
// which are easy to conflate:
//
//  1. PARSE FAILURE, for a type this function does make concrete. INT and
//     FLOAT go concrete when raw parses and fall back to Unresolved(t) when
//     it does not: this function is not a validator, so a value that fails
//     to parse here is reported as still-unknown rather than causing a
//     panic or a lie about what was bound. Validating a submitted value
//     against its declared type happens upstream, before this is called.
//     STRING and PATH always go concrete — every string parses as either.
//
//  2. BY CONSTRUCTION, for a type this function never makes concrete at
//     all. As of EXPR sub-project F1 there is NO SUCH TYPE LEFT. Bool and
//     list were the two: this paragraph used to read "F MUST add its cases
//     here when that lands, or its own parameters will silently never
//     resolve past a placeholder in phase 3", and F1 added them. The
//     instruction is kept rather than deleted because the hazard it names
//     is real and permanent for any type added later — an unhandled type
//     reaches the default branch and returns Unresolved(t), which is a
//     LEGITIMATE phase-1 result, so nothing errors and nothing logs; the
//     parameter simply never resolves. TestValueFromText_ResolvesTheF1Types
//     guards it by asserting CONCRETENESS rather than correctness, and any
//     future type must join that table.
//     This is the one place the mapping lives now that phase 2 and phase 3
//     share it (see JobParamTypes' doc comment).
//
// range_expr (CodeRangeExpr) IS made concrete, via RangeExpr(raw) with the
// same parse-failure-falls-back-to-Unresolved rule as INT/FLOAT.
// CHUNK[INT] task parameters (declared today, not deferred to F) are the
// producer: internal/openjd/expand.go stores a CHUNK[INT] task parameter's
// value as a range-expression string like "1-5", and unlike a job
// parameter's declared type (which internal/openjd's JobParamType enum
// limits to STRING/PATH/INT/FLOAT — nothing can construct a RANGE_EXPR-
// typed job parameter that passes validation), this case IS reachable in
// production, from Task.Param.<chunked-name> in the worker's phase-3 table.
// Phase 2 cannot observe this case: internal/openjd's bindTaskParamSymbols
// binds every Task.Param./Task.RawParam. entry as Unresolved(t) directly
// and never calls this function for task parameters at all (task
// parameters are per-task, not per-template — see that function's own doc
// comment) — verified by reading it, not assumed.
//
// pathFlavor selects the flavor for a CodePath result, so callers evaluating
// in different contexts get a Path value that matches their own
// evaluation's flavor rather than a fixed one: internal/openjd hardcodes
// PathPOSIX here for submission-time job-parameter binding (a known,
// pre-existing gap — nothing server-side sets a PathFormat other than the
// default), while the worker's phase-3 table (a genuine host context) uses
// PathNative.
//
// The CodeFloat case binds raw as the value's rendered form (FloatText),
// per section 1.3.4: submitting "3.500" to a FLOAT parameter must preserve
// that exact text, not the canonical "3.5" strconv.ParseFloat's companion
// FormatFloat would produce. Callers that store submitted parameters as
// plain strings (both internal/openjd and the worker do) already have the
// original text in raw — no separate capture is needed.
func ValueFromText(t Type, raw string, pathFlavor PathFormat) Value {
	switch t.Code {
	case CodeInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Unresolved(t)
		}
		return Int(n)
	case CodeFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Unresolved(t)
		}
		return FloatText(f, raw)
	case CodeString:
		return String(raw)
	case CodePath:
		return Path(raw, pathFlavor)
	case CodeRangeExpr:
		v, err := RangeExpr(raw)
		if err != nil {
			return Unresolved(t)
		}
		return v
	case CodeBool:
		b, ok := parseBoolText(raw)
		if !ok {
			return Unresolved(t)
		}
		return Bool(b)
	case CodeList:
		v, err := listFromJSON(t, raw, pathFlavor)
		if err != nil {
			return Unresolved(t)
		}
		return v
	default:
		return Unresolved(t)
	}
}

// parseBoolText interprets a BOOL parameter's submitted text per RFC 0007's
// accepted-values table.
//
// It MUST accept exactly what internal/openjd's parseBoolParamValue accepts.
// That function validates a template's declared default; this one converts a
// submitted value, and a spelling the validator admits but this rejects would
// pass submission and then resolve to nothing on the host. The two cannot
// share code -- the worker binary must never import internal/openjd, which is
// why the section 1.2.2 mapping lives in this package at all -- so
// TestValueFromText_BoolRejectsUnknownSpelling and its counterpart
// TestParseBoolParamValue are what keep them in step.
func parseBoolText(raw string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "on", "1", "1.0":
		return true, true
	case "false", "no", "off", "0", "0.0":
		return false, true
	}
	return false, false
}

// listFromJSON converts canonical JSON list text into a List value of type t.
//
// The encoding is the one internal/openjd/paramjson.go defines: that file owns
// the encode half, this is the decode half, and they live apart for the same
// dependency reason as parseBoolText above. Any element that does not convert
// makes the WHOLE value unresolved rather than binding a list with a
// wrong-typed hole in it -- a partially-correct list is harder to diagnose
// than an unresolved one, and validation upstream has already reported the
// real defect.
func listFromJSON(t Type, raw string, pathFlavor PathFormat) (Value, error) {
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return Value{}, err
	}
	// A list type carries its element type as its single parameter; there is
	// no Elem() accessor. A CodeList without one is malformed.
	if len(t.Params) != 1 {
		return Value{}, fmt.Errorf("list type %s has no element type", t)
	}
	elemType := t.Params[0]

	out := make([]Value, 0, len(items))
	for _, item := range items {
		v, err := valueFromJSONElem(elemType, item, pathFlavor)
		if err != nil {
			return Value{}, err
		}
		out = append(out, v)
	}
	// NOTE the argument order: List takes (elem, vals).
	return List(elemType, out), nil
}

// valueFromJSONElem converts one JSON element to a Value of type elemType,
// recursing for a nested list.
func valueFromJSONElem(elemType Type, item json.RawMessage, pathFlavor PathFormat) (Value, error) {
	if elemType.Code == CodeList {
		return listFromJSON(elemType, string(item), pathFlavor)
	}

	var scalar any
	if err := json.Unmarshal(item, &scalar); err != nil {
		return Value{}, err
	}
	text, ok := scalarJSONText(scalar)
	if !ok {
		return Value{}, fmt.Errorf("element %s is not a scalar", item)
	}
	v := ValueFromText(elemType, text, pathFlavor)
	if v.Type.Code == CodeUnresolved {
		return Value{}, fmt.Errorf("element %s is not a %s", item, elemType)
	}
	return v, nil
}

// scalarJSONText renders a JSON-decoded scalar losslessly. encoding/json
// produces float64 for every number, so an integral value must format without
// an exponent or a trailing ".0" -- 1 has to stay "1" so an INT element parses.
func scalarJSONText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}
