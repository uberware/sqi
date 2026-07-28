// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

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
