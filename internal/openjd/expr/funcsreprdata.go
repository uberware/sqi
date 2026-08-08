// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// reprDataFuncs is sub-project C3's SERIALIZATION group: repr_py and
// repr_json. See funcsreprshell.go for why the shell quoting functions live
// apart from these.
//
// Each table pairs a type-variable catch-all with specific nulltype,
// range_expr and path rows, which LOOKS like C1's flatten hazard a third
// time — matchShapesExactFirst breaks an exact cost tie to the EARLIEST
// shape, so a specific row ahead of a catch-all matters when the two can tie.
// Here they cannot: row order is declared this way for spec fidelity (RFC
// 0006 calls out null/range_expr/path by name), but pyRepr and jsonRepr each
// switch on v.Type.Code directly, so the catch-all's Fn renders
// CodeNull/CodePath/CodeRangeExpr exactly the same way the dedicated rows do
// — reordering the rows was tried during development and every test still
// passed. TestReprData_RendersSpecificTypes pins the OUTPUT, not the row
// order, and says so in its own doc: that test structurally CANNOT tell the
// rows apart. If a future change ever gives the catch-all branch behavior
// that diverges from the dedicated rows (for example, by narrowing that
// switch), it must add a test that can — this one can't.
// COST (sub-project E1, Task 8): rule 2 names repr_py() and repr_json() by
// name; rule 3 does not name either (only repr_sh()), but the reference's own
// measured count SCALES with a string argument's byte length for both
// (10/300/600 bytes -> 2/3/4) -- rule 3's "and similar" catch-all covers work
// that is proportional to string length even where the enumeration is silent,
// the same reading Task 6 and Task 7 already established. So the catch-all
// varT row below declares BOTH Cost{ArgElements: {0}} (rule 2, over a list --
// a DIVERGENCE, see below) and Cost{ArgBytes: {0}} (rule 3, over a string):
// chargeArgs reads args[0].s for the byte charge (empty, hence zero, for a
// list/int/float/bool argument -- only a string populates it) and
// elementCount for the element charge (zero for anything but a list/
// range_expr), so declaring both on the SAME row is safe and never
// double-charges a single argument.
//
// The DIVERGENCE: the reference's own count for a LIST argument to repr_py or
// repr_json stays flat at 1 regardless of element count (5 or 20 -- see the
// PROBE in cost_misc_internal_test.go), the same omission repr_pwsh's and
// repr_cmd's list rows have (funcsreprshell.go) despite rule 2 naming all
// four functions by name alongside repr_sh, whose list row DOES scale in the
// reference. Per the standing rule, sqi charges ArgElements here regardless.
//
// TNull and TRangeExpr get their OWN zero-Cost rows rather than falling
// through the catch-all's ArgBytes/ArgElements (which would also charge zero
// for them, since neither populates .s or is a list): TNull's payload is a
// glyph, and rule 3 does not apply to range_expr at all (confirmed flat
// against a 4444-byte range_expr TEXT in the PROBE -- range_expr's own
// constructor already carries the identical "no Cost" ruling, see
// funcsconv.go's "range_expr" entry). TPath gets Cost{ArgBytes: {0}}: rule 3
// explicitly covers path values, and the reference confirms scaling (11/299
// bytes -> total 4/6, matching path()'s own construction charge plus this
// row's).
var reprDataFuncs = map[string][]Shape{
	"repr_py": {
		{Params: []Type{TNull}, Ret: TString, Fn: func([]Value) (Value, error) {
			return String("None"), nil
		}},
		{Params: []Type{TRangeExpr}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(pyRepr(String(args[0].String())))
		}},
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(pyRepr(String(pathText(args[0]))))
		}},
		{Params: []Type{varT}, Ret: TString, Cost: Cost{ArgElements: []int{0}, ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(pyRepr(args[0]))
		}},
	},
	"repr_json": {
		{Params: []Type{TNull}, Ret: TString, Fn: func([]Value) (Value, error) {
			return String("null"), nil
		}},
		{Params: []Type{TRangeExpr}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(jsonRepr(String(args[0].String())))
		}},
		{Params: []Type{TPath}, Ret: TString, Cost: Cost{ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(jsonRepr(String(pathText(args[0]))))
		}},
		{Params: []Type{varT}, Ret: TString, Cost: Cost{ArgElements: []int{0}, ArgBytes: []int{0}}, Fn: func(args []Value) (Value, error) {
			return boundedString(jsonRepr(args[0]))
		}},
	},
}

// pyRepr renders a value the way Python's repr does, which RFC 0006 names as
// repr_py's behavior.
func pyRepr(v Value) string {
	switch v.Type.Code {
	case CodeNull:
		return "None"
	case CodeBool:
		if v.AsBool() {
			return "True"
		}
		return "False"
	case CodeList:
		parts := make([]string, len(v.AsList()))
		for i, elem := range v.AsList() {
			parts[i] = pyRepr(elem)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case CodeString, CodePath, CodeRangeExpr:
		return pyQuote(v.s)
	default:
		// int and float already spell themselves the way Python does, and
		// formatFloat deliberately reproduces Python's float repr.
		return v.String()
	}
}

// pyQuote is Python's string repr, including its quote selection: prefer a
// single quote, but switch to a double quote when the string contains a single
// quote and no double quote.
//
// The reference escapes instead of switching, giving 'it\'s' where Python and
// this give "it's". RFC 0006 names repr, so the specification is on sqi's side
// and the difference is baselined.
func pyQuote(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case !unicode.IsPrint(r) && r < 0x100:
			// Python's repr uses a two-hex-digit \xXX escape for every
			// non-printable code point below U+0100 — not just the C0
			// controls and DEL, but also the C1 controls (U+0080..U+009F)
			// and the handful of Latin-1 Supplement non-printables (U+00A0
			// NBSP, U+00AD SOFT HYPHEN). Measured against python3's repr().
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r) && r <= 0xFFFF:
			// U+0100..U+FFFF: four hex digits.
			fmt.Fprintf(&b, `\u%04x`, r)
		case !unicode.IsPrint(r):
			// Above the Basic Multilingual Plane, Python switches to a
			// CAPITAL \U escape with eight hex digits. %04x for these runes
			// would silently emit MORE than four digits — r=0x10000 in hex
			// is "10000", five digits, so a naive \u escape would render
			// five-plus digits after the \u, which is not a valid Python
			// string escape at all. This tier cannot be folded into the \u
			// one above it for that reason.
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			// Printable — ASCII or otherwise — is left exactly as it reads.
			//
			// Go's unicode.IsPrint and Python's str.isprintable() disagree
			// on a small number of BMP code points (observed: U+0897,
			// U+1B4E, U+1B4F, U+2FFC..U+2FFF and similar combining marks
			// and ideographic description characters), because the two
			// standard libraries bundle different Unicode Character
			// Database versions. That is version skew between the two
			// implementations, not a bug in this function — do not chase
			// it.
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// jsonRepr renders a value as JSON the way Python's json.dumps does by
// default, which is what the reference produces.
//
// It deliberately does NOT reuse writeJSONValue (funcsconv.go). The two differ,
// and so do their counterparts in the reference: measured, string(['café']) is
// ["café"] there — non-ASCII left literal — while repr_json('café') is
// "caf\u00e9", matching json.dumps' default ensure_ascii escaping. Go's
// encoding/json would be wrong for this function in both directions — it
// escapes "<", ">" and "&", which json.dumps does not, and leaves non-ASCII
// literal, which json.dumps escapes.
func jsonRepr(v Value) string {
	switch v.Type.Code {
	case CodeNull:
		return "null"
	case CodeBool:
		if v.AsBool() {
			return "true"
		}
		return "false"
	case CodeList:
		parts := make([]string, len(v.AsList()))
		for i, elem := range v.AsList() {
			parts[i] = jsonRepr(elem)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case CodeString, CodePath, CodeRangeExpr:
		return jsonQuote(v.s)
	default:
		return v.String()
	}
}

// jsonQuote is json.dumps' string escaping: ASCII-only output, with every
// non-ASCII rune as a generic four-hex-digit escape and astral runes as a
// surrogate pair of them.
//
// json.dumps' escaped set is every code point OUTSIDE the printable ASCII
// range space (U+0020) through tilde (U+007E) — not just the C0 controls
// (U+0000..U+001F). That includes DEL (U+007F), which has no named escape
// and so gets the generic four-hex-digit form, and two of the C0 controls
// that DO have named escapes json.dumps prefers over the generic form:
// U+0008 BACKSPACE and U+000C FORM FEED. Measured against python3's
// json.dumps().
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r < utf8.RuneSelf:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
