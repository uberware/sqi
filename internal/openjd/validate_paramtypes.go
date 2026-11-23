// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// This file validates the job-parameter types RFC 0007 (the EXPR extension's
// Extended Parameter Types) adds: BOOL, RANGE_EXPR, and the six LIST[*] types.
//
// Every function here is reached through validateEXPRParamType, called from
// validateJobParams' type switch, which reports these types as unknown when
// the template does not declare the EXPR extension; nothing after it needs to
// re-check the declaration.

// validateEXPRParamType dispatches to the right validator for a type RFC 0007
// adds, or reports the type as unknown when the template does not declare the
// EXPR extension.
//
// This check is needed in addition to the decoder's. Only an EXPR template
// canonicalizes a spelling, but a base-spec template that writes the
// canonical form verbatim ("BOOL", "LIST[INT]") reaches these switch arms
// with exprDeclared false. Without this check it would be validated against
// rules it never opted into. TestListDefault_ListTypeWithoutEXPRStillRejected
// covers the same case in the parse-time list default reader.
func validateEXPRParamType(p JobParameter, ptr string, exprDeclared bool) ValidationErrors {
	if !exprDeclared {
		return ValidationErrors{{
			Pointer: ptr + "/type",
			Message: unknownJobTypeMessage(p.Type, false),
		}}
	}
	switch {
	case p.Type == JobParamTypeBool:
		return validateBoolParamConstraints(p, ptr)
	case p.Type == JobParamTypeRangeExpr:
		return validateRangeExprParamConstraints(p, ptr)
	case isListParamType(p.Type):
		return validateListParamConstraints(p, ptr)
	}
	return nil
}

// parseBoolParamValue interprets a BOOL parameter value per RFC 0007's
// accepted-values table: JSON/YAML boolean literals, integer or float 1/0, and
// the case-insensitive strings true/yes/on/1 and false/no/off/0.
//
// The table is closed: "y", "t" and "2" are rejected
// (2.9--bool-param-{int,float,string}-invalid).
//
// The value arrives as text because [JobParameter.Default] is a *string for
// every type, so a YAML `true` and a YAML `"true"` are indistinguishable
// here; the RFC's table treats both as true.
func parseBoolParamValue(s string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1", "1.0":
		return true, true
	case "false", "no", "off", "0", "0.0":
		return false, true
	}
	return false, false
}

// validateBoolParamConstraints checks a BOOL parameter.
func validateBoolParamConstraints(p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors

	// RFC 0007: BOOL does not support allowedValues. AllowedValuesSet rather
	// than len() catches `allowedValues: []`, which decodes empty.
	if p.AllowedValuesSet {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/allowedValues",
			Message: "not supported on a BOOL parameter: restricting a two-valued " +
				"type to one value constrains nothing",
		})
	}

	errs = append(errs, rejectFields(
		ptr, "a BOOL parameter",
		field{p.MinValue != nil, "minValue"},
		field{p.MaxValue != nil, "maxValue"},
		field{p.MinLength != nil, "minLength"},
		field{p.MaxLength != nil, "maxLength"},
		field{p.Item != nil, "item"},
	)...)

	if p.Default != nil {
		errs = append(errs, prefixPointers(
			checkParamValueAgainstType(p, *p.Default, ptr, true), ptr, "/default",
		)...)
	}

	return errs
}

// defaultRangeExprMaxLength is RFC 0007's maxLength default for RANGE_EXPR.
// It applies when the template declares no maxLength of its own.
const defaultRangeExprMaxLength = 1024

// validateRangeExprParamConstraints checks a RANGE_EXPR parameter.
//
// The default is parsed with the specification's policy, not
// internal/openjd's. internal/openjd parses literal base-spec <IntRangeExpr>
// text more strictly than the specification (openjdRangePolicy sets
// PositiveStepOnly and AscendingOnly, so "10-1:-1" and "-1--10:-1" are
// rejected; see range.go and internal/openjd/intrange), and that strictness
// must be kept. The conformance fixture 2.10--range-expr-param.yaml declares
// both of those as RANGE_EXPR defaults.
//
// The permissive policy is also the one the value meets downstream: the lone
// whole-field form range: "{{Param.FrameRange}}" evaluates as a value through
// range_expr's list[int] coercion and never re-enters <IntRangeExpr> text
// (see resolve.go's evalRangeExprField), and expr.ValueFromText's
// CodeRangeExpr case calls the permissive expr.RangeExpr. Literal base-spec
// range text, including text assembled from an embedded reference (see
// resolve.go's resolveRangeExprField), keeps the strict policy.
//
// minLength and maxLength bound the expression's string length, not its
// expanded element count: "1-1000000" is nine characters. Expansion cost is
// bounded elsewhere.
func validateRangeExprParamConstraints(p JobParameter, ptr string) ValidationErrors {
	errs := rejectFields(
		ptr, "a RANGE_EXPR parameter",
		field{p.MinValue != nil, "minValue"},
		field{p.MaxValue != nil, "maxValue"},
		field{p.AllowedValuesSet, "allowedValues"},
		field{p.Item != nil, "item"},
	)

	if p.Default == nil {
		return errs
	}

	return append(errs, prefixPointers(
		checkParamValueAgainstType(p, *p.Default, ptr, true), ptr, "/default",
	)...)
}

// validateRangeExprLength applies minLength and maxLength to a RANGE_EXPR
// value's string length, substituting RFC 0007's 1024 default when the
// template declares no maximum.
//
// ptr is reported as-is, with no field segment of its own appended: its only
// caller, checkRangeExprValue, is always invoked with the parameter's own
// pointer, and its caller (validateRangeExprParamConstraints, for the
// declared default) attaches "/default" afterward via prefixPointers.
func validateRangeExprLength(def string, p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	n := utf8.RuneCountInString(def)

	if p.MinLength != nil && n < *p.MinLength {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("length %d is below minLength %d", n, *p.MinLength),
		})
	}

	maxLen := defaultRangeExprMaxLength
	if p.MaxLength != nil {
		maxLen = *p.MaxLength
	}
	if n > maxLen {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("length %d exceeds maxLength %d", n, maxLen),
		})
	}

	return errs
}

// listElemType returns the element type of a list parameter type. For
// LIST[LIST[INT]] it returns LIST[INT]; for a non-list type, the empty string.
func listElemType(t JobParamType) JobParamType {
	switch t {
	case JobParamTypeListString:
		return JobParamTypeString
	case JobParamTypeListPath:
		return JobParamTypePath
	case JobParamTypeListInt:
		return JobParamTypeInt
	case JobParamTypeListFloat:
		return JobParamTypeFloat
	case JobParamTypeListBool:
		return JobParamTypeBool
	case JobParamTypeListListInt:
		return JobParamTypeListInt
	}
	return ""
}

// validateListParamConstraints checks a LIST[*] parameter: the legality of its
// item: constraints for the declared element type, then the default's length
// and each of its elements.
func validateListParamConstraints(p JobParameter, ptr string) ValidationErrors {
	elem := listElemType(p.Type)

	errs := validateItemConstraintLegality(p.Item, elem, ptr+"/item")
	if len(errs) > 0 {
		// A constraint that cannot apply to this element type makes checking
		// elements against it meaningless -- and would produce a second,
		// confusing error per element on top of the real one.
		return errs
	}

	// A default containing a format string is not known until submission, so
	// it cannot be checked against the declared type here. Same rule the
	// scalar types use (validateParamValueConstraints), enforced by
	// checkParamValueAgainstType itself.
	if p.Default == nil {
		return errs
	}

	return append(errs, prefixPointers(
		checkParamValueAgainstType(p, *p.Default, ptr, true), ptr, "/default",
	)...)
}

// validateListLength applies the list's own minLength/maxLength, which bound
// the element count (RFC 0007) rather than any element's own length.
//
// ptr is reported as-is, with no field segment of its own appended: its only
// caller, checkListValue, is always invoked with the parameter's own pointer,
// and its caller (validateListParamConstraints, for the declared default)
// attaches "/default" afterward via prefixPointers.
func validateListLength(n int, p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	if p.MinLength != nil && n < *p.MinLength {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("has %d items, below minLength %d", n, *p.MinLength),
		})
	}
	if p.MaxLength != nil && n > *p.MaxLength {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("has %d items, above maxLength %d", n, *p.MaxLength),
		})
	}
	return errs
}

// validateListElement checks one element against its declared type and the
// item: constraint for its level, recursing one level for an inner LIST[INT].
func validateListElement(v any, elem JobParamType, c *ItemConstraint, ptr string) ValidationErrors {
	if elem == JobParamTypeListInt {
		return validateInnerList(v, c, ptr)
	}

	if errs := validateElemType(v, elem, ptr); len(errs) > 0 {
		return errs
	}
	text, _ := scalarElemText(v) // type already verified above
	return validateElemAgainstConstraint(text, elem, c, ptr)
}

// validateInnerList checks one inner list of a LIST[LIST[INT]] -- its own
// element count against c, then each integer against c.Item.
func validateInnerList(v any, c *ItemConstraint, ptr string) ValidationErrors {
	inner, ok := v.([]any)
	if !ok {
		return ValidationErrors{{Pointer: ptr, Message: "must be a list of integers"}}
	}

	var errs ValidationErrors
	if c != nil {
		if c.MinLength != nil && len(inner) < *c.MinLength {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("has %d items, below item.minLength %d",
					len(inner), *c.MinLength),
			})
		}
		if c.MaxLength != nil && len(inner) > *c.MaxLength {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("has %d items, above item.maxLength %d",
					len(inner), *c.MaxLength),
			})
		}
	}

	var innerC *ItemConstraint
	if c != nil {
		innerC = c.Item
	}
	for j, e := range inner {
		errs = append(errs, validateListElement(
			e, JobParamTypeInt, innerC, fmt.Sprintf("%s/%d", ptr, j),
		)...)
	}
	return errs
}

// validateElemType checks one element against the list's declared element
// type, using the element's JSON type rather than its text.
//
// RFC 0007's schemas say <string> for LIST[STRING] and LIST[PATH] and
// <integer> for LIST[INT], so the JSON type is what the spec constrains; a
// text check would accept the LIST[PATH] default [123, 456]
// (2.12--list-path-wrong-item-type.invalid). A numeric element may also be a
// string holding a number (an <intstring> for INT, a <floatstring> for FLOAT,
// with white space around it allowed), since Template Schemas §2.13, §2.14
// and §2.16 accept either in a default and in a value given when a Job is
// created.
//
// BOOL is the exception. RFC 0007 gives a LIST[BOOL] item the same
// accepted-values table as a BOOL parameter (true, 1, 1.0, "yes", "on"), so
// it is checked by text, through the same parseBoolParamValue the scalar type
// uses.
func validateElemType(v any, elem JobParamType, ptr string) ValidationErrors {
	bad := func(want string) ValidationErrors {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("%s is not %s", renderElem(v), want),
		}}
	}

	switch elem {
	case JobParamTypeString:
		if _, ok := v.(string); !ok {
			return bad("a string")
		}
	case JobParamTypePath:
		if _, ok := v.(string); !ok {
			return bad("a path (a string)")
		}
	case JobParamTypeInt:
		if s, ok := v.(string); ok {
			return numericStringElem(s, elem, bad)
		}
		f, ok := v.(float64)
		if !ok {
			return bad("an integer")
		}
		if f != math.Trunc(f) {
			return bad("an integer")
		}
	case JobParamTypeFloat:
		if s, ok := v.(string); ok {
			return numericStringElem(s, elem, bad)
		}
		if _, ok := v.(float64); !ok {
			return bad("a number")
		}
	case JobParamTypeBool:
		text, ok := scalarElemText(v)
		if !ok {
			return bad("a boolean")
		}
		if _, ok := parseBoolParamValue(text); !ok {
			return bad("a boolean; use true/false, 1/0, 1.0/0.0, yes/no, or on/off")
		}
	}
	return nil
}

// numericStringElem checks an INT or FLOAT element given as a string: it must
// be an <intstring> or a finite <floatstring> (Template Schemas §7.5).
func numericStringElem(s string, elem JobParamType, bad func(string) ValidationErrors) ValidationErrors {
	if elem == JobParamTypeInt {
		if _, err := expr.ParseIntString(s); err != nil {
			return bad("an integer")
		}
		return nil
	}
	if f, err := expr.ParseFloatString(s); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return bad("a number")
	}
	return nil
}

// renderElem quotes an element for an error message, so a string element is
// visibly distinguishable from a number that happens to look like one.
func renderElem(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	if text, ok := scalarElemText(v); ok {
		return text
	}
	return fmt.Sprintf("%v", v)
}

// scalarElemText renders a JSON-decoded scalar losslessly.
//
// encoding/json produces float64 for every number, so an integral value must
// format without an exponent or a trailing ".0" -- 1 has to stay "1" so it
// compares equal to an allowedValues entry decoded from YAML as "1".
func scalarElemText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}

// elemInAllowed reports whether an element is one of item.allowedValues. A
// numeric element is compared as a number, as a scalar INT or FLOAT parameter's
// allowedValues are (bind.go), so " 1 " and "1", or "1.0" and "1.00", match;
// any other element is compared as text.
func elemInAllowed(text string, elem JobParamType, allowed []string) bool {
	switch elem {
	case JobParamTypeInt:
		n, err := expr.ParseIntString(text)
		return err == nil && intInAllowed(n, allowed)
	case JobParamTypeFloat:
		f, err := expr.ParseFloatString(text)
		return err == nil && floatInAllowed(f, allowed)
	}
	return slices.Contains(allowed, text)
}

// validateElemAgainstConstraint applies the item: block for this level to one
// element. Which constraints are LEGAL here was already settled by
// validateItemConstraintLegality, so this only applies them.
func validateElemAgainstConstraint(
	text string, elem JobParamType, c *ItemConstraint, ptr string,
) ValidationErrors {
	if c == nil {
		return nil
	}
	var errs ValidationErrors

	if len(c.AllowedValues) > 0 && !elemInAllowed(text, elem, c.AllowedValues) {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("%q is not one of item.allowedValues", text),
		})
	}

	switch elem {
	case JobParamTypeString, JobParamTypePath:
		n := utf8.RuneCountInString(text)
		if c.MinLength != nil && n < *c.MinLength {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("length %d is below item.minLength %d", n, *c.MinLength),
			})
		}
		if c.MaxLength != nil && n > *c.MaxLength {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("length %d is above item.maxLength %d", n, *c.MaxLength),
			})
		}
	case JobParamTypeInt, JobParamTypeFloat:
		errs = append(errs, validateElemBounds(text, c, ptr)...)
	}

	return errs
}

// validateElemBounds applies item.minValue/item.maxValue to a numeric element.
//
// Both sides are compared as float64. An INT bound is exactly representable
// far beyond any range a parameter constraint plausibly uses, and one
// comparison keeps this from needing an int and a float branch that must be
// kept in step.
func validateElemBounds(text string, c *ItemConstraint, ptr string) ValidationErrors {
	f, err := expr.ParseFloatString(text)
	if err != nil {
		// validateElemType already reported this element as not a number at
		// the same pointer; returning the parse error would report it twice.
		return nil //nolint:nilerr // the type error is reported by validateElemType
	}

	var errs ValidationErrors
	if c.MinValue != nil {
		if lo, err := expr.ParseFloatString(*c.MinValue); err == nil && f < lo {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("%s is below item.minValue %s", text, *c.MinValue),
			})
		}
	}
	if c.MaxValue != nil {
		if hi, err := expr.ParseFloatString(*c.MaxValue); err == nil && f > hi {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("%s is above item.maxValue %s", text, *c.MaxValue),
			})
		}
	}
	return errs
}

// validateItemConstraintLegality rejects an item: constraint that cannot apply
// to the declared element type -- minValue on a list of strings, minLength on
// a list of ints, a second item level on a flat list.
//
// Catching it here means the element loop can apply constraints without
// re-checking whether each one makes sense, and it follows the same principle
// as the item: depth cap: a constraint the validator silently ignores is worse
// than one it rejects, because the author believes it is in force.
func validateItemConstraintLegality(
	c *ItemConstraint, elem JobParamType, ptr string,
) ValidationErrors {
	if c == nil {
		return nil
	}

	numeric := elem == JobParamTypeInt || elem == JobParamTypeFloat
	// An inner LIST's minLength/maxLength bound its element count, which is
	// why LIST[INT] counts as "lengthy" here alongside the string-like types.
	lengthy := elem == JobParamTypeString || elem == JobParamTypePath ||
		elem == JobParamTypeListInt

	errs := rejectFields(
		ptr, fmt.Sprintf("an element of type %s", elem),
		field{c.MinValue != nil && !numeric, "minValue"},
		field{c.MaxValue != nil && !numeric, "maxValue"},
		field{c.MinLength != nil && !lengthy, "minLength"},
		field{c.MaxLength != nil && !lengthy, "maxLength"},
		field{c.AllowedValuesSet && elem == JobParamTypeBool, "allowedValues"},
		field{c.Item != nil && elem != JobParamTypeListInt, "item"},
	)

	if c.Item != nil && elem == JobParamTypeListInt {
		errs = append(errs, validateItemConstraintLegality(
			c.Item, JobParamTypeInt, ptr+"/item",
		)...)
	}
	return errs
}

// field pairs "this key was declared" with the key's name, for rejectFields.
type field struct {
	set  bool
	name string
}

// rejectFields reports one error per declared field that the parameter type
// does not define.
//
// A constraint the validator silently ignores is worse than one it rejects:
// the template author believes it is in force, and nothing ever tells them
// otherwise. subject names the type in the message ("a BOOL parameter").
func rejectFields(ptr, subject string, fields ...field) ValidationErrors {
	var errs ValidationErrors
	for _, f := range fields {
		if f.set {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/" + f.name,
				Message: "not valid on " + subject,
			})
		}
	}
	return errs
}

// unknownJobTypeMessage names the types available in this template's context.
// A base-spec template is offered only the base-spec types, since listing
// EXPR's types would point its author at types the validator then rejects.
// Tests assert the base-spec wording.
func unknownJobTypeMessage(t JobParamType, exprDeclared bool) string {
	if exprDeclared {
		return fmt.Sprintf("unknown type %q; must be INT, FLOAT, STRING, PATH, BOOL, "+
			"RANGE_EXPR, LIST[STRING], LIST[PATH], LIST[INT], LIST[FLOAT], LIST[BOOL], "+
			"or LIST[LIST[INT]]", t)
	}
	return fmt.Sprintf("unknown type %q; must be INT, FLOAT, STRING, or PATH", t)
}
