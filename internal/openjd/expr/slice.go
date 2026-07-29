// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
)

// evalSlice evaluates a slice (spec sections 1.3.8 and 2.1.8).
//
// Slice semantics follow Python: out-of-bounds bounds are CLAMPED rather than
// rejected — in deliberate contrast to a subscript, where an out-of-bounds index
// is an error — negative indices count from the end, and a step of zero is an
// error.
func evalSlice(n *Slice, src string, syms Symbols) (Value, error) {
	recv, err := evalNode(n.X, src, syms, TAny)
	if err != nil {
		return Value{}, err
	}
	start, startOK, err := sliceComponent(n.Start, src, syms)
	if err != nil {
		return Value{}, err
	}
	stop, stopOK, err := sliceComponent(n.Stop, src, syms)
	if err != nil {
		return Value{}, err
	}
	step, stepOK, err := sliceComponent(n.Step, src, syms)
	if err != nil {
		return Value{}, err
	}
	result, err := sliceResultType(recv.Type, step)
	if err != nil {
		return Value{}, wrapAt(src, n.Offset, err)
	}
	if recv.IsUnresolved() || !startOK || !stopOK || !stepOK {
		return Unresolved(result), nil
	}
	if step != nil && *step == 0 {
		return Value{}, errorAt(src, n.Step.Pos(), "a slice step cannot be 0")
	}
	out, err := sliceValue(recv, start, stop, step)
	if err != nil {
		return Value{}, wrapAt(src, n.Offset, err)
	}
	return out, nil
}

// sliceComponent evaluates one optional slice component. It returns (nil, true,
// nil) when the component is ABSENT, which is not the same as zero: section
// 1.3.8's defaults depend on the sign of the step. ok is false when the
// component has no value yet.
func sliceComponent(n Node, src string, syms Symbols) (val *int64, ok bool, err error) {
	if n == nil {
		return nil, true, nil
	}
	v, err := evalNode(n, src, syms, TInt)
	if err != nil {
		return nil, false, err
	}
	if !isIntish(v) {
		return nil, false, errorAt(src, n.Pos(),
			"a slice component must be an int, found %s", v.Type)
	}
	if v.IsUnresolved() {
		return nil, false, nil
	}
	i := v.AsInt()
	return &i, true, nil
}

// sliceResultType gives the type a slice of recv produces, per section 2.1.8's
// three signatures.
//
// The range_expr row is the awkward one, and only a NEGATIVE step is knowable
// from the step alone: section 2.1.8 makes that unconditionally a list[int]
// ("range_expr cannot represent descending sequences"), regardless of what
// else is unknown. This function only ever runs on evalSlice's placeholder
// path, where the receiver, start or stop is not yet known (a fully resolved
// slice bypasses it and asks sliceValue directly) — so for anything else
// (a positive step, an absent one, defaulting to positive, or a step whose own
// sign is not yet known) there is no length to check the selection against,
// and an empty selection is ALSO a list[int] regardless of a positive step
// (see sliceRangeExpr): the honest answer is the union of both.
func sliceResultType(recv Type, step *int64) (Type, error) {
	switch t := unwrapUnresolved(recv); t.Code {
	case CodeList:
		return t, nil
	case CodeString:
		return TString, nil
	case CodeRangeExpr:
		if step != nil && *step < 0 {
			return ListOf(TInt), nil
		}
		return UnionOf(TRangeExpr, ListOf(TInt)), nil
	case CodeUnion:
		// Every member sliced, then combined — see unionResultType (list.go).
		// The union this very function returns one case above is the first
		// thing that lands here: "Param.Range[:][0:1]" slices a
		// "range_expr | list[int]".
		return unionResultType(t, func(m Type) (Type, error) {
			return sliceResultType(m, step)
		})
	case CodePath:
		return Type{}, errors.New(
			"a path cannot be sliced; use its parts to get its components as a list",
		)
	default:
		return Type{}, fmt.Errorf("a %s cannot be sliced", t)
	}
}

// sliceValue performs the slice on a receiver that has one.
func sliceValue(recv Value, start, stop, step *int64) (Value, error) {
	switch recv.Type.Code {
	case CodeList:
		elems := recv.AsList()
		idx := sliceIndices(start, stop, step, int64(len(elems)))
		out := make([]Value, len(idx))
		for i, at := range idx {
			out[i] = elems[at]
		}
		elemType := TNull
		if e, ok := listElem(recv.Type); ok {
			elemType = e
		}
		return List(elemType, out), nil
	case CodeString:
		runes := []rune(recv.AsStr())
		idx := sliceIndices(start, stop, step, int64(len(runes)))
		out := make([]rune, len(idx))
		for i, at := range idx {
			out[i] = runes[at]
		}
		return String(string(out)), nil
	case CodeRangeExpr:
		return sliceRangeExpr(recv, start, stop, step)
	}
	return Value{}, fmt.Errorf("a %s cannot be sliced", recv.Type)
}

// sliceRangeExpr slices a range expression, which section 2.1.8 says is treated
// as an integer list rather than as its text.
//
// A positive step gives back a range_expr, which means deriving range text from
// the selected integers. An EMPTY result cannot be a range_expr at all — section
// 2.2.1 makes range_expr("") an error — so it comes back as an empty list[int],
// the same type a negative step gives.
func sliceRangeExpr(recv Value, start, stop, step *int64) (Value, error) {
	ints, err := rangeInts(recv)
	if err != nil {
		return Value{}, err
	}
	idx := sliceIndices(start, stop, step, int64(len(ints)))
	picked := make([]int64, len(idx))
	for i, at := range idx {
		picked[i] = ints[at]
	}
	if step != nil && *step < 0 || len(picked) == 0 {
		vals := make([]Value, len(picked))
		for i, n := range picked {
			vals[i] = Int(n)
		}
		return List(TInt, vals), nil
	}
	return RangeExpr(canonicalRange(picked))
}

// sliceIndices resolves a slice's components into the concrete indices it
// selects, following Python: negative components count from the end,
// out-of-range components are clamped, and an absent component defaults
// according to the sign of the step (section 1.3.8).
//
// A step of zero never reaches here; evalSlice rejects it first.
func sliceIndices(start, stop, step *int64, length int64) []int64 {
	by := int64(1)
	if step != nil {
		by = *step
	}
	var from, to int64
	if by > 0 {
		from, to = 0, length
	} else {
		from, to = length-1, -1
	}
	if start != nil {
		from = clampSliceBound(*start, length, by > 0)
	}
	if stop != nil {
		to = clampSliceBound(*stop, length, by > 0)
	}
	var out []int64
	if by > 0 {
		for i := from; i < to; i += by {
			out = append(out, i)
		}
		return out
	}
	for i := from; i > to; i += by {
		out = append(out, i)
	}
	return out
}

// clampSliceBound resolves one negative-or-out-of-range slice bound into an
// index the loop can use. The lower clamp differs by direction: a forward slice
// stops at 0, a reverse slice at -1, which is the "one before the start"
// sentinel its loop compares against.
func clampSliceBound(i, length int64, forward bool) int64 {
	if i < 0 {
		i += length
	}
	lo := int64(0)
	if !forward {
		lo = -1
	}
	hi := length
	if !forward {
		hi = length - 1
	}
	if i < lo {
		return lo
	}
	if i > hi {
		return hi
	}
	return i
}
