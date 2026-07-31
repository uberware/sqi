// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// Section 2.2.1's string(). The scalar row's union is RFC 0006's own
	// signature; Value.String() already implements every member of it —
	// "null" for nulltype, the JSON spellings for bool, formatFloat for float,
	// the payload text for string, path and range_expr.
	"string": {
		{
			Params: []Type{UnionOf(TBool, TInt, TFloat, TString, TPath, TRangeExpr, TNull)},
			Ret:    TString,
			Fn: func(args []Value) (Value, error) {
				s := args[0].String()
				if err := checkStringBytes(len(s)); err != nil {
					return Value{}, err
				}
				return String(s), nil
			},
		},
		// RFC 0006 calls this "the JSON string representation", and it is NOT
		// Value.String(): that renders a list's string elements unquoted
		// ("[a, b]") as a diagnostic form, which is a known divergence deferred
		// to sub-project E. This one quotes them ("[\"a\", \"b\"]"), matching
		// the reference implementation. Two renderings, two functions, on
		// purpose.
		{Params: []Type{ListOf(varT)}, Ret: TString, Fn: func(args []Value) (Value, error) {
			s, err := jsonList(args[0])
			if err != nil {
				return Value{}, err
			}
			return String(s), nil
		}},
	},
	"list": {
		{Params: []Type{TRangeExpr}, Ret: ListOf(TInt), Fn: func(args []Value) (Value, error) {
			ints, err := rangeInts(args[0])
			if err != nil {
				return Value{}, err
			}
			vals := make([]Value, len(ints))
			for i, n := range ints {
				vals[i] = Int(n)
			}
			return List(TInt, vals), nil
		}},
	},
	"range_expr": {
		{Params: []Type{TString}, Ret: TRangeExpr, Fn: func(args []Value) (Value, error) {
			return RangeExpr(args[0].AsStr())
		}},
		{Params: []Type{ListOf(TInt)}, Ret: TRangeExpr, Fn: func(args []Value) (Value, error) {
			elems := args[0].AsList()
			if len(elems) == 0 {
				return Value{}, errors.New("range_expr() requires at least one value")
			}
			ints := make([]int64, len(elems))
			for i, e := range elems {
				ints[i] = e.AsInt()
			}
			// canonicalRange assumes its input is already sorted and
			// de-duplicated — B2 always fed it that way from an expanded
			// range, and this is the first caller that cannot assume it of
			// its own input.
			slices.Sort(ints)
			ints = slices.Compact(ints)
			return RangeExpr(canonicalRange(ints))
		}},
	},
	// fail() needs no special handling anywhere else, and that is worth
	// knowing rather than rediscovering. With a resolved argument it errors
	// here; with an unresolved one callFunction returns a placeholder before Fn
	// runs, because nothing has actually failed while the value is unknown. Its
	// noreturn return type then collapses out of any union it lands in
	// (collectUnionMembers, type.go), which is what makes
	// "x if c else fail(...)" typed x rather than x?.
	"fail": {
		{Params: []Type{TString}, Ret: TNoReturn, Fn: func(args []Value) (Value, error) {
			return Value{}, errors.New(args[0].AsStr())
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

// jsonList renders a list as RFC 0006's JSON representation for string().
//
// Strings, paths and range expressions are quoted and escaped; numbers,
// booleans and null are bare; nested lists recurse. The separator is ", "
// rather than JSON's bare comma, matching the reference implementation, which
// the differential test compares against character for character.
func jsonList(v Value) (string, error) {
	var b strings.Builder
	if err := writeJSONValue(&b, v); err != nil {
		return "", err
	}
	if err := checkStringBytes(b.Len()); err != nil {
		return "", err
	}
	return b.String(), nil
}

// writeJSONValue is jsonList's recursive worker.
func writeJSONValue(b *strings.Builder, v Value) error {
	switch v.Type.Code {
	case CodeList:
		b.WriteByte('[')
		for i, elem := range v.AsList() {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writeJSONValue(b, elem); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case CodeString, CodePath, CodeRangeExpr:
		quoted, err := json.Marshal(v.s)
		if err != nil {
			return err
		}
		b.Write(quoted)
	default:
		// bool, int, float and null all render bare, and Value.String()
		// already spells each of them the way JSON does.
		b.WriteString(v.String())
	}
	if err := checkStringBytes(b.Len()); err != nil {
		return err
	}
	return nil
}
