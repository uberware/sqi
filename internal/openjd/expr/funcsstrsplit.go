// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"slices"
	"strings"
)

// strSplitFuncs is sub-project C2's third group: RFC 0006 section 2.2.4's
// split, rsplit and join.
//
// They are separated from the padding group (funcsstrpad.go) because they are
// bounded by a different quantity — an ELEMENT count here, a BYTE count there —
// and one file holding both limits would give a reader no marker for which
// applies where.
var strSplitFuncs = map[string][]Shape{
	// The no-separator form splits on runs of whitespace with the ends
	// stripped, which is why split('   ') is [] while split('', ',') is [""].
	// Without a maxsplit, splitting from the left and from the right are
	// indistinguishable, so rsplit's one-argument row delegates here rather
	// than running a second scan that could drift from this one.
	"split": {
		{Params: []Type{TString}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return stringList(strings.Fields(args[0].AsStr()))
		}},
		{Params: []Type{TString, TString}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return splitSep(args[0].AsStr(), args[1].AsStr(), -1, false)
		}},
		{Params: []Type{TString, TString, TInt}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return splitSep(args[0].AsStr(), args[1].AsStr(), args[2].AsInt(), false)
		}},
	},
	"rsplit": {
		{Params: []Type{TString}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return stringList(strings.Fields(args[0].AsStr()))
		}},
		{Params: []Type{TString, TString}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return splitSep(args[0].AsStr(), args[1].AsStr(), -1, true)
		}},
		{Params: []Type{TString, TString, TInt}, Ret: ListOf(TString), Fn: func(args []Value) (Value, error) {
			return splitSep(args[0].AsStr(), args[1].AsStr(), args[2].AsInt(), true)
		}},
	},
	// RFC 0006 gives join three rows. The list[nulltype] row is what makes
	// [].join(sep) resolve at all — an empty literal is list[nulltype], not
	// list[string] — and returns "" per the specification.
	//
	// The list[path] row uses pathText, the same accessor len()'s path row
	// uses (funcsconv.go), so a path renders identically wherever it is read.
	// No string coercion happens here: this is a declared row, and C2 relies
	// on coercion only for a SCALAR path reaching a string parameter.
	"join": {
		{Params: []Type{ListOf(TNull), TString}, Ret: TString, Fn: func([]Value) (Value, error) {
			return String(""), nil
		}},
		{Params: []Type{ListOf(TString), TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return joinValues(args[0].AsList(), args[1].AsStr(), func(v Value) string { return v.AsStr() })
		}},
		{Params: []Type{ListOf(TPath), TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return joinValues(args[0].AsList(), args[1].AsStr(), pathText)
		}},
	},
}

// stringList wraps a []string as a list[string] value, bounded first.
func stringList(parts []string) (Value, error) {
	if err := checkElementCount(len(parts)); err != nil {
		return Value{}, err
	}
	vals := make([]Value, len(parts))
	for i, p := range parts {
		vals[i] = String(p)
	}
	return List(TString, vals), nil
}

// splitSep splits s on a non-empty sep, from the left or (when fromRight) the
// right, at most maxsplit times.
//
// A NEGATIVE maxsplit means unlimited, which is Python's rule and the only one
// RFC 0006 leaves room for — it documents maxsplit as "at most maxsplit times"
// and defines no behavior below zero. The reference implementation returns an
// EMPTY LIST for maxsplit = -1, discarding the whole string; that is a defect,
// which will be recorded in test/oracle/baseline.txt with the reference's own
// output when the oracle corpus lands.
func splitSep(s, sep string, maxsplit int64, fromRight bool) (Value, error) {
	if sep == "" {
		return Value{}, errEmptySeparator
	}
	// A maxsplit at or above the number of separators present is the same as
	// unlimited. Clamping here keeps the int64 argument — which a template may
	// set to anything, including MaxInt64 — from reaching the int arithmetic
	// below, where maxsplit+1 would overflow.
	total := int64(strings.Count(s, sep))
	if maxsplit < 0 || maxsplit > total {
		maxsplit = total
	}
	if !fromRight {
		return stringList(strings.SplitN(s, sep, int(maxsplit)+1))
	}
	parts := make([]string, 0, int(maxsplit)+1)
	for ; maxsplit > 0; maxsplit-- {
		at := strings.LastIndex(s, sep)
		if at < 0 {
			break
		}
		parts = append(parts, s[at+len(sep):])
		s = s[:at]
	}
	parts = append(parts, s)
	slices.Reverse(parts)
	return stringList(parts)
}

// joinValues concatenates the rendered elements with sep between them, bounding
// the total BEFORE building it.
//
// The separator contribution goes through checkRepeat for the same reason
// replaceAll's growth does: len(sep) * (n-1) is exactly the quantity being
// bounded, and a list at maxElements with a long separator overflows that
// product before any comparison on it could run.
func joinValues(vals []Value, sep string, text func(Value) string) (Value, error) {
	if len(vals) == 0 {
		return String(""), nil
	}
	total, err := checkRepeat(len(sep), int64(len(vals)-1), maxStringBytes)
	if err != nil {
		return Value{}, err
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = text(v)
		total += int64(len(parts[i]))
		if err := checkStringBytes(int(total)); err != nil {
			return Value{}, err
		}
	}
	var b strings.Builder
	b.Grow(int(total))
	for i, p := range parts {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(p)
	}
	return String(b.String()), nil
}
