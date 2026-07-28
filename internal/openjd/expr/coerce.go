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
		return len(target.Params) == 1 && includes(target.Params[0], c)
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
		return len(from.Params) == 1 && coercible(from.Params[0], to)
	}
	if to.Code == CodeUnresolved {
		return len(to.Params) == 1 && coercible(from, to.Params[0])
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
		// range_expr. The conversion itself is sub-project B2's; this is the
		// type-level half.
		if !includes(to, CodeRangeExpr) {
			if el, ok := listElem(to); ok && el.Code == CodeInt {
				return true
			}
		}
	}
	return false
}

// coercibleList covers list[T] -> list[U] elementwise and the empty-list rule.
// Both are type-level only in B1; sub-project B2 performs them.
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
		if len(t.Params) == 1 {
			return listElem(t.Params[0])
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
		if len(t.Params) == 1 {
			return singleScalarTarget(t.Params[0])
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

// coerce converts v to the target type, per section 1.2.3.
//
// Errors carry no position: like every operator implementation, this returns a
// plain error and the evaluator attaches the offset of the construct that
// failed.
func coerce(v Value, target Type) (Value, error) {
	if v.Type.Equal(target) || target.Code == CodeAny {
		return v, nil
	}
	// A placeholder has no value to convert. Coercing one narrows its
	// constraint and it stays a placeholder — which is what lets a type check
	// proceed through a coercion boundary.
	if v.IsUnresolved() {
		if !coercible(v.Type, target) {
			return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
		}
		if target.Code == CodeUnresolved {
			return Value{Type: target}, nil
		}
		return Unresolved(target), nil
	}
	if target.Code == CodeUnresolved && len(target.Params) == 1 {
		return coerce(v, target.Params[0])
	}
	// A null reaching a target that admits null passes through; nothing else
	// converts to or from null.
	if v.IsNull() {
		if includes(target, CodeNull) {
			return v, nil
		}
		return Value{}, fmt.Errorf("null %w to %s", errNotCoercible, target)
	}
	// The three list rules (elementwise, the empty list, and range_expr ->
	// list[int]) are type-level only in B1: coercible says yes, but performing
	// one needs list values, which is sub-project B2's. Both directions are
	// checked because range_expr -> list[int] has a scalar source and a list
	// target. Report the gap plainly rather than silently returning the value
	// unchanged, which would be a wrong value.
	_, srcIsList := listElem(v.Type)
	_, dstIsList := listElem(target)
	if srcIsList || dstIsList {
		if !coercible(v.Type, target) {
			return Value{}, fmt.Errorf("%s %w to %s", v.Type, errNotCoercible, target)
		}
		return Value{}, fmt.Errorf(
			"converting %s to %s is not implemented until sub-project B2", v.Type, target,
		)
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

// losslesslyCoercible reports whether a value of type from can reach the target
// type to by a conversion that CANNOT FAIL on any value.
//
// This is the subset of section 1.2.3 that overload selection may use. The full
// matrix also permits float -> int, string -> int and string -> float, each of
// which errors when the value does not fit — the spec's own examples are 3.75
// and "3.1" to int, and "" and "nothing" to float. Those are legitimate where a
// context demanded the type and is prepared for the failure, which is what
// coerce() does at an explicit target. Choosing an operator overload with one
// would silently discard information instead: "1 + 2.5" would select the
// (int, int) shape and drop the .5, and "'a' + 'b'" would select it by parsing
// both strings as integers. Section 2.1.1 requires the opposite — "the int is
// promoted to float and the float overload is used".
//
// unresolved is transparent on both sides, matching coercible's own rule: a
// placeholder's constraint is what can fail or not, and a target that is
// itself unresolved constrains no more tightly than its own constraint does.
// Leaving either side wrapped would let a still-lossy constraint (unresolved's
// float -> int, say) hide behind the "not a bare scalar" check below.
//
// A list conversion is elementwise, and lossless only when converting each
// element is: list[float] -> list[int] is exactly as lossy, element by
// element, as float -> int is on its own. The one exception is
// list[nulltype], the empty list literal's type, which has no element that
// could ever fail to convert.
func losslesslyCoercible(from, to Type) bool {
	if from.Code == CodeUnresolved && len(from.Params) == 1 {
		return losslesslyCoercible(from.Params[0], to)
	}
	if to.Code == CodeUnresolved && len(to.Params) == 1 {
		return losslesslyCoercible(from, to.Params[0])
	}
	if !coercible(from, to) {
		return false
	}
	if fromElem, ok := listElem(from); ok {
		if fromElem.Code == CodeNull {
			return true
		}
		if toElem, ok := listElem(to); ok {
			return losslesslyCoercible(fromElem, toElem)
		}
	}
	// Neither a list nor a bare scalar — any, nulltype and noreturn carry no
	// conversion that can fail.
	if !isScalarCode(from.Code) {
		return true
	}
	// The conditional rules all widen: int -> float, path -> string,
	// range_expr -> string, range_expr -> list[int]. Only the single-scalar
	// catch-all can select a narrowing conversion.
	if coercibleConditional(from, to) {
		return true
	}
	c, ok := singleScalarTarget(to)
	if !ok {
		return true
	}
	return !lossyScalarPair(from.Code, c)
}

// lossyScalarPair reports whether converting from -> to can fail on some value.
// These are exactly the three conversions section 1.2.3 marks with a failure
// condition.
func lossyScalarPair(from, to Code) bool {
	switch to {
	case CodeInt:
		return from == CodeFloat || from == CodeString
	case CodeFloat:
		return from == CodeString
	}
	return false
}
