// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/openjd/intrange"
)

// This file validates the job-parameter types RFC 0007 (the EXPR extension's
// Extended Parameter Types) adds: BOOL, RANGE_EXPR, and the six LIST[*] types.
//
// It is separate from validate.go because that file is already ~2900 lines and
// these validators share one responsibility. Every function here is reached
// only from validateJobParams' type switch, and only for a template that
// declares the EXPR extension -- the switch guards that, so nothing in this
// file needs to re-check it.

// validateEXPRParamType dispatches to the right validator for a type RFC 0007
// adds, or reports the type as unknown when the template does not declare the
// EXPR extension.
//
// THE GATE HERE IS NOT REDUNDANT with the decoder's. Only an EXPR template
// canonicalizes a spelling, but a base-spec template that writes the canonical
// form verbatim -- "BOOL", "LIST[INT]" -- lands on these switch arms with
// exprDeclared false, because the verbatim text is byte-identical to the
// constant. Without this check such a template would be validated against
// rules it never opted into. (The same shape of leak, in the parse-time list
// default reader, is pinned by
// TestListDefault_ListTypeWithoutEXPRStillRejected.)
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
// The table is CLOSED. "y", "t" and "2" are not accepted: a template author who
// means true already has four spellings, and a fifth guess is far more likely
// to be a typo than an intent. 2.9--bool-param-{int,float,string}-invalid
// exist to pin exactly that.
//
// The value arrives as text because [JobParameter.Default] is a *string for
// every type -- a YAML `true` and a YAML `"true"` are indistinguishable here,
// which is what the RFC's table wants anyway: both are true.
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

	// RFC 0007 states the carve-out explicitly: BOOL does not support
	// allowedValues, because restricting a two-valued type to one value
	// constrains nothing. AllowedValuesSet rather than len() is what catches
	// `allowedValues: []`, which decodes empty and would otherwise pass.
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

	if p.Default != nil && !strings.Contains(*p.Default, "{{") {
		if _, ok := parseBoolParamValue(*p.Default); !ok {
			errs = append(errs, ValidationError{
				Pointer: ptr + "/default",
				Message: fmt.Sprintf(
					"%q is not a boolean; use true/false, 1/0, 1.0/0.0, yes/no, or on/off",
					*p.Default,
				),
			})
		}
	}

	return errs
}

// defaultRangeExprMaxLength is RFC 0007's maxLength default for RANGE_EXPR.
// It applies when the template declares no maxLength of its own.
const defaultRangeExprMaxLength = 1024

// validateRangeExprParamConstraints checks a RANGE_EXPR parameter.
//
// THE DEFAULT IS PARSED WITH THE SPEC'S POLICY, NOT internal/openjd's. This is
// the single most consequential decision in sub-project F1, so it is written
// here rather than left to be re-derived:
//
// internal/openjd parses <IntRangeExpr> more strictly than the specification
// on purpose -- openjdRangePolicy sets PositiveStepOnly and AscendingOnly, so
// "10-1:-1" and "-1--10:-1" are rejected as literal base-spec range text (see
// this repo's CLAUDE.md, which records that divergence and preserves it). The
// conformance fixture 2.10--range-expr-param.yaml declares both of those as
// RANGE_EXPR defaults, so under the strict policy that fixture could never
// pass.
//
// The permissive policy is also the one the value meets downstream, which is
// what makes this consistent rather than merely convenient: sub-project E4b
// ruled that the LONE whole-field form range: "{{Param.FrameRange}}" evaluates
// as a VALUE through range_expr's own list[int] coercion and never re-enters
// <IntRangeExpr> text at all, and expr.ValueFromText's CodeRangeExpr case
// already calls the permissive expr.RangeExpr. Literal base-spec range text --
// including text assembled from an EMBEDDED reference, which E4b ruled on
// separately -- keeps the strict policy. F1 does not touch that.
//
// minLength and maxLength bound the expression's STRING length, not its
// expanded element count. A range expression is a compact notation precisely
// so that "1-1000000" is nine characters; bounding the expansion here would
// measure the wrong thing entirely, and expansion cost is bounded elsewhere.
func validateRangeExprParamConstraints(p JobParameter, ptr string) ValidationErrors {
	errs := rejectFields(
		ptr, "a RANGE_EXPR parameter",
		field{p.MinValue != nil, "minValue"},
		field{p.MaxValue != nil, "maxValue"},
		field{p.AllowedValuesSet, "allowedValues"},
		field{p.Item != nil, "item"},
	)

	if p.Default == nil || strings.Contains(*p.Default, "{{") {
		return errs
	}

	if _, err := intrange.Parse(*p.Default); err != nil {
		return append(errs, ValidationError{
			Pointer: ptr + "/default",
			Message: fmt.Sprintf("not a valid range expression: %v", err),
		})
	}

	return append(errs, validateRangeExprLength(*p.Default, p, ptr)...)
}

// validateRangeExprLength applies minLength and maxLength to a RANGE_EXPR
// default's string length, substituting RFC 0007's 1024 default when the
// template declares no maximum.
func validateRangeExprLength(def string, p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	n := utf8.RuneCountInString(def)

	if p.MinLength != nil && n < *p.MinLength {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/default",
			Message: fmt.Sprintf("length %d is below minLength %d", n, *p.MinLength),
		})
	}

	maxLen := defaultRangeExprMaxLength
	if p.MaxLength != nil {
		maxLen = *p.MaxLength
	}
	if n > maxLen {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/default",
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
	// scalar types use (validateParamValueConstraints).
	if p.Default == nil || strings.Contains(*p.Default, "{{") {
		return errs
	}

	items, err := decodeListDefault(*p.Default, p.Type)
	if err != nil {
		return ValidationErrors{{Pointer: ptr + "/default", Message: err.Error()}}
	}

	errs = append(errs, validateListLength(len(items), p, ptr)...)
	for i, item := range items {
		errs = append(errs, validateListElement(
			item, elem, p.Item, fmt.Sprintf("%s/default/%d", ptr, i),
		)...)
	}
	return errs
}

// validateListLength applies the list's own minLength/maxLength, which bound
// the ELEMENT COUNT (RFC 0007) rather than any element's own length.
func validateListLength(n int, p JobParameter, ptr string) ValidationErrors {
	var errs ValidationErrors
	if p.MinLength != nil && n < *p.MinLength {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/default",
			Message: fmt.Sprintf("has %d items, below minLength %d", n, *p.MinLength),
		})
	}
	if p.MaxLength != nil && n > *p.MaxLength {
		errs = append(errs, ValidationError{
			Pointer: ptr + "/default",
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
// type, using the element's JSON TYPE rather than its text.
//
// The distinction is load-bearing: 2.12--list-path-wrong-item-type.invalid
// declares LIST[PATH] with default [123, 456], and a text-based check would
// accept those -- every number renders as a perfectly good path string. RFC
// 0007's schemas say <string> for LIST[STRING] and LIST[PATH] and <integer>
// for LIST[INT], so the JSON type is what the spec actually constrains.
//
// BOOL is the deliberate exception. RFC 0007 gives a LIST[BOOL] item the same
// accepted-values table as a BOOL parameter -- true, 1, 1.0, "yes", "on" --
// so its check is by TEXT, through the same parseBoolParamValue the scalar
// type uses. Keeping the two in one function would have meant one of them
// being wrong.
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
		f, ok := v.(float64)
		if !ok {
			return bad("an integer")
		}
		if f != math.Trunc(f) {
			return bad("an integer")
		}
	case JobParamTypeFloat:
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

	if len(c.AllowedValues) > 0 && !slices.Contains(c.AllowedValues, text) {
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
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// Deliberately swallowed: validateElemType already rejected this
		// element as not-a-number and reported it with this same pointer.
		// Returning the parse error too would report one defect twice, in
		// two different vocabularies.
		return nil //nolint:nilerr // the type error is reported by validateElemType
	}

	var errs ValidationErrors
	if c.MinValue != nil {
		if lo, err := strconv.ParseFloat(*c.MinValue, 64); err == nil && f < lo {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("%s is below item.minValue %s", text, *c.MinValue),
			})
		}
	}
	if c.MaxValue != nil {
		if hi, err := strconv.ParseFloat(*c.MaxValue, 64); err == nil && f > hi {
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
	// An inner LIST's minLength/maxLength bound its ELEMENT COUNT, which is
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

// unknownJobTypeMessage names the types actually available in this template's
// context.
//
// Listing EXPR's types to a base-spec author would send them to a type the
// validator rejects on the next line, so the base-spec wording is kept exactly
// as it was before RFC 0007 -- existing tests assert it, and a base-spec
// template's diagnostics must not change at all.
func unknownJobTypeMessage(t JobParamType, exprDeclared bool) string {
	if exprDeclared {
		return fmt.Sprintf("unknown type %q; must be INT, FLOAT, STRING, PATH, BOOL, "+
			"RANGE_EXPR, LIST[STRING], LIST[PATH], LIST[INT], LIST[FLOAT], LIST[BOOL], "+
			"or LIST[LIST[INT]]", t)
	}
	return fmt.Sprintf("unknown type %q; must be INT, FLOAT, STRING, or PATH", t)
}
