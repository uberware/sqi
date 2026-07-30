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
//
// The list case is answered by listElem (coerce.go) rather than re-implemented
// here: listElem already looks through an unresolved constraint and a union
// naming one list type across its members — coerce.go consolidated list-element
// extraction to that one place on purpose, so a second, unguarded reimplementation
// here would drift (it did: it rejected a union like "list[int] | nulltype" that
// listElem resolves).
func iterableElem(t Type) (Type, bool) {
	if elem, ok := listElem(t); ok {
		return elem, true
	}
	if unwrapUnresolved(t).Code == CodeRangeExpr {
		return TInt, true
	}
	return Type{}, false
}

// evalListComp evaluates a list comprehension (spec section 1.3.7).
//
// The element expression is an IDENTITY position for the target type, like a
// list literal's elements, so a list[T] target flows inward to it — on BOTH the
// resolved path (runComp's coerce call) and the unresolved one (unresolvedComp),
// exactly as evalListLit's target flows into both of its own. The filter gets
// TBool and the iterable gets TAny.
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
	// This reads as running AFTER iterItems, but iterItems itself allocates
	// nothing new to check retroactively here: a list's items are the value's
	// own backing slice (AsList copies nothing), and a range_expr already
	// checked its own count against this same bound while expanding (rangeInts
	// self-checks before any Value exists). What this call actually guards is
	// runComp's own upcoming allocations below, sized by len(items) — the
	// "before any allocation" contract, applied to the allocations that are
	// actually downstream of this line.
	if err := checkElementCount(len(items)); err != nil {
		return Value{}, wrapAt(src, n.Offset, err)
	}
	return runComp(n, src, syms, elemTarget, elemType, items, depth)
}

// unresolvedComp handles an iterable with no value: the body is type-checked
// ONCE with the loop variable bound to a placeholder, and the result is a
// placeholder list. There is nothing to iterate and no length to bound.
//
// It is also runComp's fallback when a CONCRETE iterable's filter or element
// turns out unresolved partway through (which element survives, or what it
// produces, is no longer decidable) — elemType is the iterable's element type
// either way, computed once by evalListComp and threaded through rather than
// re-derived from whatever partial results runComp happened to collect.
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
	// Mirrors evalListLit's own unresolved path (list.go): the target's element
	// type wins when there is one, exactly as coerce() makes it win below on the
	// resolved path — the element expression's own inferred type decides it only
	// when the target leaves it open (CodeAny).
	elem := elemTarget
	if elem.Code == CodeAny {
		elem = unwrapUnresolved(v.Type)
	} else if _, cerr := coerceUnresolved(v, elem); cerr != nil {
		// A genuinely incompatible element — a bool element against a
		// list[int] target, say — is reported here exactly as coerce() would
		// report it on the resolved path, rather than silently accepted
		// because there is no concrete value yet to check against the target.
		return Value{}, wrapAt(src, n.Elem.Pos(), cerr)
	}
	return Unresolved(ListOf(elem)), nil
}

// runComp iterates a concrete iterable, applying the filter and collecting
// element values.
//
// A filter that cannot be decided — an unresolved condition — makes the whole
// result a placeholder, because which elements survive is unknown. That
// mirrors how evalCond and evalCompare treat an unresolved condition, right
// down to reusing the same must-be-a-bool check (see evalCompFilter).
func runComp(n *ListComp, src string, syms Symbols, elemTarget, elemType Type, items []Value, depth int) (Value, error) {
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
		return unresolvedComp(n, src, syms, elemTarget, elemType, depth)
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

// evalCompFilter evaluates the filter for one element. known is false when the
// condition has no value yet but could still turn out to be a bool at runtime —
// mirroring evalCond's identical two checks (eval.go): an unresolved condition
// that COULD be a bool defers the decision, but one that could never be a bool
// (section 1.2.2's "any" placeholder for an untyped "let" binding, say) is
// rejected immediately rather than silently deferred forever.
func evalCompFilter(n *ListComp, src string, syms Symbols, depth int) (keep, known bool, err error) {
	c, err := evalNode(n.Cond, src, syms, TBool, depth+1)
	if err != nil {
		return false, false, err
	}
	if c.IsUnresolved() {
		if !includes(c.Type, CodeBool) {
			return false, false, errorAt(src, n.Cond.Pos(),
				"a comprehension filter must be a bool, found %s", c.Type)
		}
		return false, false, nil
	}
	if c.Type.Code != CodeBool {
		return false, false, errorAt(src, n.Cond.Pos(),
			"a comprehension filter must be a bool, found %s", c.Type)
	}
	return c.AsBool(), true, nil
}

// checkCompFilter type-checks a filter without a concrete element, for the
// unresolved path. It rejects a filter that cannot POSSIBLY be a bool, using
// includes rather than an exact code match — the same question evalCond asks of
// an unresolved condition — so a filter whose declared type is "any" or a union
// naming bool among other members is accepted rather than rejected outright.
func checkCompFilter(n *ListComp, src string, syms Symbols, depth int) error {
	c, err := evalNode(n.Cond, src, syms, TBool, depth+1)
	if err != nil {
		return err
	}
	if !includes(c.Type, CodeBool) {
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
