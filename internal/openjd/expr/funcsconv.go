// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// convFuncs is RFC 0006's general-function group: the conversions between
// scalar types, plus len. fail (this same file, its own RFC 0006 category
// despite living beside the conversions here) rounds out sub-project C1's
// share of the table. A later sub-project never edits this table — C2, C3 and
// C4 add their own groups in their own files (funcsstrcase.go,
// funcsstrfind.go, funcsstrsplit.go, funcsstrpad.go, funcsre.go,
// funcsrepr.go, funcspath.go) and their own entry in funcs.go's mergeFuncs
// call.
var convFuncs = map[string][]Shape{
	// Section 1.3.10 rule 3 names len() BY NAME as its one explicit exemption:
	// "Simple lookups like len() that do not process the string content do not
	// add to the count." All four rows below therefore carry the zero Cost —
	// not an omission, THE load-bearing case for "zero Cost is a decision" in
	// this whole sub-project. Confirmed against the reference: len("abc") is 1
	// (rule 1 only) and len(range_expr("1-100")) is 2 (one call for
	// range_expr(), one for len() — len itself adds nothing, and in
	// particular does NOT expand the range to count it, even though
	// rangeInts() below happens to do exactly that internally; the exemption
	// is declared independently of what Fn's own implementation costs in real
	// CPU time — see Task 12 on reconciling that gap). Pinned in
	// cost_list_internal_test.go.
	"len": {
		{Params: []Type{ListOf(varT)}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return Int(int64(len(args[0].AsList()))), nil
		}},
		{Params: []Type{TString}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return Int(int64(utf8.RuneCountInString(args[0].AsStr()))), nil
		}},
		// RFC 0006: "Length of path's string representation". Codepoints, like
		// the string row above, so len(p) and len(string(p)) agree.
		{Params: []Type{TPath}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return Int(int64(utf8.RuneCountInString(pathText(args[0])))), nil
		}},
		// RFC 0006: "Number of values in range expression" — the expanded
		// count, not the text length, so len(range_expr("1-10")) is 10.
		{Params: []Type{TRangeExpr}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			ints, err := rangeInts(args[0])
			if err != nil {
				return Value{}, err
			}
			return Int(int64(len(ints))), nil
		}},
	},
	// RFC 0006's bool(). The scalar rows are conversions; the path and list
	// rows exist ONLY to carry the wording RFC 0006 demands, which is why they
	// return noreturn. Section 1.2.3 does not define a string -> bool coercion
	// at all (scalarCoercible has no CodeBool arm), so the accepted spellings
	// are this function's own behavior and not the coercion matrix's.
	//
	// None of these rows carry a Cost. Neither rule 2 nor rule 3 names bool()
	// anywhere, and the string row's own work is not "roughly proportional to
	// the string length" in rule 3's sense — boolFromString tests membership
	// in a closed set of eight short literal spellings, not string-length-
	// scaled processing. Probed: bool("true"), bool(True) and bool(1) all
	// measure 1 against the reference (rule 1 only); the path and list rows
	// always error before any processing happens.
	"bool": {
		{Params: []Type{TBool}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return args[0], nil
		}},
		{Params: []Type{TNull}, Ret: TBool, Fn: func([]Value) (Value, error) {
			return Bool(false), nil
		}},
		{Params: []Type{TInt}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(args[0].AsInt() != 0), nil
		}},
		{Params: []Type{TFloat}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return Bool(args[0].AsFloat() != 0), nil
		}},
		{Params: []Type{TString}, Ret: TBool, Fn: func(args []Value) (Value, error) {
			return boolFromString(args[0].AsStr())
		}},
		{Params: []Type{TPath}, Ret: TNoReturn, Fn: func([]Value) (Value, error) {
			//nolint:staticcheck // ST1005: capitalized verbatim per RFC 0006's own wording, pinned by TestBool_RejectsWithTheSpecifiedWording
			return Value{}, errors.New("Cannot convert path to bool")
		}},
		{Params: []Type{ListOf(varT)}, Ret: TNoReturn, Fn: func([]Value) (Value, error) {
			//nolint:staticcheck // ST1005: capitalized verbatim per RFC 0006's own wording, pinned by TestBool_RejectsWithTheSpecifiedWording
			return Value{}, errors.New("Cannot convert list to bool")
		}},
	},
	// RFC 0006 writes int and float with UNION parameters, and that is
	// load-bearing rather than cosmetic. matchShapesExactFirst refuses a
	// conversion that can fail when SELECTING an overload — the rule that stops
	// "1 + 2.5" matching (int, int) by discarding the .5 — so a narrow
	// int(value: int) row would report int(3.75) as "no signature accepts
	// (float)" instead of the non-destructive-conversion error RFC 0006 wants.
	// A union parameter is matched member-wise at no cost, passes the value
	// through unconverted (coerce's directUnionMember), and leaves the real
	// conversion to Fn, where its failure is the diagnostic.
	//
	// int() and float() carry no Cost. Neither is named by rule 2 (there is no
	// list here) or rule 3, and — unlike the string-processing functions rule
	// 3 DOES name — a numeric parse is not charged by the reference in
	// proportion to input length: isolating float()'s own contribution from
	// float("1"+"2"*300) (a 301-byte string built by two already-charged
	// operators) leaves exactly 1 operation for the conversion itself,
	// matching float("1.5")'s 1. int() shares the same coerce() path and is
	// assumed to match by the same reasoning (a 250+-digit string cannot stay
	// a valid int64 to test directly; int("0"*250+"5") does, and isolates to
	// the same flat 1).
	"int": {
		{Params: []Type{UnionOf(TInt, TFloat, TString)}, Ret: TInt, Fn: func(args []Value) (Value, error) {
			return coerce(args[0], TInt)
		}},
	},
	"float": {
		{Params: []Type{UnionOf(TInt, TFloat, TString)}, Ret: TFloat, Fn: func(args []Value) (Value, error) {
			v, err := coerce(args[0], TFloat)
			if err != nil {
				return Value{}, err
			}
			// Back through floatValue even though coerce produced the number:
			// section 1.3.4 forbids infinity and NaN, and float("inf") reaches
			// this row as a perfectly ordinary string conversion. Letting
			// coerce's result out directly would admit the one value the
			// language says cannot exist.
			return floatValue(v.AsFloat())
		}},
	},
	// Section 2.2.1's string(). The scalar row's union is RFC 0006's own
	// signature; Value.String() already implements every member of it —
	// "null" for nulltype, the JSON spellings for bool, formatFloat for float,
	// the payload text for string, path and range_expr.
	"string": {
		// No Cost: rule 3's own text scopes to "the string or path value"
		// PROCESSED, and Value.String() on a scalar does no work proportional
		// to its length beyond formatting a fixed-size number or copying a
		// string's bytes once. Discriminating probe: string("a"*1000) isolates
		// to exactly 1 operation once the 1000-byte literal's own repetition
		// charge (already declared, Task 5) is subtracted — the same flat 1 as
		// string("a"). string(path(...)) and string(range_expr(...)) isolate
		// the same way.
		{
			Params: []Type{UnionOf(TBool, TInt, TFloat, TString, TPath, TRangeExpr, TNull)},
			Ret:    TString,
			Fn: func(args []Value) (Value, error) {
				s := args[0].String()
				if err := checkStringBytes(len(s)); err != nil {
					return Value{}, err
				}
				return String(s), nil
			},
		},
		// RFC 0006 calls this "the JSON string representation", and it is NOT
		// Value.String(): that renders a list's string elements unquoted
		// ("[a, b]") as a diagnostic form, which is a known divergence deferred
		// to sub-project E. This one quotes them ("[\"a\", \"b\"]"), matching
		// the reference implementation. Two renderings, two functions, on
		// purpose.
		//
		// Cost{ArgElements: {0}} — a DELIBERATE divergence from the reference,
		// which measures a flat 1 for string([1,2,3]) AND for a 10-element
		// list, never scaling. Rule 2's operative sentence is "When a function
		// or the evaluator iterates through every element of a list, the
		// number of elements is added" — the named function list after it is
		// introduced with "such as", not "only", and writeJSONValue provably
		// walks every element of args[0] to build the JSON text. The general
		// rule's text is satisfied regardless of the enumeration, so per
		// doc.go's standing ruling that the specification outranks the
		// reference's own (Beta, known-defective) counting, this row charges.
		// See TestOperationCount_StringOverListDivergesFromReference.
		{Params: []Type{ListOf(varT)}, Ret: TString, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			s, err := jsonList(args[0])
			if err != nil {
				return Value{}, err
			}
			return String(s), nil
		}},
	},
	// list() over a range_expr materializes every value the range denotes —
	// exactly the "built-in functions that iterate lists" rule 2's own general
	// sentence describes (range() itself is named; list() is the same shape of
	// operation over the same source type). Cost{ResultElements: true}: the
	// charge scales with the PRODUCED list, not the range_expr's compact text
	// representation. Confirmed: list(range_expr("1-100")) measures 102 on the
	// reference — range_expr("1-100") alone is 1, so list()'s own share is
	// 101 = 1 (call) + 100 (the 100 produced elements).
	"list": {
		{Params: []Type{TRangeExpr}, Ret: ListOf(TInt), Cost: Cost{ResultElements: true}, Fn: func(args []Value) (Value, error) {
			vals, err := rangeExprValues(args[0])
			if err != nil {
				return Value{}, err
			}
			return List(TInt, vals), nil
		}},
	},
	"range_expr": {
		// No Cost: parsing compact range text into ranges is not proportional
		// to the EXPANDED count, matching len(range_expr(...))'s exemption
		// above. Confirmed: range_expr("1-100") measures 1 on the reference
		// regardless of the range spanning 100 values.
		{Params: []Type{TString}, Ret: TRangeExpr, Fn: func(args []Value) (Value, error) {
			return RangeExpr(args[0].AsStr())
		}},
		// Cost{ArgElements: {0}}, unlike the string row just above: this row
		// must scan every element of the INPUT list to sort, de-duplicate and
		// find its ranges, so the work genuinely is proportional to the
		// argument's length — a different question from the string row's
		// (already-compact range text). Confirmed scaling against the
		// reference: range_expr([1,2,3]) measures 4 (1 call + 3 elements) and
		// the 10-element form measures 11 (1 call + 10) — not in the brief's
		// own "rows that must charge" list, found by probing this row anyway
		// per the task's instruction to probe every row rather than trust the
		// brief's starting list.
		{Params: []Type{ListOf(TInt)}, Ret: TRangeExpr, Cost: Cost{ArgElements: []int{0}}, Fn: func(args []Value) (Value, error) {
			elems := args[0].AsList()
			if len(elems) == 0 {
				return Value{}, errors.New("range_expr() requires at least one value")
			}
			ints := make([]int64, len(elems))
			for i, e := range elems {
				ints[i] = e.AsInt()
			}
			// canonicalRange assumes its input is already sorted and
			// de-duplicated — B2 always fed it that way from an expanded
			// range, and this is the first caller that cannot assume it of
			// its own input.
			slices.Sort(ints)
			ints = slices.Compact(ints)
			return RangeExpr(canonicalRange(ints))
		}},
	},
	// fail() needs no special handling anywhere else, and that is worth
	// knowing rather than rediscovering. With a resolved argument it errors
	// here; with an unresolved one callFunction returns a placeholder before Fn
	// runs, because nothing has actually failed while the value is unknown. Its
	// noreturn return type then collapses out of any union it lands in
	// (collectUnionMembers, type.go), which is what makes
	// "x if c else fail(...)" typed x rather than x?.
	//
	// No Cost: fail() takes a message string but does no work proportional to
	// its length (it wraps it verbatim in an error), and neither rule 2 nor
	// rule 3 names it. Every call to it also fails the whole evaluation, so
	// its own charge is unobservable in the operation count regardless.
	"fail": {
		{Params: []Type{TString}, Ret: TNoReturn, Fn: func(args []Value) (Value, error) {
			return Value{}, errors.New(args[0].AsStr())
		}},
	},
}

// pathText returns a path value's text.
//
// AsStr cannot be used: it panics on anything but CodeString, and a path
// carries its text in the same payload field under a different type code. The
// direct read follows compareValues' precedent in ops.go.
func pathText(v Value) string {
	v.mustBe(CodePath)
	return v.s
}

// boolFromString applies RFC 0006's string-to-bool table, which is
// case-insensitive and closed: anything not listed is an error rather than a
// truthiness judgement. The language has no truthiness — section 1.3.5 requires
// a conditional's condition to be a bool outright — so guessing here would be
// the only place it crept in.
func boolFromString(s string) (Value, error) {
	switch strings.ToLower(s) {
	case "1", "true", "on", "yes":
		return Bool(true), nil
	case "0", "false", "off", "no":
		return Bool(false), nil
	}
	return Value{}, fmt.Errorf("cannot convert %q to bool", s)
}

// jsonList renders a list as RFC 0006's JSON representation for string().
//
// Strings, paths and range expressions are quoted and escaped; numbers,
// booleans and null are bare; nested lists recurse. The separator is ", "
// rather than JSON's bare comma, matching the reference implementation, which
// the differential test compares against character for character.
func jsonList(v Value) (string, error) {
	var b strings.Builder
	if err := writeJSONValue(&b, v); err != nil {
		return "", err
	}
	if err := checkStringBytes(b.Len()); err != nil {
		return "", err
	}
	return b.String(), nil
}

// writeJSONValue is jsonList's recursive worker.
func writeJSONValue(b *strings.Builder, v Value) error {
	switch v.Type.Code {
	case CodeList:
		b.WriteByte('[')
		for i, elem := range v.AsList() {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writeJSONValue(b, elem); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case CodeString, CodePath, CodeRangeExpr:
		// json.Marshal would escape "<", ">" and "&" as < and friends —
		// its default is chosen to be safe inside an HTML document, which is
		// not this context. Neither the reference implementation nor Python's
		// json.dumps does that, and RFC 0006 asks for "the JSON string
		// representation" without qualification, so escaping turns a faithful
		// rendering into a surprising one. An Encoder with SetEscapeHTML(false)
		// is the supported way to turn it off; it appends a newline, which is
		// trimmed.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v.s); err != nil {
			return err
		}
		b.Write(bytes.TrimRight(buf.Bytes(), "\n"))
	default:
		// bool, int, float and null all render bare, and Value.String()
		// already spells each of them the way JSON does.
		b.WriteString(v.String())
	}
	if err := checkStringBytes(b.Len()); err != nil {
		return err
	}
	return nil
}
