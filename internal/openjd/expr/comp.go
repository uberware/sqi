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
// A UNION is iterable only when EVERY member is, and its element type is the
// unification of the members' own. This is the same rule coerce.go states for a
// union value ("usable only where EVERY member would be") and the same one
// unionResultType already applies to a subscript's and a slice's union
// receiver — a union is SOME ONE of its members at runtime, so an operation
// legal on only some of them is not legal at all.
//
// It is stated here rather than delegated to listElem, which answers a
// different question. listElem SKIPS a union's non-list members, so it calls
// "list[int] | nulltype" a list and "range_expr | list[string]" a
// list[string] — the second of those is not loose acceptance but a WRONG
// INFERRED TYPE, since the value may be a range_expr yielding int. Delegating
// was a deliberate consolidation, and it was the wrong instrument: what it
// fixed was the rejection of "range_expr | list[int]", the union this package
// manufactures itself for a sliced range_expr (sliceResultType), and that case
// falls out of the every-member rule below anyway — both members yield int.
func iterableElem(t Type) (Type, bool) {
	t = unwrapUnresolved(t)
	switch t.Code {
	case CodeList:
		if len(t.Params) == 1 {
			return t.Params[0], true
		}
	case CodeRangeExpr:
		// Section 1.2.3 converts a range_expr to list[int] losslessly.
		return TInt, true
	case CodeUnion:
		return unionIterableElem(t)
	}
	return Type{}, false
}

// unionIterableElem applies the every-member rule to a union receiver,
// unifying the members' element types with section 1.2.6's own unification
// (unifyElemPair) so that "list[int] | list[float]" iterates as float rather
// than being refused for naming two list types.
func unionIterableElem(t Type) (Type, bool) {
	// UnionOf normalizes a union to at least two members, so this is
	// unreachable through the constructors; it is here because the loop below
	// would otherwise index an empty slice.
	if len(t.Params) == 0 {
		return Type{}, false
	}
	acc, ok := iterableElem(t.Params[0])
	if !ok {
		return Type{}, false
	}
	for _, member := range t.Params[1:] {
		elem, ok := iterableElem(member)
		if !ok {
			return Type{}, false
		}
		if acc, ok = unifyElemPair(acc, elem); !ok {
			return Type{}, false
		}
	}
	return acc, true
}

// evalListComp evaluates a list comprehension (spec section 1.3.7).
//
// The element expression is an IDENTITY position for the target type, like a
// list literal's elements, so a list[T] target flows inward to it — on BOTH the
// resolved path (runComp's coerce call) and the unresolved one (unresolvedComp),
// exactly as evalListLit's target flows into both of its own. The filter gets
// TBool and the iterable gets TAny.
func evalListComp(n *ListComp, ec evalCtx, target Type, depth int) (Value, error) {
	iter, err := evalNode(n.Iter, ec, TAny, depth+1)
	if err != nil {
		return Value{}, err
	}
	elemType, ok := iterableElem(iter.Type)
	if !ok {
		return Value{}, errorAt(ec.src, n.Iter.Pos(), "a %s cannot be iterated", iter.Type)
	}
	// Section 1.3.7: "A loop variable that shadows an existing binding is an
	// error." The <UserIdentifier> rule the parser enforces already prevents
	// colliding with a spec-defined symbol like Param; this catches the cases
	// that CAN collide — a "let" binding, or an enclosing loop variable.
	//
	// This is only as precise as the caller's table: expr has no scope
	// information, so a caller that binds names which are not actually in
	// scope at this expression will produce a false rejection. See doc.go.
	if _, bound := ec.syms.Lookup(n.Var); bound {
		return Value{}, errorAt(ec.src, n.VarOffset,
			"the loop variable %q shadows an existing binding", n.Var)
	}

	elemTarget := listElemTarget(target)
	if iter.IsUnresolved() {
		out, err := unresolvedComp(n, ec, elemTarget, elemType, depth)
		if err != nil {
			return Value{}, err
		}
		ec.m.release(iter) // rule 2: consumed determining the comprehension's own placeholder result
		return out, nil
	}

	// RESERVE rule 2's per-element charge before iterItems expands anything.
	// runComp charges it below, but only once the items exist, and for a
	// range_expr iterable "exist" means a full expansion: [x for x in
	// range_expr("1-10000000")] materialized 1.6 GB in 97 ms and only then
	// reported 10,000,001 operations against a limit of 10,000.
	//
	// reserveIterable, NOT elementCount. The first revision of this
	// reservation called elementCount, which for a MULTI-sub-range range_expr
	// expands to produce its answer -- so the reservation's own input did the
	// work it was meant to avert, and on the SUCCESS path the expansion then
	// happened a second time inside iterItems: +72 MB and +36 ms per call on
	// [x for x in range_expr("1-500000,2000000-2400000")], a regression no
	// operation count could see because the counts were identical.
	// reserveIterable is arithmetic (rangeexpr.go).
	//
	// The CHARGE still comes from runComp's own chargeElements(len(items)),
	// which is exact, so an evaluation that survives this is charged the same
	// total it always was -- see meter.reserve.
	if err := reserveIterable(ec, iter); err != nil {
		return Value{}, wrapAt(ec.src, n.Iter.Pos(), err)
	}
	items, err := iterItems(iter)
	if err != nil {
		return Value{}, wrapAt(ec.src, n.Iter.Pos(), err)
	}
	// rule 2/3: the iterable, consumed producing items. Its own elements are
	// never separately tracked: a list receiver's items ALIAS its own backing
	// slice (AsList copies nothing, so they are already counted inside iter's
	// own sizeOf), and a range_expr's items are freshly built ints that were
	// never passed through evalNode's allocation point at all. Either way,
	// releasing the whole iterable here accounts for its contribution in one
	// shot; nothing downstream needs to touch it again.
	ec.m.release(iter)
	// This reads as running AFTER iterItems, but iterItems itself allocates
	// nothing new to check retroactively here: a list's items are the value's
	// own backing slice (AsList copies nothing), and a range_expr already
	// checked its own count against this same bound while expanding (rangeInts
	// self-checks before any Value exists). What this call actually guards is
	// runComp's own upcoming allocations below, sized by len(items) — the
	// "before any allocation" contract, applied to the allocations that are
	// actually downstream of this line.
	if err := checkElementCount(len(items)); err != nil {
		return Value{}, wrapAt(ec.src, n.Offset, err)
	}
	return runComp(n, ec, elemTarget, elemType, items, depth)
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
func unresolvedComp(n *ListComp, ec evalCtx, elemTarget, elemType Type, depth int) (Value, error) {
	scoped := ec
	scoped.syms = &scopedSymbols{parent: ec.syms, name: n.Var, val: Unresolved(elemType)}
	if n.Cond != nil {
		if err := checkCompFilter(n, scoped, depth); err != nil {
			return Value{}, err
		}
	}
	v, err := evalNode(n.Elem, scoped, elemTarget, depth+1)
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
		return Value{}, wrapAt(ec.src, n.Elem.Pos(), cerr)
	}
	ec.m.release(v) // rule 2: consumed determining the placeholder's element type only
	return Unresolved(ListOf(elem)), nil
}

// runComp iterates a concrete iterable, applying the filter and collecting
// element values.
//
// A filter that cannot be decided — an unresolved condition — makes the whole
// result a placeholder, because which elements survive is unknown. That
// mirrors how evalCond and evalCompare treat an unresolved condition, right
// down to reusing the same must-be-a-bool check (see evalCompFilter).
func runComp(n *ListComp, ec evalCtx, elemTarget, elemType Type, items []Value, depth int) (Value, error) {
	// Section 1.3.10 rule 2 names list comprehensions first: iterating every
	// element of the iterable adds the number of elements. Charged against the
	// FULL iterable (items), not whatever survives the filter, matching the
	// spec's own worked example: [x * 2 for x in range(100)] charges 100 here
	// regardless of what the (absent) filter would have kept.
	if err := ec.m.chargeElements(len(items)); err != nil {
		return Value{}, err
	}
	out, types, unresolved, err := collectCompElements(n, ec, elemTarget, items, depth)
	if err != nil {
		return Value{}, err
	}
	if unresolved {
		// rule 2: every element already accumulated in `out` was allocated by
		// evalNode inside collectCompElements, but the whole batch is
		// abandoned in favor of unresolvedComp's own placeholder result --
		// none of it survives.
		for _, v := range out {
			ec.m.release(v)
		}
		return unresolvedComp(n, ec, elemTarget, elemType, depth)
	}
	return buildCompResult(n, ec, elemTarget, out, types)
}

// collectCompElements runs the per-item loop: applying the filter and
// evaluating the element expression for every item that survives it. Split out
// of runComp to keep that function under the repo's complexity cap.
//
// unresolved is true when the filter or an element turned out to have no value
// partway through -- the caller falls back to unresolvedComp in that case, and
// out holds whatever was accumulated before the break, for the caller to
// release.
func collectCompElements(n *ListComp, ec evalCtx, elemTarget Type, items []Value, depth int) (out []Value, types []Type, unresolved bool, err error) {
	out = make([]Value, 0, len(items))
	types = make([]Type, 0, len(items))
	for _, item := range items {
		scoped := ec
		scoped.syms = &scopedSymbols{parent: ec.syms, name: n.Var, val: item}
		if n.Cond != nil {
			keep, known, ferr := evalCompFilter(n, scoped, depth)
			if ferr != nil {
				return nil, nil, false, ferr
			}
			if !known {
				return out, types, true, nil
			}
			if !keep {
				continue
			}
		}
		v, verr := evalNode(n.Elem, scoped, elemTarget, depth+1)
		if verr != nil {
			return nil, nil, false, verr
		}
		if v.IsUnresolved() {
			// rule 2: this element triggered the switch to the unresolved path
			// and is abandoned right along with everything already in `out`
			// -- it never makes it into unresolvedComp's placeholder.
			ec.m.release(v)
			return out, types, true, nil
		}
		out = append(out, v)
		types = append(types, v.Type)
	}
	return out, types, false, nil
}

// buildCompResult unifies the produced elements' types (when the target left
// that open), coerces each element, and builds the result list. Split out of
// runComp to keep that function under the repo's complexity cap.
func buildCompResult(n *ListComp, ec evalCtx, elemTarget Type, out []Value, types []Type) (Value, error) {
	elem := elemTarget
	if elem.Code == CodeAny {
		var ok bool
		elem, ok = unifyElemTypes(types)
		if !ok {
			return Value{}, errorAt(ec.src, n.Offset,
				"the elements this comprehension produces have incompatible types: %s", joinTypes(types))
		}
	}
	converted := make([]Value, len(out))
	for i, v := range out {
		c, err := coerce(v, elem)
		if err != nil {
			return Value{}, wrapAt(ec.src, n.Elem.Pos(), err)
		}
		converted[i] = c
	}
	// rule 3: every produced element is absorbed into the list being built.
	// Release the EXACT pre-coercion values evalNode allocated (out), not the
	// coerced converted[] -- see evalListLit's identical reasoning (list.go).
	// Each element stays live from its own creation until this point, which is
	// exactly what lets a comprehension accumulate many live elements at once
	// (see TestMemoryLimit_CatchesCumulativeWorkThatTheFloorDoesNot) rather
	// than being released one at a time as it is produced.
	for _, v := range out {
		ec.m.release(v)
	}
	return List(elem, converted), nil
}

// evalCompFilter evaluates the filter for one element. known is false when the
// condition has no value yet but could still turn out to be a bool at runtime —
// mirroring evalCond's identical two checks (eval.go): an unresolved condition
// that COULD be a bool defers the decision, but one that could never be a bool
// (section 1.2.2's "any" placeholder for an untyped "let" binding, say) is
// rejected immediately rather than silently deferred forever.
func evalCompFilter(n *ListComp, ec evalCtx, depth int) (keep, known bool, err error) {
	c, err := evalNode(n.Cond, ec, TBool, depth+1)
	if err != nil {
		return false, false, err
	}
	if c.IsUnresolved() {
		if !includes(c.Type, CodeBool) {
			return false, false, errorAt(ec.src, n.Cond.Pos(),
				"a comprehension filter must be a bool, found %s", c.Type)
		}
		ec.m.release(c) // rule 2: consumed determining the filter is unresolved
		return false, false, nil
	}
	if c.Type.Code != CodeBool {
		return false, false, errorAt(ec.src, n.Cond.Pos(),
			"a comprehension filter must be a bool, found %s", c.Type)
	}
	keep = c.AsBool()
	ec.m.release(c) // rule 2: consumed extracting the plain bool; the filter value itself is discarded
	return keep, true, nil
}

// checkCompFilter type-checks a filter without a concrete element, for the
// unresolved path. It rejects a filter that cannot POSSIBLY be a bool, using
// includes rather than an exact code match — the same question evalCond asks of
// an unresolved condition — so a filter whose declared type is "any" or a union
// naming bool among other members is accepted rather than rejected outright.
func checkCompFilter(n *ListComp, ec evalCtx, depth int) error {
	c, err := evalNode(n.Cond, ec, TBool, depth+1)
	if err != nil {
		return err
	}
	if !includes(c.Type, CodeBool) {
		return errorAt(ec.src, n.Cond.Pos(),
			"a comprehension filter must be a bool, found %s", c.Type)
	}
	ec.m.release(c) // rule 2: consumed determining the filter's declared type only
	return nil
}

// reserveIterable refuses a comprehension whose iterable cannot fit the
// remaining operation budget, before iterItems materializes it.
//
// A list is already materialized, so its own length is the exact figure and
// costs nothing to obtain. A range_expr is not, and counting it is not free
// either -- reserveRangeExprExpansion (rangeexpr.go) settles it arithmetically
// rather than by expanding. Any other type is rejected by iterItems itself a
// line later, and reserves nothing here.
func reserveIterable(ec evalCtx, iter Value) error {
	switch iter.Type.Code {
	case CodeList:
		return ec.m.reserveElements(len(iter.AsList()))
	case CodeRangeExpr:
		return reserveRangeExprExpansion(ec, iter)
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
