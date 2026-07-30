// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "fmt"

// scopedSymbols binds one name on top of a parent table, for the duration of a
// comprehension's body.
//
// It implements Symbols rather than extending the interface: Symbols is
// implemented by internal/openjd, the conformance harness and every test, so
// adding a method would be a breaking change for a need that is entirely
// internal to this package.
type scopedSymbols struct {
	parent Symbols
	name   string
	val    Value
}

// Lookup implements Symbols.
func (s *scopedSymbols) Lookup(name string) (Value, bool) {
	if name == s.name {
		return s.val, true
	}
	return s.parent.Lookup(name)
}

// iterableElem reports the element type of an iterable, and whether the type is
// iterable at all.
//
// Section 1.3.7 describes comprehensions as "transforming and filtering lists",
// so list[T] and range_expr are the only iterables. A range_expr yields int
// because section 1.2.3 converts it to list[int] losslessly. A STRING is
// deliberately NOT iterable: the spec never defines iterating one, and section
// 2.1.2 makes "in" on a string a substring test, so treating it as a sequence
// of characters here would invent semantics.
func iterableElem(t Type) (Type, bool) {
	switch u := unwrapUnresolved(t); u.Code {
	case CodeList:
		return u.Params[0], true
	case CodeRangeExpr:
		return TInt, true
	}
	return Type{}, false
}

// evalListComp evaluates a list comprehension (spec section 1.3.7).
//
// The element expression is an IDENTITY position for the target type, like a
// list literal's elements, so a list[T] target flows inward to it. The filter
// gets TBool and the iterable gets TAny.
func evalListComp(n *ListComp, src string, syms Symbols, target Type, depth int) (Value, error) {
	iter, err := evalNode(n.Iter, src, syms, TAny, depth+1)
	if err != nil {
		return Value{}, err
	}
	elemType, ok := iterableElem(iter.Type)
	if !ok {
		return Value{}, errorAt(src, n.Iter.Pos(), "a %s cannot be iterated", iter.Type)
	}
	// Section 1.3.7: "A loop variable that shadows an existing binding is an
	// error." The <UserIdentifier> rule the parser enforces already prevents
	// colliding with a spec-defined symbol like Param; this catches the cases
	// that CAN collide — a "let" binding, or an enclosing loop variable.
	//
	// This is only as precise as the caller's table: expr has no scope
	// information, so a caller that binds names which are not actually in
	// scope at this expression will produce a false rejection. See doc.go.
	if _, bound := syms.Lookup(n.Var); bound {
		return Value{}, errorAt(src, n.VarOffset,
			"the loop variable %q shadows an existing binding", n.Var)
	}

	elemTarget := listElemTarget(target)
	if iter.IsUnresolved() {
		return unresolvedComp(n, src, syms, elemTarget, elemType, depth)
	}

	items, err := iterItems(iter)
	if err != nil {
		return Value{}, wrapAt(src, n.Iter.Pos(), err)
	}
	if err := checkElementCount(len(items)); err != nil {
		return Value{}, wrapAt(src, n.Offset, err)
	}
	return runComp(n, src, syms, elemTarget, items, depth)
}

// unresolvedComp handles an iterable with no value: the body is type-checked
// ONCE with the loop variable bound to a placeholder, and the result is a
// placeholder list. There is nothing to iterate and no length to bound.
func unresolvedComp(n *ListComp, src string, syms Symbols, elemTarget, elemType Type, depth int) (Value, error) {
	scoped := &scopedSymbols{parent: syms, name: n.Var, val: Unresolved(elemType)}
	if n.Cond != nil {
		if err := checkCompFilter(n, src, scoped, depth); err != nil {
			return Value{}, err
		}
	}
	v, err := evalNode(n.Elem, src, scoped, elemTarget, depth+1)
	if err != nil {
		return Value{}, err
	}
	return Unresolved(ListOf(unwrapUnresolved(v.Type))), nil
}

// runComp iterates a concrete iterable, applying the filter and collecting
// element values.
//
// A filter that cannot be decided — an unresolved condition — makes the whole
// result a placeholder, because which elements survive is unknown. That
// mirrors how evalCond and evalCompare treat an unresolved condition.
func runComp(n *ListComp, src string, syms Symbols, elemTarget Type, items []Value, depth int) (Value, error) {
	out := make([]Value, 0, len(items))
	types := make([]Type, 0, len(items))
	unresolved := false
	for _, item := range items {
		scoped := &scopedSymbols{parent: syms, name: n.Var, val: item}
		if n.Cond != nil {
			keep, known, err := evalCompFilter(n, src, scoped, depth)
			if err != nil {
				return Value{}, err
			}
			if !known {
				unresolved = true
				break
			}
			if !keep {
				continue
			}
		}
		v, err := evalNode(n.Elem, src, scoped, elemTarget, depth+1)
		if err != nil {
			return Value{}, err
		}
		if v.IsUnresolved() {
			unresolved = true
			break
		}
		out = append(out, v)
		types = append(types, v.Type)
	}
	if unresolved {
		return unresolvedComp(n, src, syms, elemTarget, elemTypeOf(items), depth)
	}
	elem := elemTarget
	if elem.Code == CodeAny {
		var ok bool
		elem, ok = unifyElemTypes(types)
		if !ok {
			return Value{}, errorAt(src, n.Offset,
				"the elements this comprehension produces have incompatible types: %s", joinTypes(types))
		}
	}
	converted := make([]Value, len(out))
	for i, v := range out {
		c, err := coerce(v, elem)
		if err != nil {
			return Value{}, wrapAt(src, n.Elem.Pos(), err)
		}
		converted[i] = c
	}
	return List(elem, converted), nil
}

// elemTypeOf reports the element type of already-evaluated items, for the
// fallback to the unresolved path. An empty slice has no element to inspect, so
// it reports nulltype — the same type section 1.2.6 rule 6 gives "[]".
func elemTypeOf(items []Value) Type {
	if len(items) == 0 {
		return TNull
	}
	return unwrapUnresolved(items[0].Type)
}

// evalCompFilter evaluates the filter for one element. known is false when the
// condition has no value yet.
func evalCompFilter(n *ListComp, src string, syms Symbols, depth int) (keep, known bool, err error) {
	c, err := evalNode(n.Cond, src, syms, TBool, depth+1)
	if err != nil {
		return false, false, err
	}
	if c.IsUnresolved() {
		return false, false, nil
	}
	if c.Type.Code != CodeBool {
		return false, false, errorAt(src, n.Cond.Pos(),
			"a comprehension filter must be a bool, found %s", c.Type)
	}
	return c.AsBool(), true, nil
}

// checkCompFilter type-checks a filter without a concrete element, for the
// unresolved path. It rejects a filter that cannot be a bool.
func checkCompFilter(n *ListComp, src string, syms Symbols, depth int) error {
	c, err := evalNode(n.Cond, src, syms, TBool, depth+1)
	if err != nil {
		return err
	}
	if t := unwrapUnresolved(c.Type); t.Code != CodeBool {
		return errorAt(src, n.Cond.Pos(),
			"a comprehension filter must be a bool, found %s", c.Type)
	}
	return nil
}

// iterItems materializes an iterable's elements. A range_expr is expanded,
// which enforces the size bound as a side effect.
func iterItems(iter Value) ([]Value, error) {
	switch iter.Type.Code {
	case CodeList:
		return iter.AsList(), nil
	case CodeRangeExpr:
		ints, err := rangeInts(iter)
		if err != nil {
			return nil, err
		}
		return intValues(ints), nil
	}
	return nil, fmt.Errorf("a %s cannot be iterated", iter.Type)
}
