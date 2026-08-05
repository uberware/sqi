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
// ROW ORDER IS LOAD-BEARING in both tables. Each pairs a type-variable
// catch-all with specific nulltype, range_expr and path rows, and
// matchShapesExactFirst breaks an exact tie to the EARLIEST shape — so the
// specific rows must come first or they never run. This is C1's flatten hazard
// for the third time; TestReprData_SpecificRowsBeatTheVariableRow pins it.
var reprDataFuncs = map[string][]Shape{
	"repr_py": {
		{Params: []Type{TNull}, Ret: TString, Fn: func([]Value) (Value, error) {
			return String("None"), nil
		}},
		{Params: []Type{TRangeExpr}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(pyRepr(String(args[0].String())))
		}},
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(pyRepr(String(pathText(args[0]))))
		}},
		{Params: []Type{varT}, Ret: TString, Fn: func(args []Value) (Value, error) {
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
		{Params: []Type{TPath}, Ret: TString, Fn: func(args []Value) (Value, error) {
			return boundedString(jsonRepr(String(pathText(args[0]))))
		}},
		{Params: []Type{varT}, Ret: TString, Fn: func(args []Value) (Value, error) {
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
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < utf8.RuneSelf || unicode.IsPrint(r):
			// Python's repr leaves printable non-ASCII literal.
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
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
// non-ASCII rune as \uXXXX and astral runes as a surrogate pair.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
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
