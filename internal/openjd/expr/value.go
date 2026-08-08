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
	// values have none. Payloads: b for bool, i for int, f for float, s for
	// string/path/rangeexpr, l for list.
	b bool
	i int64
	f float64
	s string
	l []Value

	// fs is an OPTIONAL rendered form for a float, and the only value in this
	// struct that is presentation rather than data.
	//
	// RFC 0006 requires round(3.5, 2) to produce a value that renders "3.50",
	// which no float64 can carry. Section 1.3.4 describes the same mechanism
	// for a different producer — a float parameter submitted as "3.500" keeps
	// that form until something computes on it. That needs the original
	// submitted string, which sqi has at phase 2 (job parameters are stored as
	// map[string]string): FloatText, below, binds it in
	// internal/openjd/exprcheck.go's concreteJobParamValue. round() and a
	// phase-2 float job parameter are the two producers today. Substituting
	// the carried text back into a rendered template is a separate,
	// still-unimplemented step — sub-project E4's, not this field's.
	//
	// Three invariants make it mostly safe. Float() never sets it. Equal
	// ignores it, so no comparison anywhere changes. And nothing ARITHMETIC
	// propagates it: an operation on a float returns through
	// Float()/floatValue() and therefore drops it, which IS section 1.3.4's
	// rule rather than a shortcut around it.
	//
	// The third invariant is NARROWER than it sounds: repr_py, repr_json and
	// repr_pwsh (funcsreprdata.go, funcsreprshell.go) each fall through a
	// default case that renders int and float via Value.String() -- which
	// reads fs first -- so all three leak the carry, e.g. repr_py(round(3.5,
	// 2)) is "3.50", not the canonical "3.5" a fresh float64 would repr as.
	// This is PRE-EXISTING (round() has carried since sub-project C1; a
	// second producer, a phase-2 job parameter's submitted text via
	// FloatText, does not change it) and left AS-IS on purpose: whether
	// repr_* should show a value's carried text or canonicalise it is
	// underspecified: section 1.3.4 (the specification, not RFC 0006, which
	// defines repr_* but says nothing about the carry) addresses substitution
	// and arithmetic, not a function that emits another language's literal
	// syntax. Parked for sub-project E4 to settle, not fixed here.
	fs string

	// pf is the path flavor for a CodePath value.
	//
	// It rides on the value so that every path operation after construction —
	// properties, "/", "with_*" — reads the flavor from its receiver rather
	// than from the evaluator. That is what confines Shape.FnCtx to the rows
	// that genuinely have to choose one — path()'s, and nothing else.
	//
	// The zero value is PathPOSIX, which is also the evaluator's default, so a
	// path Value built by a caller without setting it behaves as POSIX.
	pf PathFormat
}

// Null returns the null value.
func Null() Value { return Value{Type: TNull} }

// Bool returns a boolean value.
func Bool(b bool) Value { return Value{Type: TBool, b: b} }

// Int returns a 64-bit signed integer value.
func Int(i int64) Value { return Value{Type: TInt, i: i} }

// Float returns a 64-bit IEEE floating-point value.
func Float(f float64) Value { return Value{Type: TFloat, f: f} }

// floatRendered returns a float value that renders as the given text instead of
// through formatFloat. See the fs field for why this exists and what may set it.
func floatRendered(f float64, s string) Value { return Value{Type: TFloat, f: f, fs: s} }

// FloatText returns a float value that renders as text instead of through the
// default float formatting.
//
// It exists for section 1.3.4: a FLOAT job parameter submitted as "3.500"
// must keep that exact text rather than the canonical "3.5". sqi stores
// submitted parameters as map[string]string, so the original text is
// available verbatim wherever a concrete value is bound from it —
// internal/openjd/exprcheck.go's concreteJobParamValue is that call site
// today. See the fs field's comment for the invariants this carry keeps —
// including the repr_* exception to "no operation propagates it".
//
// CONTRACT the caller must uphold, unchecked at runtime: text must be a
// textual representation of f, e.g. the exact source strconv.ParseFloat(text,
// 64) produced f from -- as concreteJobParamValue's call does, parsing raw
// into f and then passing that SAME raw back as text. Nothing here verifies
// this. A mismatched pair (say, FloatText(3.5, "9000")) builds a Value whose
// NUMBER is 3.5 -- what Equal, arithmetic, and every other operation compare
// and compute with -- but whose TEXT is "9000" -- what String() (and
// therefore repr_py/repr_json/repr_pwsh) reports. The two would silently
// disagree for as long as the value's fs form is read instead of computed.
func FloatText(f float64, text string) Value { return floatRendered(f, text) }

// String returns a string value.
func String(s string) Value { return Value{Type: TString, s: s} }

// Path returns a path value with the given flavor, normalized on the way in.
func Path(text string, f PathFormat) Value {
	p := parsePath(text, f)
	return Value{Type: TPath, s: p.String(), pf: p.flavor}
}

// pathOf re-parses a path value into its structure.
func pathOf(v Value) parsedPath { return parsePath(pathText(v), v.pf) }

// List returns a list value with the given element type and elements.
//
// The element type is passed explicitly rather than inferred from vals because
// an EMPTY list still has one: "[]" is list[nulltype] (section 1.2.6), and
// section 1.2.6's inference rules can also give a list an element type no
// element has exactly — [1, 2.0] is list[float] after its elements are coerced.
func List(elem Type, vals []Value) Value {
	return Value{Type: ListOf(elem), l: vals}
}

// AsList returns the list payload. It panics if v is not a list.
func (v Value) AsList() []Value { v.mustBe(CodeList); return v.l }

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
	case CodeNull, CodeUnresolved:
		// Null and unresolved carry no payload, so equal types are equal
		// values.
		return true
	case CodeList:
		if len(v.l) != len(o.l) {
			return false
		}
		for i := range v.l {
			if !v.l[i].Equal(o.l[i]) {
				return false
			}
		}
		return true
	}
	// A payload-carrying Code with no case above is a defect, not a value this
	// function has ever seen: falling through to "equal types means equal
	// values" — the old bare default this replaced — would silently make two
	// DIFFERENT payloads compare equal, in a function roughly 30 tests across
	// this package depend on. Panic rather than guess; this can only fire when
	// a new Code gains a payload field without a matching case here.
	panic(fmt.Sprintf("expr: Value.Equal has no case for %s", v.Type))
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
		if v.fs != "" {
			return v.fs
		}
		return formatFloat(v.f)
	case CodeString, CodePath, CodeRangeExpr:
		return v.s
	case CodeList:
		parts := make([]string, len(v.l))
		for i, elem := range v.l {
			// Section 1.2.1's string-like codes render QUOTED inside a list, so
			// ["a", "b"] rather than [a, b]. A bare rendering was ambiguous --
			// ['a, b'] and ['a', 'b'] both printed as [a, b] -- and diverged
			// from the reference on 31 corpus cases across list literals,
			// coercion, comprehensions, flatten/sorted/unique, split/rsplit,
			// the regex family and the path .parts/.suffixes properties.
			switch elem.Type.Code {
			case CodeString, CodePath, CodeRangeExpr:
				parts[i] = strconv.Quote(elem.s)
			default:
				parts[i] = elem.String()
			}
		}
		return "[" + strings.Join(parts, ", ") + "]"
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
