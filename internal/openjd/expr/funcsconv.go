// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "unicode/utf8"

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
