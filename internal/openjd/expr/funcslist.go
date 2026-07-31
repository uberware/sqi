// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "errors"

// listFuncs is RFC 0006's list-function group. Its other members are added by a
// later task of this sub-project.
//
// Note the group is distinct from the CONVERSION named list() in funcsconv.go,
// which turns a range_expr into a list[int]. mergeFuncs panics on a duplicate
// name, so the two cannot silently collide.
var listFuncs = map[string][]Shape{
	"range": {
		{Params: []Type{TInt}, Ret: ListOf(TInt), Fn: func(args []Value) (Value, error) {
			return rangeList(0, args[0].AsInt(), 1)
		}},
		{Params: []Type{TInt, TInt}, Ret: ListOf(TInt), Fn: func(args []Value) (Value, error) {
			return rangeList(args[0].AsInt(), args[1].AsInt(), 1)
		}},
		{Params: []Type{TInt, TInt, TInt}, Ret: ListOf(TInt), Fn: func(args []Value) (Value, error) {
			return rangeList(args[0].AsInt(), args[1].AsInt(), args[2].AsInt())
		}},
	},
	// ORDER IS LOAD-BEARING HERE, and this is the first table in the package
	// where it is. "flatten([[1],[2]])" matches BOTH rows at cost 0 — the
	// nested row binding T to int, the flat row binding T to list[int] — and
	// matchShapesExactFirst breaks an exact tie to the EARLIEST shape. Putting
	// the flat row first would make flatten the identity on every argument.
	"flatten": {
		{Params: []Type{ListOf(ListOf(varT))}, Ret: ListOf(varT), Fn: flattenNested},
		{Params: []Type{ListOf(varT)}, Ret: ListOf(varT), Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
		// RFC 0006 lists list[nulltype] separately, and it is not redundant
		// with the row above: an empty list matches THIS row exactly (see the
		// argCostList note in mathFuncs' min table — list[nulltype] against a
		// list[nulltype] parameter scores costExact) while reaching the
		// list[T] row only by widening, so naming it keeps the selection
		// unambiguous instead of relying on the tie-break twice.
		{Params: []Type{ListOf(TNull)}, Ret: ListOf(TNull), Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
	},
}

// rangeList builds Python's range as a list.
//
// The count is computed ARITHMETICALLY and bounded before anything is
// allocated: checking afterward would be no protection at all, which is the
// same reason checkElementCount exists in limits.go.
func rangeList(start, stop, step int64) (Value, error) {
	if step == 0 {
		return Value{}, errors.New("range() requires a non-zero step")
	}
	var count int64
	if step > 0 {
		if stop > start {
			count = (stop - start + step - 1) / step
		}
	} else if stop < start {
		count = (start - stop - step - 1) / -step
	}
	if err := checkElementCount(int(count)); err != nil {
		return Value{}, err
	}
	if count == 0 {
		// Section 1.2.6: an empty list is list[nulltype], the type every other
		// list type accepts. Declaring list[int] here instead would make
		// "range(0) + ['a']" a type error for a list with no elements in it.
		return List(TNull, nil), nil
	}
	vals := make([]Value, count)
	n := start
	for i := range count {
		vals[i] = Int(n)
		n += step
	}
	return List(TInt, vals), nil
}

// flattenNested concatenates a list of lists into one list.
//
// The element type comes from the ARGUMENT's own type rather than from the
// flattened values, so an outer list of empty inner lists still reports the
// element type its type says it has.
func flattenNested(args []Value) (Value, error) {
	outer := args[0].AsList()
	total := 0
	for _, inner := range outer {
		total += len(inner.AsList())
		if err := checkElementCount(total); err != nil {
			return Value{}, err
		}
	}
	elem := TNull
	if inner, ok := listElem(args[0].Type); ok {
		if e, ok := listElem(inner); ok {
			elem = e
		}
	}
	if total == 0 {
		return List(TNull, nil), nil
	}
	vals := make([]Value, 0, total)
	for _, inner := range outer {
		vals = append(vals, inner.AsList()...)
	}
	return List(elem, vals), nil
}
