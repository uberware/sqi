// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"unicode"
)

// strFindFuncs is sub-project C2's second group: RFC 0006 section 2.2.4's trim
// and affix functions, and (from Task 4) its search functions and replace.
// They share this file because they share the empty-argument sentinels and the
// codepoint-index conversion.
var strFindFuncs = map[string][]Shape{
	// The two-argument forms take a SET of runes, not a substring:
	// lstrip("xxayx", "xy") is "ayx", not "xayx". strings.Trim and its
	// siblings already have exactly that cutset semantics, including the
	// no-op on an empty cutset that RFC 0006 wants here.
	"strip": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimSpace(args[0].AsStr())), nil
		}},
		{Params: []Type{TString, TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.Trim(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	"lstrip": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimLeftFunc(args[0].AsStr(), unicode.IsSpace)), nil
		}},
		{Params: []Type{TString, TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimLeft(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	"rstrip": {
		{Params: []Type{TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimRightFunc(args[0].AsStr(), unicode.IsSpace)), nil
		}},
		{Params: []Type{TString, TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimRight(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	// RFC 0006: "Remove prefix if present, otherwise return unchanged" — and
	// an EMPTY prefix is not an error here, unlike the empty substring the
	// search functions reject. strings.TrimPrefix is already both.
	"removeprefix": {
		{Params: []Type{TString, TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimPrefix(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	"removesuffix": {
		{Params: []Type{TString, TString}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimSuffix(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
}
