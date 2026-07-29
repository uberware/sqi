// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "strings"

// evalListLit evaluates a list literal, inferring its element type per spec
// section 1.2.6.
//
// There are two paths, and which one runs depends on whether a target type
// reached this node. With a target of list[T] every element is coerced to T;
// without one the elements' own types are unified by section 1.2.6's rules and
// then coerced to the result. This is one of the three places a target flows
// INWARD (see evalNode).
func evalListLit(n *ListLit, src string, syms Symbols, target Type) (Value, error) {
	elemTarget := listElemTarget(target)
	vals := make([]Value, 0, len(n.Elems))
	types := make([]Type, 0, len(n.Elems))
	unresolved := false
	for _, node := range n.Elems {
		v, err := evalNode(node, src, syms, elemTarget)
		if err != nil {
			return Value{}, err
		}
		// Section 1.2.6: "A null/None value cannot be an element of a list
		// literal. Including null in a list is always an error." This is
		// separate from the empty-list rule, which gives "[]" the element type
		// nulltype without any element being null.
		if v.IsNull() {
			return Value{}, errorAt(src, node.Pos(), "null cannot be an element of a list")
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
			return Value{}, errorAt(src, n.Offset,
				"the elements of this list have incompatible types: %s", joinTypes(types))
		}
	}
	if err := checkElementCount(len(vals)); err != nil {
		return Value{}, wrapAt(src, n.Offset, err)
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
			return Value{}, wrapAt(src, n.Elems[i].Pos(), err)
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
	if target.Code == CodeUnresolved && len(target.Params) == 1 {
		return listElemTarget(target.Params[0])
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
	// Rule 3: a mix of int and float is list[float].
	if isNumericPair(a, b) {
		return TFloat, true
	}
	// Rule 4: a mix of path and string is list[string].
	if isPathStringPair(a, b) {
		return TString, true
	}
	// Rule 5 is rules 3 and 4 one level down, so it is the same function
	// applied to the element types. list[nulltype] is the empty literal and
	// adopts the other side's element type.
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

// unwrapUnresolved returns a placeholder's constraint, or t unchanged. An
// element with no value still has a type, and that type is what unification
// works on.
func unwrapUnresolved(t Type) Type {
	if t.Code == CodeUnresolved && len(t.Params) == 1 {
		return t.Params[0]
	}
	return t
}

// joinTypes renders a list of types for an error message.
func joinTypes(ts []Type) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.String()
	}
	return strings.Join(names, ", ")
}
