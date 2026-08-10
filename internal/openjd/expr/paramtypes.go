// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
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
// except PATH" (wiki/2023-09-Template-Schemas.md:1246) — NOT by
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
// (wiki/2023-09-Template-Schemas.md:1991). An earlier revision of this
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
//     all. Bool and list have no case here and reach the default branch for
//     EVERY input, valid or not — they stay unresolved regardless of phase.
//     BOOL and LIST[*] are sub-project F's job/task-parameter types and a
//     template cannot declare one yet (the EXPR extension that defines them
//     is not StatusSupported). F MUST add its cases here when that lands,
//     or its own parameters will silently never resolve past a placeholder
//     in phase 3 — this is the one place that mapping lives now that
//     phase 2 and phase 3 share it (see JobParamTypes' doc comment).
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
	default:
		return Unresolved(t)
	}
}
