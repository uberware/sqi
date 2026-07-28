// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Value is an evaluated expression result.
//
// Type is always meaningful. The payload fields are valid only for their
// corresponding type code, and reading the wrong one panics. An unresolved value
// — a placeholder whose type is known but whose value is not — carries NO
// payload at all, which is the case the type-tag-separate-from-payload shape
// exists to make possible.
//
// Because Type contains a slice, Value is NOT comparable: use Equal, not "==".
type Value struct {
	// Type is the value's type. Always set.
	Type Type

	// Payload. At most one is valid, selected by Type.Code; null and unresolved
	// values have none.
	b bool
	i int64
	f float64
	s string
}

// Null returns the null value.
func Null() Value { return Value{Type: TNull} }

// Bool returns a boolean value.
func Bool(b bool) Value { return Value{Type: TBool, b: b} }

// Int returns a 64-bit signed integer value.
func Int(i int64) Value { return Value{Type: TInt, i: i} }

// Float returns a 64-bit IEEE floating-point value.
func Float(f float64) Value { return Value{Type: TFloat, f: f} }

// String returns a string value.
func String(s string) Value { return Value{Type: TString, s: s} }

// Unresolved returns a placeholder for a value that is not yet known but whose
// type satisfies the given constraint.
//
// This is the mechanism the spec's static type checking is built on: an
// expression is evaluated with placeholders in the symbol table, operations
// propagate the type, and a type error surfaces without any value existing. It
// is safe because the language is side-effect free.
//
// The type constructor with the -Of suffix, UnresolvedOf, builds the Type; this
// builds the Value.
func Unresolved(constraint Type) Value { return Value{Type: UnresolvedOf(constraint)} }

// IsUnresolved reports whether v is a placeholder rather than a known value.
func (v Value) IsUnresolved() bool { return v.Type.Code == CodeUnresolved }

// IsNull reports whether v is the null value.
func (v Value) IsNull() bool { return v.Type.Code == CodeNull }

// AsBool returns the boolean payload. It panics if v is not a bool.
func (v Value) AsBool() bool { v.mustBe(CodeBool); return v.b }

// AsInt returns the integer payload. It panics if v is not an int.
func (v Value) AsInt() int64 { v.mustBe(CodeInt); return v.i }

// AsFloat returns the floating-point payload. It panics if v is not a float.
func (v Value) AsFloat() float64 { v.mustBe(CodeFloat); return v.f }

// AsStr returns the string payload. It panics if v is not a string.
//
// Named AsStr rather than AsString because String is the fmt.Stringer
// rendering; two methods one letter apart would be a standing trap.
func (v Value) AsStr() string { v.mustBe(CodeString); return v.s }

// mustBe panics unless v's type has the given code. Reading the wrong payload is
// a bug in the operator dispatch table, not a runtime condition, and a silent
// zero would flow into a rendered command line unnoticed.
func (v Value) mustBe(c Code) {
	if v.Type.Code != c {
		panic(fmt.Sprintf("expr: read %s payload from a %s value", c, v.Type))
	}
}

// Equal reports whether two values have the same type and the same payload.
//
// Value is not comparable with "==" because Type contains a slice, so this is the
// only way to compare two values for identity. It is NOT the language's "=="
// operator: section 1.2.5 makes that cross-type, so 5 == 5.0 is true there while
// Int(5).Equal(Float(5)) is false here.
func (v Value) Equal(o Value) bool {
	if !v.Type.Equal(o.Type) {
		return false
	}
	switch v.Type.Code {
	case CodeBool:
		return v.b == o.b
	case CodeInt:
		return v.i == o.i
	case CodeFloat:
		return v.f == o.f
	case CodeString, CodePath, CodeRangeExpr:
		return v.s == o.s
	}
	// Null and unresolved carry no payload, so equal types are equal values.
	return true
}

// String renders the value as text.
//
// Floats use the shortest representation that round-trips, with a ".0" added
// when the result would otherwise look like an integer, so a float never
// renders indistinguishably from an int. Null renders as "null" and booleans
// as "true"/"false" — the JSON/YAML spellings, matching the aliases in spec
// section 1.1.4 and the string(nulltype) -> "null" rule in section 2.2.1.
//
// This is a rendering for diagnostics and tests. The definitive form used
// when interpolating a value back into a template is sub-project E's to fix,
// and section 1.3.4's float pass-through rule belongs to it.
func (v Value) String() string {
	switch v.Type.Code {
	case CodeNull:
		return "null"
	case CodeBool:
		return strconv.FormatBool(v.b)
	case CodeInt:
		return strconv.FormatInt(v.i, 10)
	case CodeFloat:
		return formatFloat(v.f)
	case CodeString, CodePath, CodeRangeExpr:
		return v.s
	case CodeUnresolved:
		// A placeholder has no value to render, so name what is known instead.
		return "<" + v.Type.String() + ">"
	}
	return "<" + v.Type.String() + ">"
}

// formatFloat renders a float the way Python's repr does, which is the form
// the spec's own examples use: 1e10 is "10000000000.0", not "1e+10".
//
// strconv's 'g' gives the shortest round-tripping digits but switches to
// exponent notation far earlier than Python, so a magnitude Python would print
// in full is re-rendered with 'f'. Outside Python's fixed-notation window —
// below 1e-4 or at 1e16 and above — exponent notation is correct and is kept.
// A ".0" is appended when the result would otherwise be indistinguishable from
// an integer.
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, "eE") {
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	if abs := math.Abs(f); abs >= 1e-4 && abs < 1e16 {
		fixed := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(fixed, ".") {
			fixed += ".0"
		}
		return fixed
	}
	return s
}
