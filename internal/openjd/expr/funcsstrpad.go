// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// strPadFuncs is the fourth string-function group: RFC 0006 section 2.2.4's
// padding functions.
//
// Separated from the split group because these are bounded by a byte count and
// those by an element count — see funcsstrsplit.go's header.
//
// Section 1.3.10 rule 3: all six rows declare Cost{ResultBytes: true} — the
// produced, padded string's length, not ArgBytes on the input: with a 10-byte
// input the count is 2/3/4 at widths 20/300/600
// (TestOperationCount_PaddingFunctionsChargeProducedBytes). This matches the
// same ResultBytes-over-ArgBytes idiom join uses above (funcsstrsplit.go) and
// the "+"/"*" operators use (ops.go): the work here is proportional to what the
// function builds (a string sized by the requested width), not to what it was
// handed.
//
// zfillString shares the padWidth mechanism ljust/rjust/center use below and
// does the same proportional work, so all three zfill rows declare the same
// Cost{ResultBytes: true} as their siblings. The reference charges two length
// terms for every padding function, the argument's and the added padding's,
// where rule 3 names one value; test/oracle/baseline-ops.txt's padding section
// adjudicates it. See TestOperationCount_ZfillDivergesFromReferenceOnWidth.
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
	// — so zfill and string() can never disagree about a float.
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
// width is a codepoint count, matching len(s) and s[i]. A width at or below the
// current length yields no padding at all, which is what makes a negative width
// a harmless no-op rather than an error: RFC 0006 declares no error condition
// for it, so inventing a rejection would be adding a rule the specification does
// not have. (The reference implementation instead panics with "capacity
// overflow" for zfill and reports a nonsense operation count for center.)
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
	// Reserve rule 3's charge on the padded length before any of it is
	// allocated. The rows' Cost{ResultBytes: true} is levied by chargeResult
	// once the padded string exists, which would report 39,064 operations
	// against a 10,000 limit only after building 10 MB ("".ljust(10000000)).
	// The pad is a rune count and the fill is one byte per rune, so
	// len(s)+n is the produced byte length exactly. See meter.reserve.
	if err := ec.m.reserveBytes(int64(len(s)) + n); err != nil {
		return 0, false, err
	}
	return int(n), true, nil
}

// padString implements ljust, rjust and center over a space fill.
//
// center follows CPython: the padding is split by floor division and then
// biased by "marg & width & 1", so center("ab", 7) is "   ab  " — three spaces
// left, two right — and not the plain floor split's "  ab   ". The bias only
// fires when the padding and the width are both odd.
//
// RFC 0006 says only "Center, pad with spaces to width" and does not settle the
// tie, so with the spec silent the tie-break follows the library the function
// is modeled on, which is also what the reference does.
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
		// CPython's str.center bias: floor split, plus one on the left when the
		// padding and the width are both odd.
		left := n/2 + int(int64(n)&width&1)
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
