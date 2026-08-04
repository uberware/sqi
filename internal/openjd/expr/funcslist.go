// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"slices"
)

// listFuncs is RFC 0006's list-function group: range, flatten, sorted,
// reversed, unique, any, all. A later sub-project never edits this table — C2,
// C3 and C4 add their own groups in their own files (funcsstrcase.go,
// funcsstrfind.go, funcsstrsplit.go, funcsstrpad.go, funcsre.go,
// funcsrepr.go, funcspath.go) and their own entry in funcs.go's mergeFuncs
// call.
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
		// RFC 0006 lists list[nulltype] separately, and the row stays for
		// spec fidelity, but this comment used to wrongly claim it is reached
		// the same way the min table's list[nulltype] row is (that reasoning
		// IS correct there, because min's rivals are list[int]/list[float],
		// concrete types that cost 1 to widen into). Here the sibling is
		// list[varT] — an unbound type variable — and argCostList's own
		// carve-out scores an empty argument as an EXACT match (cost 0)
		// against a variable just as much as against this row's own
		// list[nulltype] parameter. So an empty list literal TIES the two
		// rows at cost 0, and matchShapesExactFirst resolves that tie to the
		// EARLIEST shape — the list[varT] row above, never this one. The tie
		// is harmless today only because both rows return args[0] unchanged
		// and both Ret substitute to list[nulltype] for an empty argument, so
		// it is unobservable which row actually ran. It would stop being
		// harmless the moment a later wave gave THIS row a different Fn or
		// Ret from its list[varT] sibling: registered in this order, the
		// list[varT] row would still win the tie and this row's behavior
		// would never run.
		{Params: []Type{ListOf(TNull)}, Ret: ListOf(TNull), Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
	},
	// sorted reuses compareValues rather than defining an order of its own.
	// That function already implements section 1.2.5 for int, float, string,
	// path, bool and — recursively — list, and it is what the list ordering
	// operators use; a second comparator here would be a second implementation
	// of the same section, free to drift from the first. Its refusals come
	// along too, which is why sorting nulls is an error rather than a no-op.
	"sorted": {
		{Params: []Type{ListOf(varT)}, Ret: ListOf(varT), Fn: sortedList},
	},
	"reversed": {
		{Params: []Type{ListOf(varT)}, Ret: ListOf(varT), Fn: func(args []Value) (Value, error) {
			in := args[0].AsList()
			out := make([]Value, len(in))
			for i, v := range in {
				out[len(in)-1-i] = v
			}
			return rebuildList(args[0], out), nil
		}},
	},
	"unique": {
		{Params: []Type{ListOf(varT)}, Ret: ListOf(varT), Fn: uniqueList},
	},
	// any and all are declared over list[bool] and list[nulltype] only. There
	// is no truthiness in this language — section 1.3.5 requires a conditional's
	// condition to be a bool outright — so "any([1, 0])" is a type error rather
	// than a question about zero.
	"any": {
		{Params: []Type{ListOf(TNull)}, Ret: TBool, Fn: func([]Value) (Value, error) {
			return Bool(false), nil
		}},
		{Params: []Type{ListOf(TBool)}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			for _, v := range args[0].AsList() {
				if v.AsBool() {
					return Bool(true), nil
				}
			}
			return Bool(false), nil
		}},
	},
	"all": {
		{Params: []Type{ListOf(TNull)}, Ret: TBool, Fn: func([]Value) (Value, error) {
			return Bool(true), nil
		}},
		{Params: []Type{ListOf(TBool)}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			for _, v := range args[0].AsList() {
				if !v.AsBool() {
					return Bool(false), nil
				}
			}
			return Bool(true), nil
		}},
	},
}

// rangeList builds Python's range as a list.
//
// The count is computed and bounded before anything is allocated: checking
// afterward would be no protection at all, which is the same reason
// checkElementCount exists in limits.go. The arithmetic itself is
// rangeCount's job, not this function's — see its doc comment for why it
// works in uint64 rather than int64.
func rangeList(start, stop, step int64) (Value, error) {
	if step == 0 {
		return Value{}, errors.New("range() requires a non-zero step")
	}
	count := rangeCount(start, stop, step)
	if err := checkElementCount(count); err != nil {
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

// rangeCount computes range()'s element count as a plain int, clamped so that
// an astronomically large true count is reported as "too large" by
// checkElementCount rather than silently wrapping into a small or negative
// number that looks like a legitimate answer.
//
// It works in uint64, not int64, for the SPAN between start and stop: the
// true span between two arbitrary int64 values can be as large as
// math.MaxInt64 - math.MinInt64, i.e. 2^64-1, which does not always fit in an
// int64. Forming it as plain int64 subtraction ("stop - start") can wrap —
// exactly the defect this replaces, where "range(-9223372036854775807,
// 9223372036854775807, 9223372036854775807)" silently computed a span of -1
// and returned an empty list instead of erroring or answering 2. uint64 has
// exactly enough range (2^64 values) to hold that span exactly, with no
// wraparound possible, once the DIRECTION is known.
//
// The direction is decided first, by a plain signed comparison (step > 0 &&
// stop > start, or its mirror): comparing two int64 values is always exact
// regardless of what arithmetic on them might overflow, so this is safe on
// its own and is what tells us the span is genuinely positive before any
// subtraction happens. Only inside a proven-nonempty branch is the span
// itself computed, in uint64.
//
// The step's magnitude is taken as "uint64(-step)" even on the step < 0
// side, and that is safe for step == math.MinInt64 too, which is the one
// value a plain "-step" cannot represent as a positive int64: Go's signed
// negation of math.MinInt64 wraps back to math.MinInt64's own bit pattern,
// and that same bit pattern reinterpreted as uint64 IS math.MinInt64's true
// magnitude (2^63) — so no separate case is needed for it.
//
// The ceiling division (span/mag, plus one for a non-zero remainder) cannot
// overflow uint64 either: the quotient is at most span, and the one case
// that could push a +1 over uint64's own top (mag == 1 with span already at
// 2^64-1) can never have a nonzero remainder to trigger that +1, since
// anything mod 1 is 0.
func rangeCount(start, stop, step int64) int {
	var span, mag uint64
	switch {
	case step > 0 && stop > start:
		span = uint64(stop) - uint64(start) //nolint:gosec // G115: deliberate reinterpretation, not a bounds bug — see the doc comment above
		mag = uint64(step)
	case step < 0 && stop < start:
		span = uint64(start) - uint64(stop) //nolint:gosec // G115: deliberate reinterpretation, not a bounds bug — see the doc comment above
		mag = uint64(-step)
	default:
		return 0
	}
	count := span / mag
	if span%mag != 0 {
		count++
	}
	if count > maxElements {
		// Clamped rather than converted: at this magnitude, int(count) could
		// itself wrap to a small or negative number, which would slip past
		// checkElementCount's bound check the exact way this function exists
		// to prevent. maxElements+1 is a safe, small sentinel that is
		// unambiguously over the bound.
		return maxElements + 1
	}
	return int(count)
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
		// elem, not TNull: an outer list of empty (but concretely typed) inner
		// lists — "flatten([[1, 2][2:2]])" — must still report list[int], not
		// the more permissive list[nulltype] that would mask a downstream type
		// error. elem is already TNull here when the argument itself derives
		// no element type (e.g. flatten([]), whose own type is list[nulltype]),
		// so this does not change that case.
		return List(elem, nil), nil
	}
	vals := make([]Value, 0, total)
	for _, inner := range outer {
		vals = append(vals, inner.AsList()...)
	}
	return List(elem, vals), nil
}

// rebuildList returns a list with the same element type as src and the given
// elements. The type is copied rather than re-inferred so that reordering or
// filtering a list never changes what it is a list OF — an empty result of a
// list[int] stays list[int]'s business, not list[nulltype]'s.
func rebuildList(src Value, vals []Value) Value {
	elem := TNull
	if e, ok := listElem(src.Type); ok {
		elem = e
	}
	return List(elem, vals)
}

// sortedList returns a new list ordered ascending by section 1.2.5's
// comparator. The input is cloned first: nothing in this language mutates, and
// a sort in place would rewrite the caller's list value.
func sortedList(args []Value) (Value, error) {
	in := args[0].AsList()
	out := slices.Clone(in)
	var sortErr error
	slices.SortStableFunc(out, func(a, b Value) int {
		if sortErr != nil {
			return 0
		}
		c, err := compareValues(a, b)
		if err != nil {
			sortErr = err
			return 0
		}
		return c
	})
	if sortErr != nil {
		return Value{}, sortErr
	}
	return rebuildList(args[0], out), nil
}

// uniqueList removes duplicates, keeping the first occurrence of each value.
//
// Equality is valuesEqual — section 1.2.5's cross-type "==" — and NOT
// Value.Equal, which is Go identity and would call 5 and 5.0 different values.
// The comparison is quadratic because Value is not a valid map key (Type
// contains a slice) and because cross-type equality is not an equivalence a
// hash could reproduce anyway. maxElements caps the input, so the worst case is
// bounded.
func uniqueList(args []Value) (Value, error) {
	in := args[0].AsList()
	out := make([]Value, 0, len(in))
	for _, v := range in {
		dup := false
		for _, kept := range out {
			if valuesEqual(kept, v) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return rebuildList(args[0], out), nil
}
