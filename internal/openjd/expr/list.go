// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"strings"
)

// evalListLit evaluates a list literal, inferring its element type per spec
// section 1.2.6.
//
// There are two paths, and which one runs depends on whether a target type
// reached this node. With a target of list[T] every element is coerced to T;
// without one the elements' own types are unified by section 1.2.6's rules and
// then coerced to the result. This is one of the three places a target flows
// INWARD (see evalNode).
func evalListLit(n *ListLit, ec evalCtx, target Type, depth int) (Value, error) {
	elemTarget := listElemTarget(target)
	vals := make([]Value, 0, len(n.Elems))
	types := make([]Type, 0, len(n.Elems))
	unresolved := false
	for _, node := range n.Elems {
		v, err := evalNode(node, ec, elemTarget, depth)
		if err != nil {
			return Value{}, err
		}
		// Section 1.2.6: "A null/None value cannot be an element of a list
		// literal. Including null in a list is always an error." This is
		// separate from the empty-list rule, which gives "[]" the element type
		// nulltype without any element being null.
		if v.IsNull() {
			return Value{}, errorAt(ec.src, node.Pos(), "null cannot be an element of a list")
		}
		if v.IsUnresolved() {
			unresolved = true
		}
		vals = append(vals, v)
		types = append(types, v.Type)
	}

	elem := elemTarget
	if elem.Code == CodeAny {
		var ok bool
		elem, ok = unifyElemTypes(types)
		if !ok {
			return Value{}, errorAt(ec.src, n.Offset,
				"the elements of this list have incompatible types: %s", joinTypes(types))
		}
	}
	if err := checkElementCount(len(vals)); err != nil {
		return Value{}, wrapAt(ec.src, n.Offset, err)
	}
	// An element with no value means the list has no complete payload, so the
	// whole literal is a placeholder of the inferred type. Its elements were
	// still type-checked above, which is the point.
	if unresolved {
		return Unresolved(ListOf(elem)), nil
	}
	out := make([]Value, len(vals))
	for i, v := range vals {
		coerced, err := coerce(v, elem)
		if err != nil {
			return Value{}, wrapAt(ec.src, n.Elems[i].Pos(), err)
		}
		out[i] = coerced
	}
	return List(elem, out), nil
}

// listElemTarget returns the element type a list literal should coerce its
// elements to, or TAny when the target does not name exactly one list type.
//
// Section 1.2.6 says "with a target type context containing exactly one list[T]
// type" — a union naming two different list types does not qualify, because
// there would be no single T to aim at.
func listElemTarget(target Type) Type {
	if c, ok := unresolvedConstraint(target); ok {
		return listElemTarget(c)
	}
	if elem, ok := listElem(target); ok {
		return elem
	}
	// listElem already handles a union internally — "exactly one list type
	// across the members" — and reports ok=false for zero or for more than one
	// differing list member, so there is no further disambiguation to do here:
	// any target that reaches this point has no single T to aim at.
	return TAny
}

// unifyElemTypes implements section 1.2.6's rules 2 to 7: the element type of a
// list literal evaluated with no target type.
//
// An empty list is list[nulltype] (rule 6), which coerces to list[T] for any T.
func unifyElemTypes(ts []Type) (Type, bool) {
	if len(ts) == 0 {
		return TNull, true
	}
	acc := unwrapUnresolved(ts[0])
	for _, t := range ts[1:] {
		var ok bool
		acc, ok = unifyElemPair(acc, unwrapUnresolved(t))
		if !ok {
			return Type{}, false
		}
	}
	return acc, true
}

// unifyElemPair combines two element types into the one that holds both, or
// reports that they are incompatible (rule 7).
func unifyElemPair(a, b Type) (Type, bool) {
	if a.Equal(b) {
		return a, true
	}
	// Rule 2: a mix of int and float is list[float].
	if isNumericPair(a, b) {
		return TFloat, true
	}
	// Rule 3: a mix of path and string is list[string].
	if isPathStringPair(a, b) {
		return TString, true
	}
	// Rules 4 and 5 are rules 2 and 3 one level down (list[int] with
	// list[float] gives list[list[float]], list[path] with list[string] gives
	// list[list[string]]), so they are the same function applied to the element
	// types. list[nulltype] is the empty literal and adopts the other side's
	// element type.
	aElem, aIsList := listElem(a)
	bElem, bIsList := listElem(b)
	if aIsList && bIsList {
		switch {
		case aElem.Code == CodeNull:
			return b, true
		case bElem.Code == CodeNull:
			return a, true
		}
		elem, ok := unifyElemPair(aElem, bElem)
		if !ok {
			return Type{}, false
		}
		return ListOf(elem), true
	}
	return Type{}, false
}

// isNumericPair reports whether a and b are int and float in some order.
func isNumericPair(a, b Type) bool {
	return a.Code == CodeInt && b.Code == CodeFloat || a.Code == CodeFloat && b.Code == CodeInt
}

// isPathStringPair reports whether a and b are path and string in some order.
func isPathStringPair(a, b Type) bool {
	return a.Code == CodePath && b.Code == CodeString || a.Code == CodeString && b.Code == CodePath
}

// evalIndex evaluates a subscript (spec section 2.1.7). The receiver may be a
// list, a string or a range_expr; a path is explicitly excluded by section
// 1.3.8.
//
// An index out of bounds is an ERROR here, in deliberate contrast to a slice,
// whose bounds are clamped (section 2.1.8).
func evalIndex(n *Index, ec evalCtx, depth int) (Value, error) {
	recv, err := evalNode(n.X, ec, TAny, depth)
	if err != nil {
		return Value{}, err
	}
	idx, err := evalNode(n.Idx, ec, TInt, depth)
	if err != nil {
		return Value{}, err
	}
	if !isIntish(idx) {
		return Value{}, errorAt(ec.src, n.Idx.Pos(),
			"a subscript index must be an int, found %s", idx.Type)
	}
	elem, err := indexResultType(recv.Type)
	if err != nil {
		return Value{}, wrapAt(ec.src, n.Offset, err)
	}
	// Either operand missing means the result is typed but has no value. A
	// bounds check is impossible and must NOT be attempted: whether the index
	// is in range is not knowable until the value exists.
	if recv.IsUnresolved() || idx.IsUnresolved() {
		return Unresolved(elem), nil
	}
	out, err := indexValue(recv, idx.AsInt())
	if err != nil {
		return Value{}, wrapAt(ec.src, n.Offset, err)
	}
	return out, nil
}

// isIntish reports whether v can serve as an index: an int, or a placeholder
// constrained to one.
func isIntish(v Value) bool {
	return unwrapUnresolved(v.Type).Code == CodeInt
}

// indexResultType gives the type a subscript of recv produces, per section
// 2.1.7's three signatures. It is the type half of evalIndex, split out so that
// a receiver with no value still yields a typed result.
func indexResultType(recv Type) (Type, error) {
	switch t := unwrapUnresolved(recv); t.Code {
	case CodeList:
		return t.Params[0], nil
	case CodeString:
		return TString, nil
	case CodeRangeExpr:
		return TInt, nil
	case CodeUnion:
		return unionResultType(t, indexResultType)
	case CodePath:
		return Type{}, errors.New(
			"a path cannot be subscripted; use its parts to get its components as a list",
		)
	default:
		return Type{}, errNotSubscriptable(t)
	}
}

// errNotSubscriptable is the single place the "cannot be subscripted" wording
// lives. Both indexResultType (the type-level check) and indexValue (the
// value-level fallback) report it, and the relationship between the two is
// worth stating rather than leaving as two identical-looking literals: the
// type check runs FIRST, on every path into a subscript, so indexValue's own
// default is unreachable — it exists because Go requires the function to
// return something after the switch, not because a receiver can reach it.
func errNotSubscriptable(t Type) error {
	return fmt.Errorf("a %s cannot be subscripted", t)
}

// unionResultType answers a subscript's or a slice's result type for a UNION
// receiver, by mapping every member through fn and combining the answers.
//
// A union receiver is SOME ONE of its members at runtime, so the operation is
// legal exactly when it is legal on every member, and its static result is the
// type that holds all of the per-member results. This is the same rule the
// operator table already applies to a union ARGUMENT (shape.go's
// unionArgValueCost, which is why "Param.Range[:] + [1]" always worked); a
// subscript and a slice do not go through that table, so they need it here.
//
// Without this, three FALSE REJECTIONS at type-check time stood, of
// expressions that cannot fail at runtime. The first is self-inflicted:
// sliceResultType deliberately types a range_expr slice as
// "range_expr | list[int]", and subscripting that union — "Param.Range[:][0]",
// an int under every outcome — was then reported as not subscriptable. The
// other two come from condResult (eval.go), which types a conditional with an
// unknown condition as the union of both branches per section 1.3.1.
func unionResultType(t Type, fn func(Type) (Type, error)) (Type, error) {
	// UnionOf normalizes a union to at least two members, so this is
	// unreachable through the constructors; it is here because the loop below
	// would otherwise index an empty slice.
	if len(t.Params) == 0 {
		return Type{}, fmt.Errorf("a %s names no member type", t)
	}
	acc, err := fn(t.Params[0])
	if err != nil {
		return Type{}, err
	}
	for _, member := range t.Params[1:] {
		r, err := fn(member)
		if err != nil {
			return Type{}, err
		}
		acc = unifyResultPair(acc, r)
	}
	return acc, nil
}

// unifyResultPair combines two per-member results into one.
//
// Section 1.2.6's own unification (unifyElemPair) is used where it applies, so
// an int member and a float member give float rather than "float | int" — the
// same rule that already decides a list literal's element type. It is skipped
// when either side is ITSELF a union, because unifyElemPair reaches through a
// union with listElem and would collapse "list[int]" and
// "range_expr | list[int]" to plain list[int], silently dropping the
// range_expr possibility. In that case, and whenever the two are simply
// unrelated, the union of both is the honest answer, and the operator table
// consumes a union argument correctly.
func unifyResultPair(a, b Type) Type {
	if a.Code != CodeUnion && b.Code != CodeUnion {
		if u, ok := unifyElemPair(a, b); ok {
			return u
		}
	}
	return UnionOf(a, b)
}

// indexValue performs the subscript on a receiver that has one.
func indexValue(recv Value, i int64) (Value, error) {
	switch recv.Type.Code {
	case CodeList:
		elems := recv.AsList()
		at, ok := normalizeIndex(i, int64(len(elems)))
		if !ok {
			return Value{}, fmt.Errorf("index %d is out of bounds for a list of %d", i, len(elems))
		}
		return elems[at], nil
	case CodeString:
		runes := []rune(recv.AsStr())
		at, ok := normalizeIndex(i, int64(len(runes)))
		if !ok {
			return Value{}, fmt.Errorf("index %d is out of bounds for a string of %d characters", i, len(runes))
		}
		return String(string(runes[at])), nil
	case CodeRangeExpr:
		ints, err := rangeInts(recv)
		if err != nil {
			return Value{}, err
		}
		at, ok := normalizeIndex(i, int64(len(ints)))
		if !ok {
			return Value{}, fmt.Errorf("index %d is out of bounds for a range of %d values", i, len(ints))
		}
		return Int(ints[at]), nil
	}
	// Unreachable: indexResultType has already rejected every receiver this
	// switch does not handle. See errNotSubscriptable.
	return Value{}, errNotSubscriptable(recv.Type)
}

// normalizeIndex resolves a possibly negative index against a length, reporting
// false when it falls outside. Section 2.1.7: "-1 is the last element".
func normalizeIndex(i, length int64) (int64, bool) {
	if i < 0 {
		i += length
	}
	if i < 0 || i >= length {
		return 0, false
	}
	return i, true
}

// joinTypes renders a list of types for an error message.
func joinTypes(ts []Type) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.String()
	}
	return strings.Join(names, ", ")
}
