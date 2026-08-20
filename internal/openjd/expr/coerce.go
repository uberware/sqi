// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// This file implements section 1.2.3, implicit type coercion, as RFC 0005
// restated it in openjd-specifications#175 (merged 2026-08-19).
//
// TWO STEPS, IN ORDER. Satisfaction asks whether the result's type already
// satisfies the target, and if so the value is used UNCHANGED and nothing is
// converted (satisfies). Otherwise conversion walks the target's DESTINATIONS
// in an order fixed by the result's own type, first success wins, and a
// destination that fails is not an error so long as a later one succeeds
// (scalarDestinations, orderedListDestinations). Non-list destinations precede
// list ones, always.
//
// The older reading is still visible in this file's shape and should not be
// mistaken for the current one: it phrased every rule against what the target
// does NOT include ("int to float when the target types do not include int"),
// which is how a satisfaction check looks when it is spelled out one rule at a
// time. It also had no answer when a target offered two candidates of the same
// kind, and gave up rather than choosing -- singleScalarTarget, still used by
// the promotion path below, is that giving-up.
//
// THREE PREDICATES, NOT TWO, and the difference matters:
//
//   - coerce()            converts a VALUE against a target type.
//   - coercibleToTarget() is coerce()'s type-level twin, for the unresolved
//     path where there is no value to convert.
//   - coercible()         belongs to a DIFFERENT MECHANISM: the coercion that
//     resolves a function call and promotes 1 to 1.0 in "1 + 2.0" (promotable,
//     shape.go). RFC 0005 says in as many words that #175 does not touch it,
//     so it keeps the older rules deliberately. Routing call dispatch through
//     the destination table would make it depend on a target the caller never
//     supplied.

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
// the type to, under the rules RFC 0005 applies while RESOLVING A FUNCTION CALL
// -- the mechanism that promotes 1 to 1.0 in "1 + 2.0". It answers at the type
// level only, and performs nothing.
//
// It is NOT the predicate behind target-type coercion; that is
// coercibleToTarget, and since openjd-specifications#175 the two genuinely
// differ. The clearest case: coercible(int, "float | int") is FALSE, because
// nothing needs converting, while coercibleToTarget says TRUE because the
// target admits an int outright. Both answers are right for their own question.
// Its only callers are promotable() and shape matching; do not reach for it
// from a target-type path.
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

// satisfies reports whether a result of type from already SATISFIES target, in
// RFC 0005's sense: the value is used unchanged and no conversion is attempted.
//
// The relation is DIRECTIONAL and the specification says so explicitly: an int
// satisfies any, and any does not satisfy an int. It is deliberately not the
// symmetric matching that binds type variables during signature matching, which
// would accept a list[T1] target by binding T1 and then discarding the binding.
// Keep the two apart; shape.go owns the symmetric one.
func satisfies(from, to Type) bool {
	switch to.Code {
	case CodeAny:
		return true
	case CodeUnresolved:
		c, ok := unresolvedConstraint(to)
		return ok && satisfies(from, c)
	case CodeUnion:
		for _, m := range to.Params {
			if satisfies(from, m) {
				return true
			}
		}
		return false
	}
	if fromElem, ok := listParam(from); ok {
		if toElem, ok := listParam(to); ok {
			return satisfies(fromElem, toElem)
		}
		return false
	}
	return from.Equal(to)
}

// listParam returns t's element type when t is literally a list, without
// looking through unions or unresolved constraints.
//
// listElem() does look through both, and answers a different question: "which
// single list type does this TARGET offer". Satisfaction needs the plain one --
// a union is decomposed by satisfies() itself, one member at a time, so a
// union-aware accessor here would answer for the wrong type.
func listParam(t Type) (Type, bool) {
	if t.Code == CodeList && len(t.Params) == 1 {
		return t.Params[0], true
	}
	return Type{}, false
}

// scalarDestinations is RFC 0005's destination-order table, restated by
// openjd-specifications#175. Two principles fix the order, and both matter:
// a value stays within its own kind before it becomes text, and a conversion
// that CAN FAIL is attempted before one that always succeeds -- a universal
// fallback tried first would make every destination after it unreachable.
//
// The range_expr row's list[int] destination is not here: non-list destinations
// come before list ones, so it is attempted by coerce() after this table is
// exhausted, which is why a target offering both string and list[int] gets the
// string.
func scalarDestinations(from Code) []Code {
	switch from {
	case CodeBool:
		return []Code{CodeString}
	case CodeInt:
		return []Code{CodeFloat, CodeString}
	case CodeFloat:
		return []Code{CodeInt, CodeString}
	case CodeString:
		// int before float because every string that parses as an int also
		// parses as a float; bool and range_expr are selective parses; path
		// last because every string is a valid path.
		return []Code{CodeInt, CodeFloat, CodeBool, CodeRangeExpr, CodePath}
	case CodePath:
		return []Code{CodeString}
	case CodeRangeExpr:
		return []Code{CodeString}
	}
	return nil
}

// convertScalar performs one destination's conversion, or reports that this
// destination does not take the value. A failure here is not fatal: coerce()
// tries the next destination, and only an exhausted list is an error.
func convertScalar(v Value, to Code) (Value, error) {
	switch to {
	case CodeString:
		return String(v.String()), nil
	case CodePath:
		return Value{Type: TPath, s: v.AsStr()}, nil
	case CodeInt:
		return toInt(v)
	case CodeFloat:
		return toFloat(v)
	case CodeBool:
		if v.Type.Code != CodeString {
			return Value{}, fmt.Errorf("%s %w to bool", v.Type, errNotCoercible)
		}
		return boolFromString(v.AsStr())
	case CodeRangeExpr:
		if v.Type.Code != CodeString {
			return Value{}, fmt.Errorf("%s %w to range_expr", v.Type, errNotCoercible)
		}
		return RangeExpr(v.AsStr())
	}
	return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, to)
}

// coerceByDestination runs step 2 of RFC 0005's coercion on a scalar result:
// each destination the target offers, in the table's order, first success wins.
// A destination that fails is not an error so long as a later one succeeds.
//
// The three results are distinct and the caller needs all three. ok reports a
// conversion; a nil error with ok false means the target offered this value no
// scalar destination at all, so another rule (the list ones) may still apply; a
// non-nil error means every offered destination was attempted and failed, and
// that error is the LAST one's, which is the most specific thing there is to
// say. Returning a generic "cannot be coerced" there would throw away
// boolFromString's own message for a string that is not a bool spelling.
func coerceByDestination(v Value, target Type) (Value, bool, error) {
	var lastErr error
	for _, to := range scalarDestinations(v.Type.Code) {
		// nulltype, type variables, noreturn and unresolved contribute no
		// destination; includes() answers this for the scalar codes in the
		// table because none of them carries type parameters.
		if !includes(target, to) {
			continue
		}
		out, err := convertScalar(v, to)
		if err == nil {
			return out, true, nil
		}
		lastErr = err
	}
	return Value{}, false, lastErr
}

// coercibleToTarget is the TYPE-level twin of coerce(): it answers whether a
// result of type from could reach target under RFC 0005's two steps, without
// converting anything and without a value to convert.
//
// It is deliberately NOT coercible(). The specification keeps two mechanisms
// apart and #175 changed only one of them: target-type coercion (this one, the
// one a template FIELD applies to an expression's result) versus the coercion
// that resolves a function call and promotes 1 to 1.0 in "1 + 2.0" (coercible,
// promotable, shape.go). Routing call promotion through the destination table
// would make dispatch depend on a target the caller never supplied, so the two
// predicates stay separate even though they overlap heavily.
func coercibleToTarget(from, to Type) bool {
	if satisfies(from, to) {
		return true
	}
	// Unresolved is transparent on both sides: what a placeholder can reach is
	// decided by its constraint, and a target that is itself unresolved
	// constrains no more tightly than its constraint does.
	if c, ok := unresolvedConstraint(from); ok {
		return coercibleToTarget(c, to)
	}
	if c, ok := unresolvedConstraint(to); ok {
		return coercibleToTarget(from, c)
	}
	// A union SOURCE is usable only where every member would be: it is some one
	// of its members, decided at runtime, so a target that would reject any one
	// of them cannot safely receive it.
	if from.Code == CodeUnion {
		for _, m := range from.Params {
			if !coercibleToTarget(m, to) {
				return false
			}
		}
		return true
	}
	// null reaches only a target that already admits it; no conversion produces
	// null, so nulltype is never a destination.
	if from.Code == CodeNull {
		return includes(to, CodeNull)
	}
	for _, d := range scalarDestinations(from.Code) {
		if includes(to, d) {
			return true
		}
	}
	return hasListDestination(from, to)
}

// hasListDestination is coercibleToTarget's list half, split out to keep each
// function inside the complexity budget. It answers the same question one level
// down: does the target offer a list destination this type could reach?
func hasListDestination(from, to Type) bool {
	// range_expr -> list[int] is accepted exactly when a list[int] value would
	// satisfy the destination, and it is the only list destination a non-list
	// source has.
	if from.Code == CodeRangeExpr {
		for _, elem := range listDestinations(to) {
			if satisfies(TInt, elem) {
				return true
			}
		}
		return false
	}
	srcElem, ok := listParam(from)
	if !ok {
		return false
	}
	for _, elem := range listDestinations(to) {
		// list[nulltype] -> list[T] for any T: the empty list literal has no
		// element that could fail, so every list destination takes it.
		if srcElem.Code == CodeNull || coercibleToTarget(srcElem, elem) {
			return true
		}
	}
	return false
}

// narrowedUnionConstraint is narrowedConstraint's existential half: each member
// that can reach the target contributes what it would become, and the ones that
// cannot are discarded rather than failing the whole coercion. Coercing
// unresolved[int | string] to an int target therefore yields unresolved[int].
func narrowedUnionConstraint(from, target Type) (Type, bool) {
	var out []Type
	for _, m := range from.Params {
		if satisfies(m, target) {
			out = append(out, m)
			continue
		}
		if n, ok := narrowedConstraint(m, target); ok {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return Type{}, false
	}
	return UnionOf(out...), true
}

// narrowedConstraint is the type-level half of RFC 0005's "Coercion of
// Unresolved Values": what a PLACEHOLDER's constraint becomes when it is
// coerced, given that there is no payload to decide which destination wins.
//
// It narrows to the UNION of every destination with a type-level rule rather
// than betting on one, because the invariant the specification states has two
// halves and a guess breaks the second: the narrowed constraint must satisfy
// the target, AND the concrete result's type must satisfy the narrowed
// constraint. unresolved[float] against "int | string" narrows to
// unresolved[int | string] -- a 3.0 payload takes float->int and a 3.5 payload
// fails it and falls through to string, and both outcomes lie inside that.
//
// A union CONSTRAINT is existential, and this is the one place where a union on
// the source side does not mean "every member must clear the bar": a constraint
// is a set of possibilities, not a value, so members that cannot coerce are
// discarded rather than failing the whole thing. coercibleToTarget's own
// union-source rule is the opposite for the opposite reason -- a union-typed
// VALUE is some one of its members, chosen at runtime, so a target that would
// reject any one of them cannot safely receive it.
func narrowedConstraint(from, target Type) (Type, bool) {
	if from.Code == CodeUnion {
		return narrowedUnionConstraint(from, target)
	}
	var dests []Type
	for _, d := range scalarDestinations(from.Code) {
		if includes(target, d) {
			dests = append(dests, Type{Code: d})
		}
	}
	if from.Code == CodeRangeExpr {
		for _, elem := range listDestinations(target) {
			if satisfies(TInt, elem) {
				// Materializing a range only ever produces a list[int], so the
				// constraint narrows to that and not to the destination.
				dests = append(dests, ListOf(TInt))
				break
			}
		}
	}
	if srcElem, ok := listParam(from); ok {
		for _, elem := range orderedListDestinations(srcElem, target) {
			if srcElem.Code == CodeNull || coercibleToTarget(srcElem, elem) {
				dests = append(dests, ListOf(elem))
			}
		}
	}
	if len(dests) == 0 {
		return Type{}, false
	}
	return UnionOf(dests...), true
}

// listDestinations returns the element type of every list the target offers, in
// the target's own normalized member order -- which RFC 0005 defines (type
// parameters sorted alphabetically, nulltype last), so it is a stated order and
// not this function's choice.
func listDestinations(target Type) []Type {
	switch target.Code {
	case CodeUnresolved:
		if c, ok := unresolvedConstraint(target); ok {
			return listDestinations(c)
		}
	case CodeUnion:
		var out []Type
		for _, m := range target.Params {
			out = append(out, listDestinations(m)...)
		}
		return out
	case CodeList:
		if len(target.Params) == 1 {
			return []Type{target.Params[0]}
		}
	}
	return nil
}

// orderedListDestinations is the list[S] row of RFC 0005's destination table:
// "list destinations in S's order, applied to their element types", so
// list[float] against "list[int] | list[string]" attempts list[int] first.
//
// The sort is STABLE and unranked destinations keep their normalized position,
// which is what makes the empty list work without a second rule: list[nulltype]
// has no destination order of its own (scalarDestinations(nulltype) is empty),
// so every candidate is unranked and the union's normalized member order stands
// -- exactly what the specification says the empty list's nominal element type
// follows.
func orderedListDestinations(srcElem, target Type) []Type {
	cands := listDestinations(target)
	order := scalarDestinations(srcElem.Code)
	rank := func(t Type) int {
		for i, c := range order {
			if t.Code == c {
				return i
			}
		}
		return len(order)
	}
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i]) < rank(cands[j]) })
	return cands
}

// convertListTo performs one list destination's elementwise conversion. A
// failure is that destination's, not the coercion's: the caller tries the next.
func convertListTo(v Value, elem Type) (Value, error) {
	elems := v.AsList()
	if err := checkElementCount(len(elems)); err != nil {
		return Value{}, err
	}
	out := make([]Value, len(elems))
	for i, e := range elems {
		converted, err := coerce(e, elem)
		if err != nil {
			return Value{}, fmt.Errorf("element %d: %w", i, err)
		}
		out[i] = converted
	}
	return List(elem, out), nil
}

// coerceByListDestination runs the list destinations, after every scalar one has
// been tried and failed. The three results mean what coerceByDestination's do.
func coerceByListDestination(v Value, target Type) (Value, bool, error) {
	// range_expr -> list[int] is the one list conversion from a non-list source,
	// and it is accepted exactly when a list[int] value would satisfy the
	// destination: list[int], list[any] and list[int | string], but not
	// list[float] or list[string]. Implicit rules do not chain, so the
	// materialized list is not widened element-wise afterwards.
	if v.Type.Code == CodeRangeExpr {
		for _, elem := range listDestinations(target) {
			if !satisfies(TInt, elem) {
				continue
			}
			ints, err := rangeInts(v)
			if err != nil {
				return Value{}, false, err
			}
			return List(TInt, intValues(ints)), true, nil
		}
		return Value{}, false, nil
	}
	srcElem, ok := listParam(v.Type)
	if !ok {
		return Value{}, false, nil
	}
	var lastErr error
	for _, elem := range orderedListDestinations(srcElem, target) {
		out, err := convertListTo(v, elem)
		if err == nil {
			return out, true, nil
		}
		lastErr = err
	}
	return Value{}, false, lastErr
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
	if v.IsUnresolved() {
		return coerceUnresolved(v, target)
	}
	// Step 1, satisfaction: a result whose type the target already admits is
	// used UNCHANGED, and step 2 is never reached for it. This subsumes three
	// carve-outs the older reading needed separately -- the Equal/any early
	// return, directUnionMember, and coerceList's "the target admits the list
	// without naming an element type" branch -- and it fixes what they got
	// wrong between them: a list[int] against a list[any] target kept its own
	// list[int] type here only by accident of which branch caught it first.
	if satisfies(v.Type, target) {
		return v, nil
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
	// Step 2, conversion, for a scalar result: the destination table, in order.
	// It runs before the list branch because RFC 0005 puts every non-list
	// destination ahead of every list one -- which is what makes a range_expr
	// against a target offering both string and list[int] become the string,
	// and the list[int] destination unreachable there.
	out, ok, convErr := coerceByDestination(v, target)
	if ok {
		return out, nil
	}
	if convErr != nil {
		return Value{}, convErr
	}
	// The list destinations, attempted only after every scalar one, because
	// RFC 0005 puts the whole non-list group first. The gate is on the SOURCE:
	// a plain scalar reaching a target that merely CONTAINS a list type --
	// "T? | list[T]", which section 1.3.2 makes the target of every template
	// "args" item -- has no list conversion to perform, and sending it here
	// used to reach AsList() and PANIC. range_expr is the one non-list source
	// with a list destination.
	out, ok, listErr := coerceByListDestination(v, target)
	if ok {
		return out, nil
	}
	if listErr != nil {
		return Value{}, listErr
	}
	return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
}

// coerceUnresolved coerces a PLACEHOLDER, which has no value to convert.
// Coercing one narrows its constraint and it stays a placeholder — which is
// what lets a type check proceed through a coercion boundary.
func coerceUnresolved(v Value, target Type) (Value, error) {
	// Step 1 applies to a PLACEHOLDER too, and RFC 0005 says what it leaves
	// behind: "Satisfaction leaves the value alone, so its constraint keeps the
	// source type" -- an unresolved[list[int]] against a list[any] target stays
	// unresolved[list[int]] rather than widening, matching the concrete
	// list[int] that would be returned unchanged.
	//
	// This subsumes the directUnionMember carve-out that stood here, and it is
	// the same asymmetry that carve-out was added (in E4b's whole-branch review)
	// to close: coerce() reaches satisfaction before it ever consults coercible,
	// so asking coercible alone made a placeholder strictly harder to coerce
	// than a concrete value of the very same type. Phase 1 is where that bites,
	// because every job parameter is a placeholder at template-upload time, and
	// section 1.3.12's four-member INT range union is the first target here with
	// more than one scalar member: range: "{{Param.Frames}}" was rejected at
	// upload and the identical expression accepted at submit.
	if c, ok := unresolvedConstraint(v.Type); ok && satisfies(c, target) {
		return v, nil
	}
	if target.Code == CodeUnresolved {
		if !coercibleToTarget(v.Type, target) {
			return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
		}
		return Value{Type: target}, nil
	}
	c, ok := unresolvedConstraint(v.Type)
	if !ok {
		return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
	}
	narrowed, ok := narrowedConstraint(c, target)
	if !ok {
		return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
	}
	return Unresolved(narrowed), nil
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
