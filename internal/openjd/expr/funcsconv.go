// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// convFuncs is RFC 0006's general-function group: the conversions between
// scalar types, plus len. Its other members and the validation function fail
// are added by later tasks of this sub-project.
var convFuncs = map[string][]Shape{
	"len": {
		{Params: []Type{ListOf(varT)}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return Int(int64(len(args[0].AsList()))), nil
		}},
		{Params: []Type{TString}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return Int(int64(utf8.RuneCountInString(args[0].AsStr()))), nil
		}},
		// RFC 0006: "Length of path's string representation". Codepoints, like
		// the string row above, so len(p) and len(string(p)) agree.
		{Params: []Type{TPath}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return Int(int64(utf8.RuneCountInString(pathText(args[0])))), nil
		}},
		// RFC 0006: "Number of values in range expression" — the expanded
		// count, not the text length, so len(range_expr("1-10")) is 10.
		{Params: []Type{TRangeExpr}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			ints, err := rangeInts(args[0])
			if err != nil {
				return Value{}, err
			}
			return Int(int64(len(ints))), nil
		}},
	},
	// RFC 0006's bool(). The scalar rows are conversions; the path and list
	// rows exist ONLY to carry the wording RFC 0006 demands, which is why they
	// return noreturn. Section 1.2.3 does not define a string -> bool coercion
	// at all (scalarCoercible has no CodeBool arm), so the accepted spellings
	// are this function's own behavior and not the coercion matrix's.
	"bool": {
		{Params: []Type{TBool}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
		{Params: []Type{TNull}, Ret: TBool, Fn: func([]Value) (Value, error) {
			return Bool(false), nil
		}},
		{Params: []Type{TInt}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(args[0].AsInt() != 0), nil
		}},
		{Params: []Type{TFloat}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(args[0].AsFloat() != 0), nil
		}},
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return boolFromString(args[0].AsStr())
		}},
		{Params: []Type{TPath}, Ret: TNoReturn, Fn: func([]Value) (Value, error) {
			//nolint:staticcheck // ST1005: capitalized verbatim per RFC 0006's own wording, pinned by TestBool_RejectsWithTheSpecifiedWording
			return Value{}, errors.New("Cannot convert path to bool")
		}},
		{Params: []Type{ListOf(varT)}, Ret: TNoReturn, Fn: func([]Value) (Value, error) {
			//nolint:staticcheck // ST1005: capitalized verbatim per RFC 0006's own wording, pinned by TestBool_RejectsWithTheSpecifiedWording
			return Value{}, errors.New("Cannot convert list to bool")
		}},
	},
	// RFC 0006 writes int and float with UNION parameters, and that is
	// load-bearing rather than cosmetic. matchShapesExactFirst refuses a
	// conversion that can fail when SELECTING an overload — the rule that stops
	// "1 + 2.5" matching (int, int) by discarding the .5 — so a narrow
	// int(value: int) row would report int(3.75) as "no signature accepts
	// (float)" instead of the non-destructive-conversion error RFC 0006 wants.
	// A union parameter is matched member-wise at no cost, passes the value
	// through unconverted (coerce's directUnionMember), and leaves the real
	// conversion to Fn, where its failure is the diagnostic.
	"int": {
		{Params: []Type{UnionOf(TInt, TFloat, TString)}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return coerce(args[0], TInt)
		}},
	},
	"float": {
		{Params: []Type{UnionOf(TInt, TFloat, TString)}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			v, err := coerce(args[0], TFloat)
			if err != nil {
				return Value{}, err
			}
			// Back through floatValue even though coerce produced the number:
			// section 1.3.4 forbids infinity and NaN, and float("inf") reaches
			// this row as a perfectly ordinary string conversion. Letting
			// coerce's result out directly would admit the one value the
			// language says cannot exist.
			return floatValue(v.AsFloat())
		}},
	},
}

// pathText returns a path value's text.
//
// AsStr cannot be used: it panics on anything but CodeString, and a path
// carries its text in the same payload field under a different type code. The
// direct read follows compareValues' precedent in ops.go.
func pathText(v Value) string {
	v.mustBe(CodePath)
	return v.s
}

// boolFromString applies RFC 0006's string-to-bool table, which is
// case-insensitive and closed: anything not listed is an error rather than a
// truthiness judgement. The language has no truthiness — section 1.3.5 requires
// a conditional's condition to be a bool outright — so guessing here would be
// the only place it crept in.
func boolFromString(s string) (Value, error) {
	switch strings.ToLower(s) {
	case "1", "true", "on", "yes":
		return Bool(true), nil
	case "0", "false", "off", "no":
		return Bool(false), nil
	}
	return Value{}, fmt.Errorf("cannot convert %q to bool", s)
}
