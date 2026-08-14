// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// errEmptySubstring backs RFC 0006's rule that count, find, rfind, index,
	// rindex and replace's "old" argument must be non-empty.
	errEmptySubstring = errors.New("the substring must not be empty")
	// errEmptySeparator is the same rule for split and rsplit's "sep", kept
	// separate because the two map onto separate conformance fixtures
	// (expr2.2.4--replace-empty-old vs expr2.2.4--split-empty-separator) and
	// because "substring" is the wrong word for a separator. Declared here so
	// funcsstrsplit.go (Task 5) can consume it without a second sentinel.
	errEmptySeparator = errors.New("the separator must not be empty")
	// errSubstringNotFound is index and rindex's failure. find and rfind
	// return -1 for the same condition; only the index pair raises.
	errSubstringNotFound = errors.New("substring not found")
)

// strFindFuncs is sub-project C2's second group: RFC 0006 section 2.2.4's trim
// and affix functions, and (from Task 4) its search functions and replace.
// They share this file because they share the empty-argument sentinels and the
// codepoint-index conversion.
//
// Section 1.3.10 rule 3 (sub-project E1, Task 7): every row in this file
// declares Cost{ArgBytes: []int{0}} -- the RECEIVER's (arg0's) own byte
// length ONLY. None of the second or third string arguments (a cutset,
// prefix, needle, "old" or "new") ever contribute, confirmed by holding the
// receiver fixed and growing the other argument from 10 to 300 bytes without
// moving the count (cost_string_internal_test.go's
// TestOperationCount_StripCutsetDoesNotAffectCost,
// TestOperationCount_AffixArgumentDoesNotAffectRemoveprefixCost,
// TestOperationCount_NeedleLengthDoesNotAffectFindCost). startswith/endswith
// charge the FULL receiver even though a real implementation only needs the
// prefix/suffix span -- the reference does, and rule 3's general clause does
// not carve out an exception for a function whose real work is smaller than
// its receiver.
//
// replace() is the one row here worth a specific note: it can balloon a
// small receiver into an enormous result (old shorter than new, many
// occurrences), and the reference's own operation count does not move when
// that happens -- confirmed with a 100-byte receiver and a 300-byte "new",
// producing a ~30000-byte result at an unchanged count
// (TestOperationCount_ReplaceDoesNotChargeTheProducedResult). sqi follows the
// reference here rather than diverging (unlike join/zfill below in this
// task): rule 3 groups replace() in the SAME sentence as upper()/lower()/
// strip(), which are all confirmed receiver-driven (see funcsstrcase.go's own
// Cost comment), and the growth this exposes is independently bounded by
// maxStringBytes (replaceAll's own checkRepeat call, below) -- a SEPARATE
// limit (section 1.3.9) from the one this Cost declares (section 1.3.10).
var strFindFuncs = map[string][]Shape{
	// The two-argument forms take a SET of runes, not a substring:
	// lstrip("xxayx", "xy") is "ayx", not "xayx". strings.Trim and its
	// siblings already have exactly that cutset semantics, including the
	// no-op on an empty cutset that RFC 0006 wants here.
	"strip": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimSpace(args[0].AsStr())), nil
		}},
		{Params: []Type{TString, TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.Trim(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	"lstrip": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimLeftFunc(args[0].AsStr(), unicode.IsSpace)), nil
		}},
		{Params: []Type{TString, TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimLeft(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	"rstrip": {
		{Params: []Type{TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimRightFunc(args[0].AsStr(), unicode.IsSpace)), nil
		}},
		{Params: []Type{TString, TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimRight(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	// RFC 0006: "Remove prefix if present, otherwise return unchanged" — and
	// an EMPTY prefix is not an error here, unlike the empty substring the
	// search functions reject. strings.TrimPrefix is already both.
	"removeprefix": {
		{Params: []Type{TString, TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimPrefix(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	"removesuffix": {
		{Params: []Type{TString, TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return String(strings.TrimSuffix(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	// An empty prefix or suffix is NOT an error — startswith('abc', '') is
	// true. RFC 0006 states the non-empty requirement for count/find/rfind/
	// index/rindex/replace/split/rsplit and for nothing else.
	"startswith": {
		{Params: []Type{TString, TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return Bool(strings.HasPrefix(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	"endswith": {
		{Params: []Type{TString, TString}, Ret: TBool, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return Bool(strings.HasSuffix(args[0].AsStr(), args[1].AsStr())), nil
		}},
	},
	// Non-overlapping, so count("aaaa","aa") is 2 and not 3. strings.Count is
	// already non-overlapping; the empty-substring guard runs first because
	// strings.Count("abc","") returns 4 rather than erroring.
	"count": {
		{Params: []Type{TString, TString}, Ret: TInt, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			sub := args[1].AsStr()
			if sub == "" {
				return Value{}, errEmptySubstring
			}
			return Int(int64(strings.Count(args[0].AsStr(), sub))), nil
		}},
	},
	"find": {
		{Params: []Type{TString, TString}, Ret: TInt, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			i, err := runeIndexOf(args[0].AsStr(), args[1].AsStr(), false)
			if err != nil {
				return Value{}, err
			}
			return Int(i), nil
		}},
	},
	"rfind": {
		{Params: []Type{TString, TString}, Ret: TInt, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			i, err := runeIndexOf(args[0].AsStr(), args[1].AsStr(), true)
			if err != nil {
				return Value{}, err
			}
			return Int(i), nil
		}},
	},
	// index and rindex differ from find and rfind ONLY in what a miss does:
	// -1 there, an error here. RFC 0006 spells both out.
	"index": {
		{Params: []Type{TString, TString}, Ret: TInt, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return foundIndex(args[0].AsStr(), args[1].AsStr(), false)
		}},
	},
	"rindex": {
		{Params: []Type{TString, TString}, Ret: TInt, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return foundIndex(args[0].AsStr(), args[1].AsStr(), true)
		}},
	},
	"replace": {
		{Params: []Type{TString, TString, TString}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return replaceAll(args[0].AsStr(), args[1].AsStr(), args[2].AsStr())
		}},
	},
}

// runeIndexOf returns the CODEPOINT index of sub within s, or -1 when absent.
//
// Codepoints, not bytes, so that find(), len() and s[i] all count the same
// thing: len(string) is utf8.RuneCountInString (funcsconv.go) and a string
// subscript is rune-indexed (list.go:302), so a byte offset here would make
// s[find(s, sub)] land on the wrong character for any non-ASCII s. The
// reference implementation agrees — find('héllo','l') is 2 there, not 3.
func runeIndexOf(s, sub string, last bool) (int64, error) {
	if sub == "" {
		return 0, errEmptySubstring
	}
	at := strings.Index(s, sub)
	if last {
		at = strings.LastIndex(s, sub)
	}
	if at < 0 {
		return -1, nil
	}
	return int64(utf8.RuneCountInString(s[:at])), nil
}

// foundIndex is runeIndexOf for index() and rindex(), which raise on a miss
// rather than returning -1.
func foundIndex(s, sub string, last bool) (Value, error) {
	i, err := runeIndexOf(s, sub, last)
	if err != nil {
		return Value{}, err
	}
	if i < 0 {
		return Value{}, fmt.Errorf("%w: %q", errSubstringNotFound, sub)
	}
	return Int(i), nil
}

// replaceAll is strings.ReplaceAll with the growth bounded BEFORE anything is
// allocated.
//
// The bound is only interesting when repl is longer than old, and it is
// computed through checkRepeat rather than by forming occurrences*grow first: that
// product is exactly the quantity being bounded, so computing it and checking
// afterward is not a check at all — for a large enough count it wraps int64 and
// sails past the comparison. checkRepeat's own doc comment carries the full
// argument; this is the same hazard C1 hit in range().
func replaceAll(s, old, repl string) (Value, error) {
	if old == "" {
		return Value{}, errEmptySubstring
	}
	if grow := len(repl) - len(old); grow > 0 {
		n := int64(strings.Count(s, old))
		added, err := checkRepeat(grow, n, maxStringBytes)
		if err != nil {
			return Value{}, err
		}
		if err := checkStringBytes(len(s) + int(added)); err != nil {
			return Value{}, err
		}
	}
	return String(strings.ReplaceAll(s, old, repl)), nil
}
