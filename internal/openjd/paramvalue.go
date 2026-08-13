// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"strings"

	"github.com/uberware/sqi/internal/openjd/intrange"
)

// checkParamValueAgainstType applies a declared parameter type's rules to one
// candidate value, attaching ptr as the pointer prefix for every error it
// reports.
//
// It is the ONE implementation of those rules. Two callers need them against
// different sources: validation checks the template's own default
// (validate_paramtypes.go), and binding checks a submitted value. Writing the
// rules twice is how the two drift apart, and this package already carries
// one deliberately-duplicated coercion table -- expr.parseBoolText against
// openjd.parseBoolParamValue -- which is justified only because the worker
// binary cannot import internal/openjd. Inside one package there is no such
// boundary and no such justification.
//
// It does NOT check whether the field was declared or defaulted, and it does
// NOT check the LEGALITY of an item: constraint for the declared element type
// -- both are properties of the declaration, not of a value, so they stay
// with the caller. Running a legality check per submitted value would report
// the same declaration error once per submission.
//
// A value containing "{{" is skipped: it is a format string whose value is
// not known until it resolves, so checking it against its declared type here
// would reject a template that is correct.
func checkParamValueAgainstType(p JobParameter, value, ptr string) ValidationErrors {
	if strings.Contains(value, "{{") {
		return nil
	}

	switch {
	case p.Type == JobParamTypeBool:
		return checkBoolValue(value, ptr)
	case p.Type == JobParamTypeRangeExpr:
		return checkRangeExprValue(value, p, ptr)
	case isListParamType(p.Type):
		return checkListValue(value, p, ptr)
	default:
		// INT, FLOAT, STRING, PATH: the base-spec value rules already have a
		// single shared implementation (validateParamValueConstraints's
		// per-value half), predating RFC 0007 and untouched by it. Routing
		// through it here means every parameter type -- base and extended --
		// answers through this one entry point.
		return validateParamValueInBounds(p, value, ptr)
	}
}

// checkBoolValue is the value half of validateBoolParamConstraints: does
// value parse as one of RFC 0007's accepted BOOL spellings.
func checkBoolValue(value, ptr string) ValidationErrors {
	if _, ok := parseBoolParamValue(value); !ok {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf(
				"%q is not a boolean; use true/false, 1/0, 1.0/0.0, yes/no, or on/off",
				value,
			),
		}}
	}
	return nil
}

// checkRangeExprValue is the value half of validateRangeExprParamConstraints:
// does value parse as an <IntRangeExpr> under the spec's permissive policy,
// and does it satisfy the parameter's minLength/maxLength.
func checkRangeExprValue(value string, p JobParameter, ptr string) ValidationErrors {
	if _, err := intrange.Parse(value); err != nil {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("not a valid range expression: %v", err),
		}}
	}
	return validateRangeExprLength(value, p, ptr)
}

// checkListValue is the value half of validateListParamConstraints: does
// value decode as the declared LIST[*] type's JSON, does its element count
// satisfy minLength/maxLength, and does every element satisfy its type and
// item: constraint.
//
// It does NOT check the item: constraint's own legality for the declared
// element type -- that is validateItemConstraintLegality, which stays with
// the caller because it is a property of the declaration.
func checkListValue(value string, p JobParameter, ptr string) ValidationErrors {
	elem := listElemType(p.Type)

	items, err := decodeListDefault(value, p.Type)
	if err != nil {
		return ValidationErrors{{Pointer: ptr, Message: err.Error()}}
	}

	errs := validateListLength(len(items), p, ptr)
	for i, item := range items {
		errs = append(errs, validateListElement(
			item, elem, p.Item, fmt.Sprintf("%s/%d", ptr, i),
		)...)
	}
	return errs
}

// prefixPointers rewrites every error's pointer as though checkParamValueAgainstType
// had been called at ptr+field instead of ptr, so a caller can place the
// shared core's result under the field the value actually came from (e.g.
// "/default" for a declared default). Any element-relative suffix the core
// appended -- an index, or a nested index -- stays trailing.
//
// This only works because the core always builds its pointers by extending
// ptr, never by inventing a new prefix: trimming ptr off the front of a
// returned pointer and re-assembling around field is therefore exact.
// ("/p" -> "/p/default", and "/p/0" -> "/p/default/0", not "/p/0/default".)
func prefixPointers(errs ValidationErrors, ptr, field string) ValidationErrors {
	for i := range errs {
		errs[i].Pointer = insertPointerSegment(errs[i].Pointer, ptr, field)
	}
	return errs
}

// insertPointerSegment inserts field immediately after ptr in pointer,
// keeping whatever the core appended past ptr (an element index, or a nested
// one) trailing after it.
func insertPointerSegment(pointer, ptr, field string) string {
	return ptr + field + strings.TrimPrefix(pointer, ptr)
}
