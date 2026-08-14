// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"strconv"
)

// This file implements section 1.2.3, implicit type coercion.
//
// The spec's rules are phrased against what the TARGET does not include — "int
// to float when the target types do not include int" — so every rule below asks
// includes() about the target rather than examining the source alone.
//
// Two entry points, because two callers need different things. Shape matching
// (shape.go) asks only whether a type COULD reach a declared parameter type: it
// must not convert anything before a shape is chosen, and on the unresolved path
// there is no value to convert. coerce() performs the conversion afterward.

// includes reports whether target admits a value whose type code is c, looking
// through union members and through an unresolved constraint.
func includes(target Type, c Code) bool {
	switch target.Code {
	case CodeAny:
		return true
	case CodeUnresolved:
		constraint, ok := unresolvedConstraint(target)
		return ok && includes(constraint, c)
	case CodeUnion:
		for _, m := range target.Params {
			if includes(m, c) {
				return true
			}
		}
		return false
	}
	return target.Code == c
}

// coercible reports whether a value of type from can be implicitly converted to
// the target type to, per section 1.2.3. It answers at the type level only, and
// performs nothing.
func coercible(from, to Type) bool {
	if from.Equal(to) || to.Code == CodeAny {
		return true
	}
	// Unresolved is transparent on both sides: what a placeholder can coerce to
	// is decided by its constraint, and a target that is itself unresolved
	// constrains no more tightly than the constraint does.
	if from.Code == CodeUnresolved {
		constraint, ok := unresolvedConstraint(from)
		return ok && coercible(constraint, to)
	}
	if to.Code == CodeUnresolved {
		constraint, ok := unresolvedConstraint(to)
		return ok && coercible(from, constraint)
	}
	// Null coerces to any target that admits null, and to nothing else -- the
	// type-level counterpart of coerce()'s own v.IsNull() rule (below), needed
	// here because coerceUnresolved narrows a PLACEHOLDER's constraint through
	// coercible rather than converting a concrete value through coerce.
	// Without this rule, an unresolved union member of type nulltype -- for
	// example the "else None" branch of "{{ Param.S if Param.Flag else None
	// }}", still unresolved because Param.Flag has no phase-1 value -- was
	// rejected even against a target that obviously admits null (section
	// 1.3.2's TargetArgItem, "string? | list[string]"), because isScalarCode
	// deliberately excludes nulltype from the catch-all scalar rule below --
	// that exclusion is about VALUE conversion ("null coerces to nothing"),
	// not about whether a target that already NAMES null admits it unchanged.
	// A concrete null value never reached this gap (coerce's own IsNull()
	// branch has always handled it); only the still-unresolved, still-a-type
	// case did.
	if from.Code == CodeNull {
		return includes(to, CodeNull)
	}
	// A union value is usable only where EVERY member would be: it is SOME ONE
	// of its members, decided at runtime, so a target that would reject any one
	// of them cannot safely receive it. This is the dual of shape.go's
	// unionArgCost, which scores a union on the PARAMETER side by trying each
	// member and keeping the best — correct there because the caller picks
	// which member satisfies it. Here the union itself picks which member it
	// is, so every member must clear the bar.
	if from.Code == CodeUnion {
		for _, m := range from.Params {
			if !coercible(m, to) {
				return false
			}
		}
		return true
	}
	if coercibleConditional(from, to) {
		return true
	}
	if coercibleList(from, to) {
		return true
	}
	// Any scalar value when the target types have a single scalar type. This is
	// the catch-all, so it runs last: the conditional rules above are narrower
	// and must win where they apply.
	if isScalarCode(from.Code) {
		if c, ok := singleScalarTarget(to); ok {
			return scalarCoercible(from.Code, c)
		}
	}
	return false
}

// coercibleConditional covers the four rules that fire only when the target does
// NOT already admit the source type, so that a value reaches a target that wants
// it unchanged rather than being converted needlessly.
func coercibleConditional(from, to Type) bool {
	switch from.Code {
	case CodeInt:
		return includes(to, CodeFloat) && !includes(to, CodeInt)
	case CodePath:
		return includes(to, CodeString) && !includes(to, CodePath)
	case CodeRangeExpr:
		if includes(to, CodeString) && !includes(to, CodeRangeExpr) {
			return true
		}
		// range_expr -> list[int] when the target includes list[int] but not
		// range_expr. This is the type-level half; coerceList below performs
		// the conversion.
		if !includes(to, CodeRangeExpr) {
			if el, ok := listElem(to); ok && el.Code == CodeInt {
				return true
			}
		}
	}
	return false
}

// coercibleList covers list[T] -> list[U] elementwise and the empty-list rule.
// This is the type-level half; coerceList performs the conversion once
// coerce() has confirmed via this function that it is legal.
func coercibleList(from, to Type) bool {
	fromElem, fromIsList := listElem(from)
	toElem, toIsList := listElem(to)
	if !fromIsList || !toIsList {
		return false
	}
	// list[nulltype] is the empty list literal's type and is compatible with
	// any list type.
	if fromElem.Code == CodeNull {
		return true
	}
	return coercible(fromElem, toElem)
}

// listElem returns the element type of the single list type in t, looking
// through a union and an unresolved constraint. It reports false when t admits
// no list, or more than one differing list type.
func listElem(t Type) (Type, bool) {
	switch t.Code {
	case CodeList:
		if len(t.Params) == 1 {
			return t.Params[0], true
		}
	case CodeUnresolved:
		if c, ok := unresolvedConstraint(t); ok {
			return listElem(c)
		}
	case CodeUnion:
		var found Type
		seen := false
		for _, m := range t.Params {
			el, ok := listElem(m)
			if !ok {
				continue
			}
			if seen && !found.Equal(el) {
				return Type{}, false
			}
			found, seen = el, true
		}
		return found, seen
	}
	return Type{}, false
}

// singleScalarTarget returns the one scalar code the target admits, and reports
// false when the target admits none or more than one. Section 1.2.3's catch-all
// rule applies only when the intent is unambiguous.
func singleScalarTarget(t Type) (Code, bool) {
	switch t.Code {
	case CodeUnresolved:
		if c, ok := unresolvedConstraint(t); ok {
			return singleScalarTarget(c)
		}
	case CodeUnion:
		var found Code
		seen := false
		for _, m := range t.Params {
			c, ok := singleScalarTarget(m)
			if !ok {
				continue
			}
			if seen && found != c {
				return 0, false
			}
			found, seen = c, true
		}
		return found, seen
	default:
		if isScalarCode(t.Code) {
			return t.Code, true
		}
	}
	return 0, false
}

// isScalarCode reports whether c is one of the scalar types section 1.2.3's
// catch-all rule converts between. nulltype is excluded: null coerces to nothing.
func isScalarCode(c Code) bool {
	switch c {
	case CodeBool, CodeInt, CodeFloat, CodeString, CodePath, CodeRangeExpr:
		return true
	}
	return false
}

// scalarCoercible reports whether section 1.2.3's non-destructive scalar
// conversions permit from -> to. Whether a particular VALUE survives the
// conversion is coerce()'s question: "3.75" to int is permitted here and fails
// there.
func scalarCoercible(from, to Code) bool {
	if from == to {
		return true
	}
	switch to {
	case CodeString:
		// bool/int/float/path -> string, and range_expr -> string.
		return true
	case CodePath:
		return from == CodeString
	case CodeInt:
		return from == CodeFloat || from == CodeString
	case CodeFloat:
		return from == CodeInt || from == CodeString
	}
	return false
}

// errNotCoercible is the sentinel behind every "cannot be coerced" report, so a
// caller can distinguish an inapplicable conversion from a conversion that
// applied and then failed on the value.
var errNotCoercible = errors.New("cannot be coerced")

// Coerce converts v to target per section 1.2.3's implicit type coercion --
// the very same conversion an evaluation's own target type applies to its
// result, exported so a caller that has ALREADY evaluated can convert once
// more without re-parsing and re-evaluating the source.
//
// It exists for one caller, and the reason is structural rather than
// convenient. internal/openjd's task-parameter range resolver
// (resolve.go's evalRangeExprField) must evaluate a whole-field range
// expression against exactly the type section 1.3.12 gives that field --
// "int | string | range_expr | list[int]" for an INT parameter -- because
// anything else would make its accept/reject verdict differ from the
// checker's, which is the drift the whole wave exists to prevent. But those
// four union members mean DIFFERENT things downstream: an int or a string
// becomes range TEXT, while a range_expr or a list becomes the range's
// VALUES. So the resolver evaluates against the union, then converts the
// range_expr arm -- and only that arm -- into list[int].
//
// Doing that second step through this function reuses section 1.2.3's own
// range_expr -> list[int] rule (coerceList's rangeInts call, which expands in
// increasing, de-duplicated order per section 3.4.1.1.1). The alternative
// would be a second, independent implementation of "take the integers out of
// a range_expr" living in internal/openjd, whose ordering policy could drift
// from this package's -- and internal/openjd already has a deliberately
// DIFFERENT <IntRangeExpr> policy (first-seen order, no negative step, no
// start > end), so "drift" there is not hypothetical, it is the default
// outcome of writing the expansion twice.
//
// Errors carry no position, exactly as coerce's do: the caller is expected to
// attach whatever position it has.
func Coerce(v Value, target Type) (Value, error) { return coerce(v, target) }

// coerce converts v to the target type, per section 1.2.3.
//
// Errors carry no position: like every operator implementation, this returns a
// plain error and the evaluator attaches the offset of the construct that
// failed.
func coerce(v Value, target Type) (Value, error) {
	if v.Type.Equal(target) || target.Code == CodeAny {
		return v, nil
	}
	if v.IsUnresolved() {
		return coerceUnresolved(v, target)
	}
	if c, ok := unresolvedConstraint(target); ok {
		return coerce(v, c)
	}
	// A null reaching a target that admits null passes through; nothing else
	// converts to or from null.
	if v.IsNull() {
		if includes(target, CodeNull) {
			return v, nil
		}
		return Value{}, fmt.Errorf("null %w to %s", errNotCoercible, target)
	}
	// A value whose type is already one of a union target's own members needs no
	// conversion at all: the target names it. This is the same direct-membership
	// carve-out the scalar path below carries, but comparing whole TYPES rather
	// than type codes, which is what a list needs — includes() matches a list by
	// its outer code alone and would call a list[int] a member of
	// "list[string] | int". It has to sit above the list branch, because
	// coercible cannot answer it: coercibleList asks listElem about the target,
	// and listElem reports a union naming two DIFFERENT list types as not
	// list-shaped at all, which is why "[1.0, 2.0]" was rejected by the target
	// "list[float] | list[int]" that literally names its type.
	if directUnionMember(target, v.Type) {
		return v, nil
	}
	// The three list rules of section 1.2.3: elementwise conversion, the empty
	// list, and range_expr -> list[int].
	//
	// The gate is on the SOURCE, not on either side. A list-shaped TARGET alone
	// is not enough: a plain scalar reaching a target that merely CONTAINS a list
	// type — "T? | list[T]", which section 1.3.2 makes the target of every
	// template "args" item, so it is the first shape a caller will construct —
	// has no list conversion to perform at all and belongs on the scalar path
	// below. Sending it here instead reached coerceList's AsList() and PANICKED.
	// range_expr is the one non-list source with a list conversion, and only when
	// the target really is list-shaped; a range_expr against a string target is
	// section 1.2.3's range_expr -> string rule, which is the scalar path's.
	_, srcIsList := listElem(v.Type)
	dstElem, dstIsList := listElem(target)
	if srcIsList || (v.Type.Code == CodeRangeExpr && dstIsList) {
		if !coercible(v.Type, target) {
			return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
		}
		return coerceList(v, target, dstElem, dstIsList)
	}
	// The general applicability check, with a direct-membership carve-out:
	// coercible alone is too strict here because it deliberately reports false
	// for a scalar that is already a direct (if ambiguous) member of a union
	// target — that case needs no conversion at all, which is exactly why
	// coercible refuses it. targetScalarCode alone is too permissive: its
	// final catch-all resolves "which code to become" without ever asking
	// whether that direction is legal, which let bool/int/float -> path reach
	// AsStr() and panic. This keeps every real conversion validated by the
	// already-vetted coercible/scalarCoercible pair.
	if !includes(target, v.Type.Code) && !coercible(v.Type, target) {
		return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
	}
	return coerceScalar(v, target)
}

// coerceUnresolved coerces a PLACEHOLDER, which has no value to convert.
// Coercing one narrows its constraint and it stays a placeholder — which is
// what lets a type check proceed through a coercion boundary.
func coerceUnresolved(v Value, target Type) (Value, error) {
	// The direct-membership carve-out, the exact counterpart of coerce()'s own
	// (directUnionMember, below), and missing here until EXPR sub-project
	// E4b's whole-branch review. coercible answers "does a CONVERSION apply"
	// and is pinned false for a type a union target already admits unchanged,
	// so asking it alone made a PLACEHOLDER strictly harder to coerce than a
	// concrete value of the very same type: coerce() reaches
	// directUnionMember before it ever consults coercible, and a concrete
	// string against "int | list[int] | range_expr | string" therefore passed
	// while unresolved[string] against the identical target was rejected.
	//
	// Phase 1 is exactly where that asymmetry bites: every job parameter is an
	// unresolved placeholder at template-upload time (symbolsFor, exprcheck.go),
	// so section 1.3.12's four-member INT range union — the first target in
	// this codebase with more than one scalar member — rejected
	// range: "{{Param.Frames}}" at upload and then accepted the identical
	// expression at submit once Frames was bound. Reported at review as
	// "declaring EXPR removes base-spec capability at this field".
	//
	// Narrowing to the whole union (below) rather than to the member is the
	// same thing every other success here does: a placeholder carries a
	// constraint, not a decision.
	if c, ok := unresolvedConstraint(v.Type); ok && directUnionMember(target, c) {
		return Unresolved(target), nil
	}
	if !coercible(v.Type, target) {
		return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
	}
	if target.Code == CodeUnresolved {
		return Value{Type: target}, nil
	}
	return Unresolved(target), nil
}

// directUnionMember reports whether target is a union that names t exactly as
// one of its own members, looking through an unresolved constraint.
//
// This is deliberately NOT coercible's question. coercible answers "does a
// conversion apply", and its own tests pin it to false for a type the target
// already admits unchanged — coercible(int, "float | int") is false precisely
// because nothing needs converting. That is the answer coerce() needs here too,
// with the opposite consequence: nothing to convert means pass the value
// through, not refuse it.
func directUnionMember(target, t Type) bool {
	if c, ok := unresolvedConstraint(target); ok {
		return directUnionMember(c, t)
	}
	return target.Code == CodeUnion && containsType(target.Params, t)
}

// coerceScalar performs a scalar conversion whose applicability coercible has
// already confirmed. It resolves which scalar code to aim for, then converts.
func coerceScalar(v Value, target Type) (Value, error) {
	to, ok := targetScalarCode(v, target)
	if !ok {
		return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
	}
	if to == v.Type.Code {
		return v, nil
	}
	switch to {
	case CodeString:
		return String(v.String()), nil
	case CodePath:
		return Value{Type: TPath, s: v.AsStr()}, nil
	case CodeInt:
		return toInt(v)
	case CodeFloat:
		return toFloat(v)
	}
	return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
}

// coerceList performs the list conversions of section 1.2.3, having already
// confirmed with coercible that the conversion is legal.
//
// dstElem/dstIsList are passed in rather than recomputed: the caller needed them
// to decide this branch applied at all.
func coerceList(v Value, target, dstElem Type, dstIsList bool) (Value, error) {
	// The invariant this function is written against, stated where it is relied
	// upon: only a list source or a range_expr source has a list conversion, and
	// the AsList() below is unchecked precisely because of it. A scalar arriving
	// here used to panic there rather than being reported, so the guard is an
	// error and not an assertion.
	if _, ok := listElem(v.Type); !ok && v.Type.Code != CodeRangeExpr {
		return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
	}
	// range_expr -> list[int]: expand, then convert elementwise in case the
	// target's element type is not int (list[float], say).
	if v.Type.Code == CodeRangeExpr {
		ints, err := rangeInts(v)
		if err != nil {
			return Value{}, err
		}
		return coerceList(List(TInt, intValues(ints)), target, dstElem, dstIsList)
	}
	// A list value whose type already satisfies the target needs no conversion.
	// listElem's element-aware comparison is what makes this safe where the
	// scalar path's code-only includes() would not be: it cannot confuse a
	// list[string] with a list[int] inside a union target.
	if !dstIsList {
		// The target admits the list without naming an element type — TAny, or
		// a union in which the list is a direct member.
		return v, nil
	}
	elems := v.AsList()
	if err := checkElementCount(len(elems)); err != nil {
		return Value{}, err
	}
	out := make([]Value, len(elems))
	for i, elem := range elems {
		converted, err := coerce(elem, dstElem)
		if err != nil {
			return Value{}, fmt.Errorf("element %d: %w", i, err)
		}
		out[i] = converted
	}
	return List(dstElem, out), nil
}

// targetScalarCode picks the scalar code v should become. The conditional rules
// of section 1.2.3 win over the single-scalar catch-all where they apply, in the
// same order coercibleConditional checks them.
func targetScalarCode(v Value, target Type) (Code, bool) {
	switch v.Type.Code {
	case CodeInt:
		if includes(target, CodeFloat) && !includes(target, CodeInt) {
			return CodeFloat, true
		}
	case CodePath:
		if includes(target, CodeString) && !includes(target, CodePath) {
			return CodeString, true
		}
	case CodeRangeExpr:
		if includes(target, CodeString) && !includes(target, CodeRangeExpr) {
			return CodeString, true
		}
	}
	if includes(target, v.Type.Code) {
		return v.Type.Code, true
	}
	return singleScalarTarget(target)
}

// toInt implements float/string -> int, which section 1.2.3 requires to be
// non-destructive: a value that cannot be represented exactly is an error, not a
// truncation.
func toInt(v Value) (Value, error) {
	switch v.Type.Code {
	case CodeFloat:
		f := v.AsFloat()
		i := int64(f)
		// Compare back rather than checking the fraction: this also rejects a
		// magnitude too large for int64, where the conversion itself is
		// undefined.
		if float64(i) != f {
			return Value{}, fmt.Errorf("the float %s cannot be represented exactly as an int", v)
		}
		return Int(i), nil
	case CodeString:
		i, err := strconv.ParseInt(v.AsStr(), 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("the string %q cannot be represented as an int", v.AsStr())
		}
		return Int(i), nil
	}
	return Value{}, fmt.Errorf("%s %w to int", v.Type, errNotCoercible)
}

// toFloat implements int/string -> float, erroring when a string is not a
// number. floatValue rejects a result that is infinite or not a number, so a
// string like "inf" cannot slip through.
func toFloat(v Value) (Value, error) {
	switch v.Type.Code {
	case CodeInt:
		return floatValue(float64(v.AsInt()))
	case CodeString:
		f, err := strconv.ParseFloat(v.AsStr(), 64)
		if err != nil {
			return Value{}, fmt.Errorf("the string %q cannot be parsed as a float", v.AsStr())
		}
		return floatValue(f)
	}
	return Value{}, fmt.Errorf("%s %w to float", v.Type, errNotCoercible)
}

// promotable reports whether a value of type from may be chosen, ON THE
// CALLER'S BEHALF, to fill a parameter declared as to — the narrower question
// shape.go's argCost asks when SELECTING an operator overload, as opposed to
// coerce()'s question of what happens once a context has already demanded a
// specific type.
//
// Those are different questions with different answers. Section 1.2.3's
// single-scalar catch-all ("any scalar value when the target types have a
// single scalar type") answers the coerce() question: given a concrete target
// the caller named, may this value become it. It says nothing about which
// target an operator with several overloads should be steered toward, and
// applying it there answers a question the spec never asked: every bare-scalar
// shape in a multi-shape operator trivially "has a single scalar type" on its
// own, so the catch-all would let ANY scalar argument reach it — an operator
// offering int, float and string overloads would see every pair of scalars
// funneled through the string one, since bool/int/float/path -> string can
// never fail. That is precisely how "true + true" and "'a' + 1" wrongly
// stopped being errors when the operator tables were first built as multi-shape
// lists: the catch-all's own qualifier, "a single scalar type", was meant to
// rule out exactly that ambiguity, not fire once per candidate shape.
//
// What DOES belong to overload selection is section 2.1.1's and 2.1.4's own
// named compatible pairs — "the int is promoted to float", "compatible pairs
// (int/float and string/path)" — which are exactly coercibleConditional's four
// rules: int -> float, path -> string, range_expr -> string, range_expr ->
// list[int]. Each is a conversion the spec explicitly calls compatible, and
// none can fail on any value, so choosing an overload with one never discards
// information the way choosing float -> int or string -> int would (the spec's
// own destructive examples: 3.75 and "3.1" to int, "" and "nothing" to float).
//
// unresolved is transparent on both sides, matching coercible's own rule: a
// placeholder's constraint is what can fail or not, and a target that is
// itself unresolved constrains no more tightly than its own constraint does.
//
// A list conversion is elementwise, and promotable only when converting each
// element is: list[float] -> list[int] is exactly as lossy, element by
// element, as float -> int is on its own. The one exception is
// list[nulltype], the empty list literal's type, which has no element that
// could ever fail to convert.
func promotable(from, to Type) bool {
	// A placeholder is promoted on its constraint; the same on the target side.
	if c, ok := unresolvedConstraint(from); ok {
		return promotable(c, to)
	}
	if c, ok := unresolvedConstraint(to); ok {
		return promotable(from, c)
	}
	// Same "every member must clear the bar" rule as coercible's union branch
	// above, using promotable per member since this function answers the
	// narrower, lossless question. Unlike coercible, promotable has no
	// from.Equal(to) short-circuit — none of its other callers ever invoke it
	// on two already-equal types, since argCost's param.Equal(arg) check
	// filters those out first — so a member that already equals the target
	// must be accepted explicitly here; falling through to a bare
	// promotable(m, to) call would wrongly reject it.
	if from.Code == CodeUnion {
		for _, m := range from.Params {
			if !m.Equal(to) && !promotable(m, to) {
				return false
			}
		}
		return true
	}
	if !coercible(from, to) {
		return false
	}
	// A list is promoted elementwise. The empty list literal is compatible with
	// any list type and has no element that could fail, so it always promotes.
	if fromElem, ok := listElem(from); ok {
		toElem, ok := listElem(to)
		if !ok {
			return false
		}
		if fromElem.Code == CodeNull {
			return true
		}
		return promotable(fromElem, toElem)
	}
	// Only the conditional rules: int -> float, path -> string,
	// range_expr -> string, range_expr -> list[int]. Each is a compatible pair
	// the spec names, and none can fail on a value.
	return coercibleConditional(from, to)
}
