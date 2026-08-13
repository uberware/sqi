// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"
)

// BindJobParameters resolves each declared job parameter to a concrete string
// value, validates every value against its type and constraints, and returns
// the fully-bound name→value map.
//
// Resolution order per declared parameter:
//  1. provided[name] — the caller-supplied value, if present.
//  2. p.Default — the template default, if the parameter has one.
//  3. Required-but-missing error: the parameter has neither a supplied value
//     nor a default.
//
// Any key in provided that does not match a declared parameter name produces
// an "unknown parameter" error.  All errors are accumulated before returning
// so callers receive a complete problem list in one round-trip.
//
// Pointer conventions:
//   - Unknown provided key  → /parameterValues/<name>
//   - Required-but-missing  → /parameterDefinitions/<name>
//   - Type / constraint err → /parameterValues/<name>
//
// AllowedValues comparison:
//   - INT   — both value and each allowed entry are parsed as int64;
//     "01" and "1" are considered equal.
//   - FLOAT — both parsed as float64; "1.00" and "1.0" are considered equal.
//   - STRING / PATH — exact string comparison.
//
// On success, returns the resolved map and nil errors.
// On failure, returns (nil, errors).
func BindJobParameters(params []JobParameter, provided map[string]string) (map[string]string, ValidationErrors) {
	var errs ValidationErrors

	// Build a set of declared names for fast lookup.
	declared := make(map[string]struct{}, len(params))
	for _, p := range params {
		declared[p.Name] = struct{}{}
	}

	// Reject any provided key that isn't a declared parameter.
	for name := range provided {
		if _, ok := declared[name]; !ok {
			errs = append(errs, ValidationError{
				Pointer: "/parameterValues/" + name,
				Message: fmt.Sprintf("unknown parameter %q", name),
			})
		}
	}

	// Resolve and validate each declared parameter.
	resolved := make(map[string]string, len(params))
	for _, p := range params {
		value, hasValue := provided[p.Name]
		if !hasValue && p.Default != nil {
			value = *p.Default
			hasValue = true
		}

		if !hasValue {
			errs = append(errs, ValidationError{
				Pointer: "/parameterDefinitions/" + p.Name,
				Message: fmt.Sprintf("required parameter %q has no value and no default", p.Name),
			})
			continue
		}

		if verrs := validateParamValue(p, value); len(verrs) > 0 {
			errs = append(errs, verrs...)
			continue
		}

		resolved[p.Name] = value
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return resolved, nil
}

// validateParamValue checks value against p.Type and all configured constraints.
// All violations for the parameter are accumulated before returning.
func validateParamValue(p JobParameter, value string) ValidationErrors {
	ptr := "/parameterValues/" + p.Name
	switch p.Type {
	case JobParamTypeInt:
		return validateIntParamValue(p, value, ptr)
	case JobParamTypeFloat:
		return validateFloatParamValue(p, value, ptr)
	case JobParamTypeString, JobParamTypePath:
		return validateStringOrPathParamValue(p, value, ptr)
	case JobParamTypeBool, JobParamTypeRangeExpr:
		// The rules are the same ones the declared default is checked against;
		// see checkParamValueAgainstType on why there is only one copy.
		return checkParamValueAgainstType(p, value, ptr)
	}
	if isListParamType(p.Type) {
		return checkParamValueAgainstType(p, value, ptr)
	}
	return nil
}

// validateIntParamValue validates an INT parameter value against its constraints.
func validateIntParamValue(p JobParameter, value, ptr string) ValidationErrors {
	var errs ValidationErrors
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a valid INT", value),
		}}
	}
	if p.MinValue != nil {
		if minV, err := strconv.ParseInt(*p.MinValue, 10, 64); err == nil && n < minV {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("value %d is below minimum %d", n, minV),
			})
		}
	}
	if p.MaxValue != nil {
		if maxV, err := strconv.ParseInt(*p.MaxValue, 10, 64); err == nil && n > maxV {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("value %d is above maximum %d", n, maxV),
			})
		}
	}
	if len(p.AllowedValues) > 0 && !intInAllowed(n, p.AllowedValues) {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not in allowedValues %v", value, p.AllowedValues),
		})
	}
	return errs
}

// validateFloatParamValue validates a FLOAT parameter value against its constraints.
func validateFloatParamValue(p JobParameter, value, ptr string) ValidationErrors {
	var errs ValidationErrors
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a valid FLOAT", value),
		}}
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ValidationErrors{{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not a finite FLOAT", value),
		}}
	}
	if p.MinValue != nil {
		if minV, err := strconv.ParseFloat(*p.MinValue, 64); err == nil && f < minV {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("value %v is below minimum %v", f, minV),
			})
		}
	}
	if p.MaxValue != nil {
		if maxV, err := strconv.ParseFloat(*p.MaxValue, 64); err == nil && f > maxV {
			errs = append(errs, ValidationError{
				Pointer: ptr,
				Message: fmt.Sprintf("value %v is above maximum %v", f, maxV),
			})
		}
	}
	if len(p.AllowedValues) > 0 && !floatInAllowed(f, p.AllowedValues) {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not in allowedValues %v", value, p.AllowedValues),
		})
	}
	return errs
}

// validateStringOrPathParamValue validates a STRING or PATH parameter value against its constraints.
func validateStringOrPathParamValue(p JobParameter, value, ptr string) ValidationErrors {
	var errs ValidationErrors
	n := utf8.RuneCountInString(value)
	if p.MinLength != nil && n < *p.MinLength {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("value length %d is below minimum %d", n, *p.MinLength),
		})
	}
	if p.MaxLength != nil && n > *p.MaxLength {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("value length %d is above maximum %d", n, *p.MaxLength),
		})
	}
	if len(p.AllowedValues) > 0 && !slices.Contains(p.AllowedValues, value) {
		errs = append(errs, ValidationError{
			Pointer: ptr,
			Message: fmt.Sprintf("value %q is not in allowedValues %v", value, p.AllowedValues),
		})
	}
	return errs
}

// intInAllowed returns true if n matches any entry in allowed when parsed as int64.
// Non-parseable allowed entries are silently skipped.
func intInAllowed(n int64, allowed []string) bool {
	for _, av := range allowed {
		if m, err := strconv.ParseInt(av, 10, 64); err == nil && m == n {
			return true
		}
	}
	return false
}

// floatInAllowed returns true if f matches any entry in allowed when parsed as float64.
// Comparison uses == so two strings that produce the same float64 (e.g. "1.0" and "1.00")
// are considered equal. Non-parseable allowed entries are silently skipped.
func floatInAllowed(f float64, allowed []string) bool {
	for _, av := range allowed {
		if g, err := strconv.ParseFloat(av, 64); err == nil && g == f {
			return true
		}
	}
	return false
}
