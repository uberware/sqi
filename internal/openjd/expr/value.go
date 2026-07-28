// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is the type tag of a Value.
//
// Sub-project A implements the five scalar kinds below. Sub-project B adds
// KindPath, KindRangeExpr and KindList; because Kind is a field in its own
// right rather than something inferred from which payload is populated, B can
// also add the spec's unresolved[T] without reshaping Value or anything built
// on it. unresolved[T] carries a type parameter — and can itself be a union
// of types, as when an if/else whose condition is unresolved yields
// unresolved[T | S] — so B will need a type descriptor alongside the Kind
// tag, not merely a payload-free Kind constant; the Kind/payload separation
// is what keeps that addition additive rather than a rewrite.
type Kind int

// The kind constants. Sub-project A produces the five scalar kinds below.
const (
	KindNull Kind = iota
	KindBool
	KindInt
	KindFloat
	KindString
)

var kindNames = map[Kind]string{
	KindNull:   "null",
	KindBool:   "bool",
	KindInt:    "int",
	KindFloat:  "float",
	KindString: "string",
}

func (k Kind) String() string {
	if name, ok := kindNames[k]; ok {
		return name
	}
	return "unknown type"
}

// Value is an evaluated expression result.
//
// Kind is always meaningful; the payload fields are valid only for their
// corresponding Kind, and reading the wrong one panics. The zero Value is a
// null, so a function returning (Value{}, err) never yields an incoherent
// value.
type Value struct {
	// Kind is the value's type. Always set.
	Kind Kind

	// Payload. Exactly one is valid, selected by Kind; a null has none.
	b bool
	i int64
	f float64
	s string
}

// Null returns the null value.
func Null() Value { return Value{Kind: KindNull} }

// Bool returns a boolean value.
func Bool(b bool) Value { return Value{Kind: KindBool, b: b} }

// Int returns a 64-bit signed integer value.
func Int(i int64) Value { return Value{Kind: KindInt, i: i} }

// Float returns a 64-bit IEEE floating-point value.
func Float(f float64) Value { return Value{Kind: KindFloat, f: f} }

// String returns a string value.
func String(s string) Value { return Value{Kind: KindString, s: s} }

// IsNull reports whether v is the null value.
func (v Value) IsNull() bool { return v.Kind == KindNull }

// AsBool returns the boolean payload. It panics if v is not a bool.
func (v Value) AsBool() bool { v.mustBe(KindBool); return v.b }

// AsInt returns the integer payload. It panics if v is not an int.
func (v Value) AsInt() int64 { v.mustBe(KindInt); return v.i }

// AsFloat returns the floating-point payload. It panics if v is not a float.
func (v Value) AsFloat() float64 { v.mustBe(KindFloat); return v.f }

// AsStr returns the string payload. It panics if v is not a string.
//
// Named AsStr rather than AsString because String is the fmt.Stringer
// rendering; two methods one letter apart would be a standing trap.
func (v Value) AsStr() string { v.mustBe(KindString); return v.s }

// mustBe panics unless v has the given kind. Reading the wrong payload is a
// bug in the operator dispatch table, not a runtime condition, and a silent
// zero would flow into a rendered command line unnoticed.
func (v Value) mustBe(k Kind) {
	if v.Kind != k {
		panic(fmt.Sprintf("expr: read %s payload from a %s value", k, v.Kind))
	}
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
	switch v.Kind {
	case KindNull:
		return "null"
	case KindBool:
		return strconv.FormatBool(v.b)
	case KindInt:
		return strconv.FormatInt(v.i, 10)
	case KindFloat:
		return formatFloat(v.f)
	case KindString:
		return v.s
	}
	return ""
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
