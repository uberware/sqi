// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// strPadFuncs is sub-project C2's fourth and last group: RFC 0006 section
// 2.2.4's padding functions.
//
// Separated from the split group because these are bounded by a BYTE count and
// those by an ELEMENT count — see funcsstrsplit.go's header.
//
// Section 1.3.10 rule 3 (sub-project E1, Task 7): all six rows declare
// Cost{ResultBytes: true} — the PRODUCED, padded string's length, not
// ArgBytes on the input. Discriminated by holding the input FIXED at 10
// bytes and growing only the requested WIDTH: an ArgBytes(input) reading
// would stay flat at every width; it does not — 2/3/4 at widths 20/300/600
// (TestOperationCount_PaddingFunctionsChargeProducedBytes). This matches the
// same ResultBytes-over-ArgBytes idiom join uses above (funcsstrsplit.go) and
// the "+"/"*" operators use (Task 5, ops.go): the work here is proportional
// to what the function BUILDS (a string sized by the requested width), not
// to what it was handed.
//
// zfill is the ONE row in this file where the reference disagrees with its
// own three siblings: probing shows its measured count does NOT scale with
// width at all — '5'.zfill(10), .zfill(300), .zfill(600) and .zfill(1000)
// all measure 2, tracking only the 1-byte INPUT ('5')'s own ArgBytes charge,
// never the padded output. zfillString shares the EXACT SAME padWidth
// mechanism ljust/rjust/center use below, and does the exact same
// proportional work, so this is a reference omission rather than a
// meaningful semantic difference — the same family of defect the brief flags
// for negative widths ("center('ab', -3) failing with a bogus 72-quadrillion
// operation count, and zfill with a negative width panics outright") but on
// the positive-width path instead. Per the standing rule ("the specification
// outranks the reference"), sqi does not reproduce the omission: all three
// zfill rows declare the SAME Cost{ResultBytes: true} as their siblings,
// rather than making zfill the one padding function whose declared cost does
// not reflect the padding it performs. See
// TestOperationCount_ZfillDivergesFromReferenceOnWidth and
// cost_string_internal_test.go's PROBE comment for the full transcript.
//
// A negative width never reaches this growth path at all — padWidth
// (below) already treats width <= the current length as a no-op, so there is
// nothing extra for either ljust/rjust/center or zfill to charge beyond
// ResultBytes on the (unchanged) input, and no divergence to adjudicate for
// that case specifically.
var strPadFuncs = map[string][]Shape{
	"ljust": {
		{Params: []Type{TString, TInt}, Ret: TString, Cost: Cost{ResultBytes: true}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return padString(ec, args[0].AsStr(), args[1].AsInt(), padRight)
		}},
	},
	"rjust": {
		{Params: []Type{TString, TInt}, Ret: TString, Cost: Cost{ResultBytes: true}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return padString(ec, args[0].AsStr(), args[1].AsInt(), padLeft)
		}},
	},
	"center": {
		{Params: []Type{TString, TInt}, Ret: TString, Cost: Cost{ResultBytes: true}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return padString(ec, args[0].AsStr(), args[1].AsInt(), padCenter)
		}},
	},
	// RFC 0006 gives zfill three rows, one per first-argument type, because a
	// number has to be rendered before it can be padded. The int and float rows
	// render through the value's own String(), which is formatFloat for a float
	// — deliberately, so zfill and string() can never disagree about a float.
	//
	// The float row is also why "(42).zfill(5)" needs its parentheses: RFC 0006
	// notes the Python grammar reads "42." as the start of a float literal.
	"zfill": {
		{Params: []Type{TString, TInt}, Ret: TString, Cost: Cost{ResultBytes: true}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return zfillString(ec, args[0].AsStr(), args[1].AsInt())
		}},
		{Params: []Type{TInt, TInt}, Ret: TString, Cost: Cost{ResultBytes: true}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return zfillString(ec, strconv.FormatInt(args[0].AsInt(), 10), args[1].AsInt())
		}},
		{Params: []Type{TFloat, TInt}, Ret: TString, Cost: Cost{ResultBytes: true}, FnCtx: func(ec evalCtx, args []Value) (Value, error) {
			return zfillString(ec, args[0].String(), args[1].AsInt())
		}},
	},
}

// padSide selects where padString puts the fill.
type padSide int

const (
	padRight padSide = iota
	padLeft
	padCenter
)

// padWidth reports how many pad runes take s up to width, and reports false
// when none are needed.
//
// width is a CODEPOINT count, matching len(s) and s[i]. A width at or below the
// current length yields no padding at all, which is what makes a NEGATIVE width
// a harmless no-op rather than an error: RFC 0006 declares no error condition
// for it, so inventing a rejection would be adding a rule the specification does
// not have. (The reference implementation instead panics with "capacity
// overflow" for zfill and reports a nonsense operation count for center; that
// will be recorded in test/oracle/baseline.txt when the oracle corpus lands.)
//
// The bound goes through checkRepeat, not through len(s)+n compared afterward:
// width is an arbitrary int64 straight from the expression, so at MaxInt64 the
// naive sum wraps negative and would sail past any comparison on it. checkRepeat
// never forms the product — its own doc comment carries the argument.
func padWidth(ec evalCtx, s string, width int64) (padLen int, need bool, err error) {
	cur := int64(utf8.RuneCountInString(s))
	if width <= cur {
		return 0, false, nil
	}
	n := width - cur
	if _, err := checkRepeat(1, n, maxStringBytes-len(s)); err != nil {
		return 0, false, err
	}
	// RESERVE rule 3's charge on the PADDED length before any of it is
	// allocated. The rows' Cost{ResultBytes: true} is levied by chargeResult
	// once the padded string exists, which reported 39,064 operations against
	// a 10,000 limit only after building 10 MB ("".ljust(10000000), measured).
	// The pad is a rune count and the fill is one byte per rune, so
	// len(s)+n is the produced byte length exactly. See meter.reserve.
	if err := ec.m.reserveBytes(int64(len(s)) + n); err != nil {
		return 0, false, err
	}
	return int(n), true, nil
}

// padString implements ljust, rjust and center over a space fill.
//
// center splits by FLOOR division and leaves the remainder on the right, so
// center("ab", 7) is "  ab   ". CPython disagrees — it biases the split with
// "marg & width & 1" and answers "   ab  " — and the reference implementation
// does not. We follow the reference, which is also the simpler rule; matching
// CPython here would create an oracle divergence for no reason.
func padString(ec evalCtx, s string, width int64, side padSide) (Value, error) {
	n, need, err := padWidth(ec, s, width)
	if err != nil {
		return Value{}, err
	}
	if !need {
		return String(s), nil
	}
	switch side {
	case padLeft:
		return String(strings.Repeat(" ", n) + s), nil
	case padCenter:
		left := n / 2
		return String(strings.Repeat(" ", left) + s + strings.Repeat(" ", n-left)), nil
	default:
		return String(s + strings.Repeat(" ", n)), nil
	}
}

// zfillString pads with leading zeros, keeping a leading sign in front of them.
//
// RFC 0006 states the sign rule for all three rows: zfill("-10", 4) is "-010"
// and zfill(-1, 3) is "-01", not "0-10" and "0-1". The sign is counted toward
// the width, so zfill("-", 4) is "-000" — three zeros, four characters.
func zfillString(ec evalCtx, s string, width int64) (Value, error) {
	n, need, err := padWidth(ec, s, width)
	if err != nil {
		return Value{}, err
	}
	if !need {
		return String(s), nil
	}
	sign, body := "", s
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		sign, body = s[:1], s[1:]
	}
	return String(sign + strings.Repeat("0", n) + body), nil
}
