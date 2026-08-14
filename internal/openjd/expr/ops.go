// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

var (
	errIntOverflow  = errors.New("integer overflow")
	errDivideByZero = errors.New("division by zero")
	errModuloByZero = errors.New("modulo by zero")
	errZeroNegPower = errors.New("zero cannot be raised to a negative power")
	errInfinite     = errors.New("the result is infinite")
	errNotANumber   = errors.New("the result is not a number")
	errNegFracPower = errors.New("a negative number cannot be raised to a fractional power")
)

// Type-variable types, used only in signatures. A variable binds at match time
// and is substituted into the declared return.
var (
	varT  = Type{Code: CodeVarT}
	varT1 = Type{Code: CodeVarT1}
)

// binaryFunc is the shape of an operator implementation a Shape wraps. The
// operands are guaranteed to have been coerced to the Shape's declared
// parameter types before an implementation runs, so it may use the payload
// accessors without checking.
type binaryFunc func(l, r Value) (Value, error)

// identity is OpPos's implementation: unary "+" changes nothing.
func identity(v Value) (Value, error) { return v, nil }

// shapeBinary adapts a two-operand implementation to a Shape's argument slice.
func shapeBinary(f func(l, r Value) (Value, error)) func(args []Value) (Value, error) {
	return func(args []Value) (Value, error) { return f(args[0], args[1]) }
}

// shapeBinaryCtx is shapeBinary for an implementation that needs the evaluation
// context -- specifically, one that must RESERVE its result's section 1.3.10
// cost against ec.m before allocating it (meter.reserve). It adapts to FnCtx
// rather than Fn, so a row using it sets FnCtx, not Fn.
func shapeBinaryCtx(f func(ec evalCtx, l, r Value) (Value, error)) func(ec evalCtx, args []Value) (Value, error) {
	return func(ec evalCtx, args []Value) (Value, error) { return f(ec, args[0], args[1]) }
}

// shapeUnary adapts a one-operand implementation to a Shape's argument slice.
func shapeUnary(f func(v Value) (Value, error)) func(args []Value) (Value, error) {
	return func(args []Value) (Value, error) { return f(args[0]) }
}

// binaryShapes lists every accepted signature of each binary operator, in the
// spec's own terms (sections 2.1.1, 2.1.2, 2.1.4).
//
// A shape declares what it returns as well as what it takes, which is what lets
// an operand with no value still yield a typed result. Ordering within a list
// mostly does not affect which shape wins: matchShapes (shape.go) ranks every
// candidate by cost — an exact match cheaper than a coercing one — and the
// lowest total wins regardless of list position. The one place position still
// matters is an exact tie, which keeps the earliest shape; no operator table
// here relies on that, since no operator lists two shapes of equal cost for
// the same argument types. The FUNCTION registry does rely on it: flatten's
// nested and flat rows tie at cost 0 for a list of lists, and the earliest-wins
// rule is what keeps flatten from being the identity. See funcslist.go.
//
// Sub-projects B2 and C add shapes here and to their own function registry; a
// list with no matching shape is reported as "unsupported operand types", which
// is the same single mechanism sub-project A used.
var binaryShapes = map[Op][]Shape{
	OpAdd: {
		// Section 1.3.10 rule 2/3: scalar int/float arithmetic processes no
		// list and no string, so rule 1 (charged by callShape for every row)
		// is the whole charge -- neither row below declares a Cost.
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(addInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(floatBinary(func(a, b float64) float64 { return a + b }))},
		// Rule 3 names string concatenation by name. Cost.ResultBytes, not
		// ArgBytes on both operands: ceil(len/256) is not additive the way a
		// plain element count is, and measuring against the reference (see
		// the probe comment on TestOperationCount_Operators,
		// cost_ops_internal_test.go) shows it charges the PRODUCED string's
		// length once, not each operand's length separately. The two
		// readings agree whenever both operands are short or their split
		// doesn't cross a 256 boundary differently than their sum does; they
		// were told apart with unequal, over-256-byte operands.
		{Params: []Type{TString, TString}, Ret: TString, Cost: Cost{ResultBytes: true}, Fn: shapeBinary(concatStrings)},
		// RFC 0006 section 2.1.5's __add__(p: path, suffix: string) -> path.
		//
		// This row is what makes "P + 'x'" a PATH rather than the string it
		// used to be: it matches EXACTLY (cost 0) where the (string, string)
		// row above matches only by coercing the path operand to string (cost
		// 1, section 1.2.3's path -> string rule). The reverse pair is
		// unaffected — "'x' + P" still takes the string row, because
		// string -> path is not a conversion the matcher will make to select
		// an overload (promotable, coerce.go) — and so is "'a' + 'b'", which
		// the string row still matches at cost 0.
		//
		// Cost.ResultBytes: a path is a string value for rule 3 (RFC 0006),
		// and the same ResultBytes-over-ArgBytes resolution applies here as
		// it does to string concatenation just above — confirmed against the
		// reference by isolating this row's own contribution from path()'s
		// construction cost. See TestOperationCount_PathOperators.
		{Params: []Type{TPath, TString}, Ret: TPath, Cost: Cost{ResultBytes: true}, Fn: shapeBinary(appendToPath)},
		// Section 2.1.3: range_expr + range_expr -> list[int]. range_expr
		// promotes losslessly to BOTH list[int] and string (coercibleConditional,
		// coerce.go), and the generic list shape below cannot admit a bare
		// range_expr argument at all — its element type is an unbound type
		// variable, and range_expr's own promotion rule only recognizes a
		// concrete list[int] target, not list[T]. Without this row,
		// "Param.Range + Param.Range" ties into the (string, string) shape above
		// and wrongly concatenates as text ("1-31-3") instead of expanding to
		// list[int]. An EXACT match (cost 0, beating the string shape's cost 2)
		// rather than a tie broken by position, so its Fn is concatRanges, not
		// concatLists directly: an exact-match Shape needs no coercion at all,
		// so declaring the params as list[int] here (to get coercion's range
		// expansion for free, as the mixed row below does) would report this
		// row's own cost as 2 and recreate the very tie this row exists to
		// avoid. concatRanges performs the same expansion by hand instead.
		//
		// Cost.ArgElements charges rule 2's "list concatenation" on both
		// operands even though this row's declared params are range_expr, not
		// list[int]: elementCount (ops.go) computes a range_expr's count via
		// rangeExprCount, so the charge is correct without waiting for
		// concatRanges to run. That count is arithmetic for a single
		// sub-range and falls back to a bounded expansion for two or more --
		// see rangeExprCount's own doc comment (rangeexpr.go), which is the
		// contract rather than this restatement of it. Verified
		// against the reference by subtracting each range_expr() call's own
		// cost from the combined expression — see the probe comment on
		// TestOperationCount_Operators.
		{Params: []Type{TRangeExpr, TRangeExpr}, Ret: ListOf(TInt), Cost: Cost{ArgElements: []int{0, 1}}, Fn: shapeBinary(concatRanges)},
		// Section 2.1.3. The two element variables are independent, so that
		// "[1] + [2.0]" matches at all; concatLists then computes the real
		// common element type from both operands, since section 2.1.3's
		// "finding a common type" (list[int] + list[float] -> list[float])
		// cannot be expressed as a binding. RetOf performs the SAME unification
		// over the bound types, for the path where an operand has no value and
		// concatLists never runs. Substituting into a declared Ret cannot do
		// it: Ret: ListOf(varT) reads only the LEFT operand, which typed
		// "[] + Param.Items" as list[nulltype] while "Param.Items + []" — the
		// same concatenation — came out list[int].
		//
		// Cost.ArgElements charges rule 2's "list concatenation" by name, on
		// both operands. Unlike rule 3's ceil(len/256), a plain element count
		// is additive, so ArgElements{0,1} and a hypothetical "charge the
		// produced list's length instead" always agree here (len(a)+len(b) IS
		// len(result)) — no probe can tell them apart, so ArgElements is used
		// because it is what the rule's own wording most directly describes:
		// the evaluator iterating through each operand list.
		{
			Params: []Type{ListOf(varT), ListOf(varT1)},
			Ret:    ListOf(varT),
			RetOf:  concatRet,
			Cost:   Cost{ArgElements: []int{0, 1}},
			Fn:     shapeBinary(concatLists),
		},
		// A bare range_expr operand cannot reach the generic list[T] shape
		// above at all: coercibleConditional's range_expr -> list[int] rule
		// (coerce.go) only recognizes a CONCRETE list[int] target, not an
		// unbound type variable, so promotable(range_expr, list[T]) is false
		// and the shape is inadmissible for that argument. A concrete
		// list[int] parameter on both sides is what actually lets
		// "Param.Range + [4]" and "[0] + Param.Range" match — range_expr
		// promotes to list[int] on whichever side it appears, and a real
		// list[int] argument matches this row exactly — verified by
		// TestListOperators rather than assumed. Declaring the parameter as
		// list[int] rather than range_expr matters: callShape only coerces
		// (and so only expands the range) when the declared type differs from
		// the argument's own, so a bare range_expr parameter would pass the
		// unconverted range_expr straight to concatLists, which expects a
		// list payload.
		//
		// Cost.ArgElements, same as the generic list+list row above and for
		// the same reason: this is still "list concatenation" under rule 2,
		// just at a signature the generic row cannot admit. callShape coerces
		// a range_expr operand to list[int] BEFORE chargeArgs runs (this row
		// is not exact-match for that argument), so elementCount reads the
		// already-expanded list's length here rather than needing
		// rangeExprCount's arithmetic shortcut the way the range_expr+range_expr
		// row above does.
		{Params: []Type{ListOf(TInt), ListOf(TInt)}, Ret: ListOf(TInt), Cost: Cost{ArgElements: []int{0, 1}}, Fn: shapeBinary(concatLists)},
	},
	// Section 1.3.10 rules 2/3: scalar subtraction processes no list and no
	// string, so rule 1 is the whole charge.
	OpSub: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(subInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(floatBinary(func(a, b float64) float64 { return a - b }))},
	},
	OpMul: {
		// Scalar multiplication: rule 1 only, same reasoning as OpSub above.
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(mulInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(floatBinary(func(a, b float64) float64 { return a * b }))},
		// Section 2.1.3's __mul__(list[T], int) and section 2.1.2's
		// __mul__(string, int). String repetition was absent until the size
		// bound in limits.go existed to cap the repeat count.
		//
		// promoteNoRangeText on the string row is what makes "Param.Range * 2"
		// an ERROR rather than the string repetition "1-101-10" it used to be,
		// and it is the answer to a question the OpAdd table above raises: why
		// does "+" go out of its way to stop a range_expr pair concatenating as
		// text while "*" would not? Because the spec answers the two operators
		// differently, and both times explicitly.
		//
		// Section 1.2.3's range_expr -> string coercion is real, so a bare
		// range_expr operand CAN reach a string parameter — that is precisely
		// how "Param.Range + '!'" works. But the operator tables enumerate
		// where a range_expr operand is meant to be seen, and the enumeration
		// is exhaustive on its face: section 2.1.2 spells out
		// __add__(string, range_expr) and __add__(range_expr, string) as their
		// own rows, section 2.1.3 spells out three more __add__ rows plus
		// __contains__(range_expr, int) — and NEITHER table gives __mul__ a
		// range_expr row of any kind. Those explicit __add__ rows would be
		// redundant if the coercion fired on its own during overload selection,
		// so their existence is the evidence that it does not: a range_expr
		// reaches a string parameter where the spec says so, and nowhere else.
		// Reading it the other way makes "Param.Frames * 2" quietly produce a
		// doubled frame-range TEXT, which no template author can have meant.
		//
		// The other two readings were considered and rejected. Treating the
		// range as list[int] (matching the __add__ precedent, giving a
		// list[int] result) needs section 1.2.3's range_expr -> list[int] rule,
		// which is conditioned on "the target types include list[int]" — the
		// generic list row's element is an unbound variable, not int, so the
		// rule does not apply and the row is inadmissible for a bare
		// range_expr, exactly as the OpAdd table documents. Keeping the string
		// repetition needs the coercion to fire where no row invites it. The
		// reference implementation independently rejects this expression with
		// "Cannot use '*' operator with range_expr and int" under every target
		// type tried (any, string, list[int]) — evidence, not the reason.
		// Rule 2 names list repetition by name. ResultElements, not
		// ArgElements on the list operand: repetition's produced length is
		// len(operand)*n, which ArgElements cannot express (it would charge
		// the pre-repetition length, undercounting by a factor of n), and
		// matches the reference exactly — see TestOperationCount_Operators's
		// "[1,2] * 3" case (1 call + 6 produced elements = 7).
		//
		// FnCtx, not Fn: the ResultElements charge above is levied by
		// chargeResult AFTER the repeated list exists, which is a report and
		// not a bound (meter.reserve). repeatList therefore takes ec and
		// reserves its arithmetic total before allocating.
		{Params: []Type{ListOf(varT), TInt}, Ret: ListOf(varT), Cost: Cost{ResultElements: true}, FnCtx: shapeBinaryCtx(repeatList)},
		// Rule 3 names string repetition by name. ResultBytes for the same
		// reason ResultElements is right for list repetition just above: the
		// produced length is len(operand)*n, and the spec's own worked
		// example (ceilDiv256's doc comment) confirms ceil(PRODUCED length /
		// 256) is the formula, not a per-operand charge.
		// FnCtx for the same reason as the row above: repeatString reserves
		// ceil(produced length / 256) before strings.Repeat allocates it.
		{Params: []Type{TString, TInt}, Ret: TString, Promote: promoteNoRangeText, Cost: Cost{ResultBytes: true}, FnCtx: shapeBinaryCtx(repeatString)},
	},
	// Dividing two ints yields a float — the spec's __truediv__(int, int) -> float.
	OpDiv: {
		// Scalar division: rule 1 only, same reasoning as OpSub/OpMul above.
		{Params: []Type{TInt, TInt}, Ret: TFloat, Fn: shapeBinary(divInts)},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(divFloats)},
		// RFC 0006 section 2.1.5's two path-join rows. Both are registered
		// even though the (path, path) one would be reached from the (path,
		// string) row anyway by section 1.2.3's path -> string coercion: an
		// exact row is what the specification's table lists, it costs nothing
		// to match, and it does not depend on that coercion staying in place.
		//
		// Neither row admits an int on either side, which is what keeps
		// "path('a') / 1" and "1 / path('a')" errors: int -> string is a
		// coercion but not a promotion, so it cannot be used to SELECT an
		// overload (see matchShapesExactFirst, shape.go).
		//
		// Cost.ResultBytes on both rows: a path is a string value for rule 3
		// (RFC 0006), and joinPaths's own contribution — isolated from
		// path()'s construction cost by subtracting it from the combined
		// expression's measured total — matches ResultBytes and not
		// ArgBytes{0,1}, the same resolution as string concatenation above.
		// See TestOperationCount_PathOperators and the probe comment on
		// TestOperationCount_Operators.
		{Params: []Type{TPath, TString}, Ret: TPath, Cost: Cost{ResultBytes: true}, Fn: shapeBinary(joinPaths)},
		{Params: []Type{TPath, TPath}, Ret: TPath, Cost: Cost{ResultBytes: true}, Fn: shapeBinary(joinPaths)},
	},
	// Floor-dividing two floats yields an int — __floordiv__(float, float) -> int.
	// Rule 1 only for both rows: scalar arithmetic, same as OpSub/OpMul/OpDiv.
	OpFloorDiv: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(floorDivInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TInt, Fn: shapeBinary(floorDivFloats)},
	},
	// Rule 1 only: scalar arithmetic, same as the other scalar operators above.
	OpMod: {
		{Params: []Type{TInt, TInt}, Ret: TInt, Fn: shapeBinary(intBinary(modInt))},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(modFloats)},
	},
	// int ** int is int for a non-negative exponent and float for a negative one,
	// so its declared return is the union the spec writes: float | int.
	// Rule 1 only for both rows: scalar arithmetic, same as the operators above.
	OpPow: {
		{Params: []Type{TInt, TInt}, Ret: UnionOf(TInt, TFloat), Fn: shapeBinary(powInts)},
		{Params: []Type{TFloat, TFloat}, Ret: TFloat, Fn: shapeBinary(powFloats)},
	},

	// CORRECTION (review finding on this task): an earlier revision of this
	// comment claimed rule 2's "contains()" was a functionShapes registry
	// entry Tasks 6-8 own, and left every OpIn/OpNotIn row uncharged on that
	// basis. That premise is false. RFC 0005's own dunder-transform table
	// (lines 1445-1446) is explicit: "In -> __contains__, NotIn ->
	// __not_contains__ ... x in y becomes __contains__(y, x)". There is no
	// registry function named "contains" anywhere in the spec (confirmed:
	// `rg '"contains"' internal/openjd/expr/` finds no functionShapes entry,
	// only this comment and a test label) — "contains()" in rule 2's list
	// (and __contains__/__not_contains__ in the wiki's operator table,
	// section 2.1.2/2.1.3) IS this operator, named by its dunder rather than
	// its surface syntax the way every other operator in rule 2's list is
	// (list concatenation "+", not "__add__"). So "in"/"not in" against a
	// STRING or a LIST *is* named by rule 2/3 and must charge.
	//
	// ALL THREE rows charge, including range_expr. An earlier revision of this
	// paragraph ended "only the range_expr row is genuinely unnamed (the
	// reference agrees: its count does not scale with the range's size)",
	// which read as a justification for leaving it alone. It is not one: being
	// unnamed decides nothing once rule 2's enumeration is read as open
	// (doc.go), and containsRangeInt expands the range in full regardless of
	// what the reference charges. See the row-level CORRECTION below.
	//
	// Cost.ArgBytes{1} on the string/string rows and Cost.ArgElements{1} on
	// the list rows, charging the CONTAINER (index 1, per the item-is-
	// Params[0] note below) rather than the searched-for item: rule 2/3 charge
	// scales with what is iterated or scanned, which is the container, not
	// the (constant-size) needle. Confirmed against the reference with
	// container sizes that discriminate a per-container charge from no
	// charge and from a per-item charge — see the probe comment on
	// TestOperationCount_InOperator.
	//
	// Note the operand order: the AST puts the searched-for value on the LEFT
	// ("item in list"), matching containsString's own (needle, haystack)
	// convention, so the item is each shape's first parameter.
	//
	// The item and the list's element use TWO distinct variables (varT,
	// varT1), not the same one twice: a shared variable requires both
	// occurrences to bind to the EXACTLY equal type (shape.go's argCost), which
	// would reject "2.0 in [1, 2, 3]" outright since it never reaches
	// containsElem's cross-type valuesEqual at all. Verified by
	// TestListOperators's "2.0 in [1, 2, 3]" case rather than assumed.
	//
	// The substring rows carry promoteNoRangeText for the same reason OpMul's
	// string row does, and it is the same defect one operator over: without it
	// a range_expr operand coerced to its own text and "'1' in Param.Range"
	// answered a SUBSTRING question about "1-10" rather than the membership
	// question section 2.1.3 defines, whose only range_expr row is
	// __contains__(range_expr, int) — the row directly below. Neither section
	// 2.1.2 nor 2.1.3 gives "in" a string/range_expr row, and the reference
	// implementation likewise rejects "'1' in range_expr('1-10')".
	OpIn: {
		{Params: []Type{TString, TString}, Ret: TBool, Promote: promoteNoRangeText, Cost: Cost{ArgBytes: []int{1}}, Fn: shapeBinary(containsString)},
		{Params: []Type{varT, ListOf(varT1)}, Ret: TBool, Cost: Cost{ArgElements: []int{1}}, Fn: shapeBinary(containsElem)},
		// CORRECTION (final whole-branch review, sub-project E1). An earlier
		// revision left this row and its OpNotIn twin UNCHARGED, arguing:
		// "Uncharged on purpose: the reference's own count does not scale with
		// the range's size (see the probe comment on
		// TestOperationCount_InOperator), so nothing here supports a per-range
		// charge the way the string and list rows above have one." That
		// observation about the reference is accurate and still holds; the
		// CONCLUSION drawn from it was wrong twice over. First, the reference's
		// behavior is subordinate to the specification by this package's
		// standing rule, and it was the SOLE stated reason here. Second, the
		// specification's own test is what this row actually fails:
		// containsRangeInt (below) calls rangeInts, which FULLY EXPANDS the
		// range, so "a function ... iterates through every element of a list"
		// is satisfied outright. Measured before the fix, "1 in Param.R" with
		// R = range_expr("1-1000000") charged 1 operation while expanding a
		// million integers. Now Cost{ArgElements: {1}} — index 1, the
		// container, matching the string and list rows above — which prices
		// the expansion arithmetically (elementCount routes a range_expr
		// through rangeExprCount) and charges it BEFORE the expansion runs.
		// The residual flat +1 the reference adds here is unchanged and stays
		// baselined; only the SCALING component is at issue.
		{Params: []Type{TInt, TRangeExpr}, Ret: TBool, Cost: Cost{ArgElements: []int{1}}, Fn: shapeBinary(containsRangeInt)},
	},
	OpNotIn: {
		{Params: []Type{TString, TString}, Ret: TBool, Promote: promoteNoRangeText, Cost: Cost{ArgBytes: []int{1}}, Fn: shapeBinary(notContainsString)},
		{Params: []Type{varT, ListOf(varT1)}, Ret: TBool, Cost: Cost{ArgElements: []int{1}}, Fn: shapeBinary(negate(containsElem))},
		{Params: []Type{TInt, TRangeExpr}, Ret: TBool, Cost: Cost{ArgElements: []int{1}}, Fn: shapeBinary(negate(containsRangeInt))},
	},

	OpLt: orderingShapes(OpLt),
	OpGt: orderingShapes(OpGt),
	OpLe: orderingShapes(OpLe),
	OpGe: orderingShapes(OpGe),
}

// orderingShapes builds the four same-type ordering signatures for one
// comparison operator. Section 2.1.4 also permits int/float and string/path
// cross-pairs; both reach one of these same-type shapes by promotion, but not
// the ordinary promotion argCost (shape.go) gives every other operator —
// each shape here carries Promote: promoteOrdering, which admits only those
// two named pairs (and their elementwise form inside a list) rather than
// every lossless conversion coerce.go allows. Without that restriction,
// section 1.2.3's range_expr -> string coercion would leak into the
// (string, string) shape below and wrongly let a range_expr order against a
// string.
//
// The four SCALAR rows below declare no Cost, and correctly so: they compare
// two scalars, iterating no list and processing no string, so section
// 1.3.10's rule 1 is the whole charge. (The reference measures 2 for every
// ordering comparison, scalar or list, where sqi measures 1; that flat +1 is
// an unexplained reference quirk, baselined in test/oracle/baseline-ops.txt,
// and is a separate question from the scaling one below.)
//
// CORRECTION (final whole-branch review, sub-project E1). The LIST/LIST row
// was in that same uncharged set, on this reasoning: "Section 1.3.10's rule 2
// and rule 3 each close with an explicit 'This applies to: ...' enumeration,
// and ordering operators appear in neither one — not even the list/list row,
// which iterates its operands' elements (compareLists) but is not the
// 'list/range equality comparisons' rule 2 names, a distinct operation with
// its own row." The citation was to rfcs/0005-expression-language.md; the
// SPECIFICATION (wiki/2026-02-Expression-Language.md) introduces the same
// enumeration with "such as", and doc.go now settles that disagreement in
// favor of the open reading for the whole package. Under it, a token-level
// distinction between "==" and "<" does not survive: applyBinary's equality
// path and this row run the SAME walk over the same operands (valuesEqual
// and compareLists both compare elementwise), and only one was charged.
// Measured before the fix, a 20,000-int list compared 2,000 times cost 6,002
// operations under "<" while the identical "==" expression tripped the
// 10,000,000 limit — a 1,666x discrepancy for identical work. The row now
// declares Cost{ArgElements: {0}}, charging the LEFT operand's element count,
// exactly as applyBinary's equality charge does, so the two agree by
// construction rather than by coincidence.
func orderingShapes(op Op) []Shape {
	return []Shape{
		{Params: []Type{TInt, TInt}, Ret: TBool, Promote: promoteOrdering, Fn: shapeBinary(ordering(op, compareInts))},
		{Params: []Type{TFloat, TFloat}, Ret: TBool, Promote: promoteOrdering, Fn: shapeBinary(ordering(op, compareFloats))},
		{Params: []Type{TString, TString}, Ret: TBool, Promote: promoteOrdering, Fn: shapeBinary(ordering(op, compareStrings))},
		{Params: []Type{TBool, TBool}, Ret: TBool, Promote: promoteOrdering, Fn: shapeBinary(ordering(op, compareBools))},
		// Section 1.2.5's lexicographic list ordering. The shared varT is
		// UNIFIED across both operand lists rather than required exactly
		// equal, admitting section 2.1.4's compatible pairs elementwise: "[1]
		// < [1.0]" binds varT to float, coerces both operands to
		// list[float], and reaches compareLists, answering false. A
		// genuinely incompatible pair like ['a'] < [1] has no unification to
		// reach and is still rejected at shape matching.
		{Params: []Type{ListOf(varT), ListOf(varT)}, Ret: TBool, Promote: promoteOrdering, Cost: Cost{ArgElements: []int{0}}, Fn: shapeBinary(ordering(op, compareLists))},
	}
}

// unaryShapes lists every accepted signature of each prefix operator.
//
// No row in this table declares a Cost, and every row's reason is the same
// one: each operand is a scalar, so no list is iterated and no string is
// processed, leaving rule 1 as the whole charge (the same reason the scalar
// rows of binaryShapes carry no Cost). That holds for numeric negation,
// unary plus and boolean "not" alike, despite the reference measuring 2 for
// "not True" rather than 1.
//
// CORRECTION (final whole-branch review, sub-project E1): an earlier
// revision of this comment argued "not" additionally "is not named by rule 2
// or rule 3's closed 'This applies to: ...' enumerations". That half of the
// argument is withdrawn — doc.go settles the enumerations as OPEN, so being
// unnamed by them proves nothing either way. The rows are still correctly
// uncharged; only the reasoning above stands. and/or are
// a separate case entirely and are not in this table at all: RFC 0005 line
// 1454 says they are handled directly by the evaluator rather than
// transformed to function calls, so evalLogical never reaches applyUnary,
// callShape, or any Cost here (see doc.go).
var unaryShapes = map[Op][]Shape{
	OpNeg: {
		{Params: []Type{TInt}, Ret: TInt, Fn: shapeUnary(negInt)},
		{Params: []Type{TFloat}, Ret: TFloat, Fn: shapeUnary(negFloat)},
	},
	OpPos: {
		{Params: []Type{TInt}, Ret: TInt, Fn: shapeUnary(identity)},
		{Params: []Type{TFloat}, Ret: TFloat, Fn: shapeUnary(identity)},
	},
	OpNot: {
		{Params: []Type{TBool}, Ret: TBool, Fn: shapeUnary(notBool)},
	},
}

// applyBinary dispatches a binary operator, or reports that no signature accepts
// the operand types. Errors carry no position; the evaluator wraps them with the
// offset of the operator that failed.
//
// ec is threaded through to callShape and is NOT optional. An earlier revision
// passed callShape(evalCtx{}, ...) here, with a comment arguing that a
// zero-value context was safe "because no row of binaryShapes ever sets FnCtx".
// CORRECTION (sub-project E1): that reasoning held only while callShape read
// nothing but FnCtx from the context. callShape now charges section 1.3.10
// operations against ec.m, and a zero-value evalCtx carries a nil meter, so a
// fresh context here would silently un-count every operator in the language.
func applyBinary(ec evalCtx, op Op, l, r Value) (Value, error) {
	switch op {
	case OpEq, OpNe:
		// Equality is total across types (section 1.2.5), so it is never
		// unsupported — but with a placeholder it has no concrete answer
		// either. A bare false would be wrong: the values may be equal at
		// runtime.
		if l.IsUnresolved() || r.IsUnresolved() {
			// The call was dispatched even though nothing ran, so rule 1
			// applies. Rules 2 and 3 do NOT: no list was iterated and no
			// string was processed. See doc.go for the ruling.
			if err := ec.m.charge(1); err != nil {
				return Value{}, err
			}
			return Unresolved(TBool), nil
		}
		// This path returns before matchShapes, so it never reaches callShape.
		// Rule 1 still applies: an equality comparison is a call.
		if err := ec.m.charge(1); err != nil {
			return Value{}, err
		}
		// Section 1.3.10 rule 2 names "list/range equality comparisons"
		// explicitly. elementCount is 0 for any non-collection operand, so
		// this is a no-op charge for every other type pair; it only fires
		// when the LEFT operand is a list or range_expr. Charged against the
		// left operand only -- matching the brief's adjudicated ruling
		// (cost_eval_internal_test.go's TestOperationCount_ListEquality),
		// which sets 1 (call) + 2 (elements) = 3 for a two-element
		// comparison against the reference's unexplained 2.
		if n, err := elementCountBounded(ec, l); err != nil {
			return Value{}, err
		} else if err := ec.m.chargeElements(n); err != nil {
			return Value{}, err
		}
		// The RIGHT operand is RESERVED, never charged -- see
		// reserveEqualityExpansion (rangeexpr.go). The left-only CHARGE above
		// is the adjudicated ruling and is unchanged by this; what this adds
		// is a refusal in front of the expansion listOrRangeEqual performs on
		// whichever side is a range_expr, which for a right-hand one had
		// nothing in front of it at all.
		if err := reserveEqualityExpansion(ec, l, r); err != nil {
			return Value{}, err
		}
		if op == OpEq {
			return Bool(valuesEqual(l, r)), nil
		}
		return Bool(!valuesEqual(l, r)), nil
	}

	s, b, ok := matchShapes(binaryShapes[op], []Type{l.Type, r.Type})
	if !ok {
		return Value{}, fmt.Errorf("unsupported operand types for %s: %s and %s", op, l.Type, r.Type)
	}
	if l.IsUnresolved() || r.IsUnresolved() {
		// The call was dispatched even though nothing ran, so rule 1 applies.
		// Rules 2 and 3 do NOT: no list was iterated and no string was
		// processed. See doc.go for the ruling.
		if err := ec.m.charge(1); err != nil {
			return Value{}, err
		}
		return unresolvedResult(s, b), nil
	}
	return callShape(ec, s, b, []Value{l, r})
}

// applyUnary dispatches a prefix operator.
//
// See applyBinary on why ec is threaded rather than zero-valued: CORRECTION
// (sub-project E1) to the previous claim that a zero-value evalCtx was safe at
// this call site.
func applyUnary(ec evalCtx, op Op, v Value) (Value, error) {
	s, b, ok := matchShapes(unaryShapes[op], []Type{v.Type})
	if !ok {
		return Value{}, fmt.Errorf("unsupported operand type for %s: %s", op, v.Type)
	}
	if v.IsUnresolved() {
		// The call was dispatched even though nothing ran, so rule 1 applies.
		// Rules 2 and 3 do NOT: no list was iterated and no string was
		// processed. See doc.go for the ruling.
		if err := ec.m.charge(1); err != nil {
			return Value{}, err
		}
		return unresolvedResult(s, b), nil
	}
	return callShape(ec, s, b, []Value{v})
}

// unresolvedResult is the whole reason a Shape declares a return type.
//
// When an operand has no value there is nothing to execute, so the result is
// read off the matched shape instead: substitute the bound type variables into
// the declared Ret and wrap it as a placeholder. Note that the shape was still
// SELECTED by the operand types, so a type error is still caught here — this
// path makes an expression checkable, not unconditionally valid.
//
// A shape whose result is a unification of its operands rather than a copy of
// one of them supplies RetOf and is asked instead; see Shape.RetOf.
func unresolvedResult(s Shape, b bindings) Value {
	if s.RetOf != nil {
		return Unresolved(s.RetOf(b))
	}
	return Unresolved(substitute(s.Ret, b))
}

// callShape coerces the arguments to the shape's declared parameter types and
// runs it. The shape may have been selected at a widening cost rather than an
// exact one, so an argument can still need converting — that is where section
// 2.1.1's int-to-float promotion actually happens.
func callShape(ec evalCtx, s Shape, b bindings, args []Value) (Value, error) {
	// Section 1.3.10 rule 1: every call is one operation, and this is the one
	// place every operator and every library function funnels through.
	if err := ec.m.charge(1); err != nil {
		return Value{}, err
	}
	for i := range args {
		want := substitute(s.Params[i], b)
		if args[i].Type.Equal(want) {
			continue
		}
		// This loop runs BEFORE chargeArgs, so a coercion that materializes
		// a collection is unpriced work: "sorted([1] + range_expr(
		// "1-10000000"))" expanded 2768 MB here and was only then charged for
		// it. See reserveRangeExprCoercion (rangeexpr.go).
		if err := reserveRangeExprCoercion(ec, args[i], want); err != nil {
			return Value{}, err
		}
		converted, err := coerce(args[i], want)
		if err != nil {
			return Value{}, err
		}
		args[i] = converted
	}
	if err := chargeArgs(ec, s.Cost, args); err != nil {
		return Value{}, err
	}
	var (
		out Value
		err error
	)
	if s.FnCtx != nil {
		out, err = s.FnCtx(ec, args)
	} else {
		out, err = s.Fn(args)
	}
	if err != nil {
		return Value{}, err
	}
	if err := chargeResult(ec, s.Cost, out); err != nil {
		return Value{}, err
	}
	return out, nil
}

// chargeArgs applies a Cost's declared argument charges.
//
// An index naming a parameter position that does not exist is ignored rather
// than panicking: a Cost is data, and a wrong index is caught by the coverage
// tests in the per-group tasks, not by crashing a running server.
func chargeArgs(ec evalCtx, c Cost, args []Value) error {
	for _, i := range c.ArgElements {
		if i < 0 || i >= len(args) {
			continue
		}
		n, err := elementCountBounded(ec, args[i])
		if err != nil {
			return err
		}
		if err := ec.m.chargeElements(n); err != nil {
			return err
		}
	}
	for _, i := range c.ArgBytes {
		if i < 0 || i >= len(args) {
			continue
		}
		if err := ec.m.chargeBytes(args[i].s); err != nil {
			return err
		}
	}
	return nil
}

// chargeResult applies a Cost's declared charges to the produced value.
func chargeResult(ec evalCtx, c Cost, out Value) error {
	if c.ResultElements {
		n, err := elementCount(out)
		if err != nil {
			return err
		}
		if err := ec.m.chargeElements(n); err != nil {
			return err
		}
	}
	if c.ResultBytes {
		if err := ec.m.chargeBytes(out.s); err != nil {
			return err
		}
	}
	return nil
}

// elementCountBounded is [elementCount] with a reservation in front of the one
// receiver kind for which COUNTING is itself expensive.
//
// A list answers its own count in constant time. A range_expr does not: with
// two or more sub-ranges rangeExprCount expands to count (rangeexpr.go), so a
// charge site that called elementCount in order to decide whether the work
// fits had already done the work by the time it found out -- 687 MB spent to
// produce a figure the very next line refuses. reserveRangeExprExpansion
// settles that arithmetically first.
//
// Use it at every site that charges BEFORE doing the work it prices.
// chargeResult is deliberately not one of those sites: its value already
// exists.
//
// It is NOT a complete guard against unreserved expansion on its own, and an
// earlier revision of this comment read as though it were. A charge site is
// only one of the ways a range_expr gets expanded; the coercions
// (reserveRangeExprCoercion) and equality's right-hand operand
// (reserveEqualityExpansion) have no charge in front of them at all. See
// reserveRangeExprExpansion (rangeexpr.go) for the full enumeration.
func elementCountBounded(ec evalCtx, v Value) (int, error) {
	if v.Type.Code == CodeRangeExpr {
		if err := reserveRangeExprExpansion(ec, v); err != nil {
			return 0, err
		}
	}
	return elementCount(v)
}

// elementCount is a value's rule-2 element count.
//
// A range_expr's count comes from rangeExprCount rather than from expanding the
// value here: charging the cost of counting must not itself repeat the work
// being charged for. That is exact and allocation-free for a single sub-range,
// and for two or more it falls back to an expansion bounded by the same cap
// rangeInts checks -- see rangeExprCount's doc comment (rangeexpr.go) for why
// the fallback exists and what it costs. A non-collection value counts as
// zero.
func elementCount(v Value) (int, error) {
	switch v.Type.Code {
	case CodeList:
		return len(v.l), nil
	case CodeRangeExpr:
		return rangeExprCount(v)
	default:
		return 0, nil
	}
}

// intBinary adapts an int64 operation to the table's signature.
func intBinary(f func(a, b int64) (int64, error)) binaryFunc {
	return func(l, r Value) (Value, error) {
		out, err := f(l.AsInt(), r.AsInt())
		if err != nil {
			return Value{}, err
		}
		return Int(out), nil
	}
}

// floatValue wraps a computed float, applying spec section 1.3.4: the language
// produces no negative zero, no infinity and no NaN. Every float-producing
// operation must return through here.
func floatValue(f float64) (Value, error) {
	switch {
	case math.IsNaN(f):
		return Value{}, errNotANumber
	case math.IsInf(f, 0):
		return Value{}, errInfinite
	case f == 0:
		// True for -0.0 as well; assigning the literal normalizes the sign.
		return Float(0), nil
	}
	return Float(f), nil
}

func addInt(a, b int64) (int64, error) {
	sum := a + b
	if (a > 0 && b > 0 && sum < 0) || (a < 0 && b < 0 && sum >= 0) {
		return 0, errIntOverflow
	}
	return sum, nil
}

func subInt(a, b int64) (int64, error) {
	diff := a - b
	if (b < 0 && diff < a) || (b > 0 && diff > a) {
		return 0, errIntOverflow
	}
	return diff, nil
}

func mulInt(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	// Checked before the division below, which would itself panic on
	// math.MinInt64 / -1.
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, errIntOverflow
	}
	product := a * b
	if product/b != a {
		return 0, errIntOverflow
	}
	return product, nil
}

func negInt(v Value) (Value, error) {
	i := v.AsInt()
	if i == math.MinInt64 {
		return Value{}, errIntOverflow
	}
	return Int(-i), nil
}

// floorDivInt implements "//" with floored division (spec section 2.1.1),
// rounding toward negative infinity. Go's "/" truncates toward zero, so a
// negative result with a non-zero remainder is one too high.
func floorDivInt(a, b int64) (int64, error) {
	if b == 0 {
		return 0, errDivideByZero
	}
	if a == math.MinInt64 && b == -1 {
		return 0, errIntOverflow
	}
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q, nil
}

// modInt implements "%" with floored semantics, so the result takes the
// divisor's sign: -7 % 3 is 2, not -1.
func modInt(a, b int64) (int64, error) {
	if b == 0 {
		return 0, errModuloByZero
	}
	if a == math.MinInt64 && b == -1 {
		return 0, nil
	}
	r := a % b
	if r != 0 && (r < 0) != (b < 0) {
		r += b
	}
	return r, nil
}

// divInts implements __truediv__(int, int) -> float: "/" is always float
// division, even when the operands divide exactly.
func divInts(l, r Value) (Value, error) {
	b := r.AsInt()
	if b == 0 {
		return Value{}, errDivideByZero
	}
	return floatValue(float64(l.AsInt()) / float64(b))
}

// powInts implements __pow__(int, int) -> int | float. The result is an int
// for a non-negative exponent and a float for a negative one, so 2 ** 3 is 8
// and 2 ** -3 is 0.125.
func powInts(l, r Value) (Value, error) {
	base, exp := l.AsInt(), r.AsInt()
	if exp < 0 {
		if base == 0 {
			return Value{}, errZeroNegPower
		}
		return floatValue(math.Pow(float64(base), float64(exp)))
	}
	out, err := powIntPositive(base, exp)
	if err != nil {
		return Value{}, err
	}
	return Int(out), nil
}

// powIntPositive raises base to a non-negative exponent by repeated squaring.
//
// The algorithm matters: a loop over the exponent would not terminate for
// "2 ** 9223372036854775807", whereas squaring overflows within about six
// iterations and reports it immediately. Bases of 0, 1 and -1 never overflow
// and terminate in at most 63 iterations.
func powIntPositive(base, exp int64) (int64, error) {
	result, factor, e := int64(1), base, exp
	for e > 0 {
		if e&1 == 1 {
			var err error
			if result, err = mulInt(result, factor); err != nil {
				return 0, err
			}
		}
		e >>= 1
		if e == 0 {
			break
		}
		var err error
		if factor, err = mulInt(factor, factor); err != nil {
			return 0, err
		}
	}
	return result, nil
}

// floatBinary adapts a total float64 operation to the table's signature. The
// result passes through floatValue, so section 1.3.4's no-infinity, no-NaN
// and no-negative-zero rules apply to every float-producing operator.
func floatBinary(f func(a, b float64) float64) binaryFunc {
	return func(l, r Value) (Value, error) {
		return floatValue(f(l.AsFloat(), r.AsFloat()))
	}
}

func divFloats(l, r Value) (Value, error) {
	b := r.AsFloat()
	if b == 0 {
		return Value{}, errDivideByZero
	}
	return floatValue(l.AsFloat() / b)
}

// floorDivFloats implements __floordiv__(float, float) -> int. The int return
// is what the spec's signature table specifies; it is not a slip.
func floorDivFloats(l, r Value) (Value, error) {
	b := r.AsFloat()
	if b == 0 {
		return Value{}, errDivideByZero
	}
	q := math.Floor(l.AsFloat() / b)
	// float64(math.MaxInt64) is exactly 2^63, one past the last representable
	// int64, so the upper bound is a >= test rather than a > test.
	if math.IsNaN(q) || q < math.MinInt64 || q >= float64(math.MaxInt64) {
		return Value{}, errIntOverflow
	}
	return Int(int64(q)), nil
}

// modFloats implements "%" on floats with floored semantics, so the result
// takes the divisor's sign: -7.0 % 3.0 is 2.0. math.Mod truncates.
func modFloats(l, r Value) (Value, error) {
	b := r.AsFloat()
	if b == 0 {
		return Value{}, errModuloByZero
	}
	rem := math.Mod(l.AsFloat(), b)
	if rem != 0 && (rem < 0) != (b < 0) {
		rem += b
	}
	return floatValue(rem)
}

func powFloats(l, r Value) (Value, error) {
	base, exp := l.AsFloat(), r.AsFloat()
	if base == 0 && exp < 0 {
		return Value{}, errZeroNegPower
	}
	if base < 0 && exp != math.Trunc(exp) {
		return Value{}, errNegFracPower
	}
	return floatValue(math.Pow(base, exp))
}

// concatStrings implements section 2.1.2's string concatenation, bounded by
// the same maxStringBytes that bounds string REPETITION.
//
// Without the bound this was the one unbounded producer left in the package —
// a plain asymmetry, since concatLists has always checked its own element
// count. Repetition alone is no defense: "'x' * 900000" is well inside the
// bound, and twenty of those chained with "+" measured 180,000,000 bytes, 18
// times over it. The check is ARITHMETIC on the two lengths, before the
// concatenation allocates, for the same reason checkElementCount is: a check
// made after allocating would be no protection at all.
func concatStrings(l, r Value) (Value, error) {
	if err := checkStringBytes(len(l.AsStr()) + len(r.AsStr())); err != nil {
		return Value{}, err
	}
	return String(l.AsStr() + r.AsStr()), nil
}

// joinPaths implements RFC 0006 section 2.1.5's "/" for both of its rows.
//
// The right operand's text is re-parsed under the LEFT operand's flavor. That
// is where a binary path operator's flavor has to come from — a path Value
// carries its own (Value.pf), and the evaluator's setting is reachable only
// from a Shape.FnCtx row, which by design is path()'s alone. Re-parsing the
// right operand under the left's flavor also means the two rows need no
// separate code: a path right operand contributes nothing but its text.
//
// The join itself is pathJoin's (pathval.go), and the rendering is String()'s;
// boundedPath re-parses the rendered text, which is what normalizes the result
// and bounds its size, exactly as every other path-producing function does.
func joinPaths(l, r Value) (Value, error) {
	parent := pathOf(l)
	joined := pathJoin(parent, parsePath(pathOperandText(r), parent.flavor))
	return boundedPath(joined.String(), joined.flavor)
}

// appendToPath implements section 2.1.5's __add__(p: path, suffix: string) ->
// path: the suffix is appended with NO separator and the result is re-parsed,
// so "path('/a/b') + 'x'" is the path "/a/bx".
//
// The specification's table describes this as appending to the last
// COMPONENT, and for every path that has one the two readings agree. They part
// company on a path that has none, where appending to the rendered text is the
// reading that matches the reference implementation: "path('/') + 'x'" is
// "/x", not an empty-name error.
func appendToPath(l, r Value) (Value, error) {
	p := pathOf(l)
	return boundedPath(p.String()+r.AsStr(), p.flavor)
}

// pathOperandText reads a path operator's right operand, which the (path,
// string) and (path, path) rows deliver under two different type codes: AsStr
// panics on a path and pathText panics on a string, so neither accessor alone
// can serve both rows.
func pathOperandText(v Value) string {
	if v.Type.Code == CodePath {
		return pathText(v)
	}
	return v.AsStr()
}

// containsString implements __contains__(a, b) for "b in a". The expression's
// LEFT operand is the needle and the right is the haystack, which is the
// reverse of the signature's parameter order.
func containsString(l, r Value) (Value, error) {
	return Bool(strings.Contains(r.AsStr(), l.AsStr())), nil
}

func notContainsString(l, r Value) (Value, error) {
	return Bool(!strings.Contains(r.AsStr(), l.AsStr())), nil
}

func negFloat(v Value) (Value, error) { return floatValue(-v.AsFloat()) }

func notBool(v Value) (Value, error) { return Bool(!v.AsBool()), nil }

// ordering turns a three-way comparison into the binaryFunc for one ordering
// operator. Each ordering shape it backs is declared same-type (int/int,
// float/float, string/string, bool/bool, list/list); section 2.1.4's
// compatible cross-pairs — int/float and string/path — reach a same-type
// shape by promotion during shape matching (shape.go's argCost), not by an
// extra row here. A cross-type pair that is not one of those two compatible
// pairs still has no shape to promote into, so it is correctly reported as
// unsupported.
//
// The comparator returns an error, not just an int, because compareLists
// (the list/list shape) can recurse into elements with no defined order and
// has to report that rather than fabricate a result — the scalar
// comparators below simply never fail.
func ordering(op Op, compare func(l, r Value) (int, error)) binaryFunc {
	return func(l, r Value) (Value, error) {
		c, err := compare(l, r)
		if err != nil {
			return Value{}, err
		}
		switch op {
		case OpLt:
			return Bool(c < 0), nil
		case OpGt:
			return Bool(c > 0), nil
		case OpLe:
			return Bool(c <= 0), nil
		case OpGe:
			return Bool(c >= 0), nil
		}
		return Value{}, fmt.Errorf("%s is not an ordering operator", op)
	}
}

func compareInts(l, r Value) (int, error) { return cmp.Compare(l.AsInt(), r.AsInt()), nil }

func compareFloats(l, r Value) (int, error) { return cmp.Compare(l.AsFloat(), r.AsFloat()), nil }

func compareStrings(l, r Value) (int, error) { return strings.Compare(l.AsStr(), r.AsStr()), nil }

// compareBools orders False before True (spec section 2.1.4).
func compareBools(l, r Value) (int, error) {
	return cmp.Compare(boolRank(l.AsBool()), boolRank(r.AsBool())), nil
}

// compareLists implements section 1.2.5's lexicographic list ordering:
// elements are compared pairwise from the start using compareValues (so a
// nested list compares recursively through here too), the first unequal pair
// decides, and if every compared element is equal the shorter list is less.
func compareLists(a, b Value) (int, error) {
	left, right := a.AsList(), b.AsList()
	for i := 0; i < len(left) && i < len(right); i++ {
		c, err := compareValues(left[i], right[i])
		if err != nil {
			return 0, err
		}
		if c != 0 {
			return c, nil
		}
	}
	return cmp.Compare(len(left), len(right)), nil
}

// compareValues orders two same-type values, for comparing list elements
// pairwise in compareLists. It dispatches to the same per-type comparators
// the scalar ordering shapes use (shape matching guarantees a list's two
// operands share an element type, so this never sees a genuine cross-type
// pair from that path) and recurses through compareLists for a nested list.
func compareValues(a, b Value) (int, error) {
	switch {
	case a.Type.Code == CodeInt && b.Type.Code == CodeInt:
		return compareInts(a, b)
	case a.Type.Code == CodeFloat && b.Type.Code == CodeFloat:
		return compareFloats(a, b)
	case a.Type.Code == CodeString && b.Type.Code == CodeString:
		return compareStrings(a, b)
	case a.Type.Code == CodePath && b.Type.Code == CodePath:
		// Section 2.1.4 lists path among the orderable types. A SCALAR path
		// pair never reaches here — it promotes to the (string, string) shape
		// and is compared as text — but a list[path] pair matches the list
		// ordering shape EXACTLY, at no cost, so nothing coerces its elements
		// and this is the only place the comparison can happen. Comparing the
		// payload as text is what the scalar path does after promotion, so the
		// two agree by construction. The payload is read directly (v.s) rather
		// than through AsStr, which panics on anything but CodeString.
		return strings.Compare(a.s, b.s), nil
	case a.Type.Code == CodeBool && b.Type.Code == CodeBool:
		return compareBools(a, b)
	case a.Type.Code == CodeList && b.Type.Code == CodeList:
		return compareLists(a, b)
	}
	return 0, fmt.Errorf("unsupported operand types for a comparison: %s and %s", a.Type, b.Type)
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// valuesEqual implements cross-type equality (spec section 1.2.5):
//
//   - int vs float compare numerically, so 5 == 5.0
//   - string vs path: the path converts to string for the comparison
//   - bool vs any non-bool is always unequal, so true == 1 is false
//   - string vs a number is always unequal, so "5" == 5 is false
//   - null equals only null
//   - list equality is recursive, and a range_expr compared with a list is
//     expanded and compared elementwise (both via listOrRangeEqual below)
//   - every other cross-type pair is unequal
//
// The null and bool cases are split out from numericOrStringEqual below
// purely to keep this function's cyclomatic complexity under the repo's
// lint threshold; the combined behavior is unchanged.
func valuesEqual(l, r Value) bool {
	switch {
	case l.Type.Code == CodeNull || r.Type.Code == CodeNull:
		return l.Type.Code == r.Type.Code
	case l.Type.Code == CodeBool || r.Type.Code == CodeBool:
		return l.Type.Code == r.Type.Code && l.AsBool() == r.AsBool()
	}
	return numericOrStringEqual(l, r)
}

// numericOrStringEqual handles the int/float/string cases of valuesEqual.
// Callers must have already excluded null and bool operands.
func numericOrStringEqual(l, r Value) bool {
	switch {
	case l.Type.Code == CodeInt && r.Type.Code == CodeInt:
		return l.AsInt() == r.AsInt()
	case l.Type.Code == CodeFloat && r.Type.Code == CodeFloat:
		return l.AsFloat() == r.AsFloat()
	case l.Type.Code == CodeInt && r.Type.Code == CodeFloat:
		return intEqualsFloat(l.AsInt(), r.AsFloat())
	case l.Type.Code == CodeFloat && r.Type.Code == CodeInt:
		return intEqualsFloat(r.AsInt(), l.AsFloat())
	case l.Type.Code == CodeString && r.Type.Code == CodeString:
		return l.AsStr() == r.AsStr()
	case l.Type.Code == CodePath || r.Type.Code == CodePath:
		return pathEqual(l, r)
	}
	return listOrRangeEqual(l, r)
}

// listOrRangeEqual handles the list and range_expr equality rows of
// valuesEqual, split out for the same complexity reason pathEqual is: list
// equality is recursive (same length, all corresponding elements equal, spec
// section 1.2.5), and a range_expr compared against a list is expanded and
// compared elementwise.
//
// It is also where the range_expr vs range_expr row lives — closing a
// previously PARKED gap where two range_exprs, including one compared to
// itself, always compared unequal. Comparing their raw text is not right in
// general (section 1.2.5's expansion rule would make "1-3" equal "1,2,3",
// which a payload comparison can't see), so the general case expands both
// sides and compares elementwise like the list rows. But two operands with
// IDENTICAL text always expand to the identical list, so that case is
// checked first without expanding — which is what keeps a range_expr past
// the element-count bound equal to itself even though expanding it would
// error.
//
// An expansion failure elsewhere in this function (list vs an oversized
// range_expr, or two DIFFERENT range_expr texts where at least one is
// oversized) becomes false rather than propagating: valuesEqual returns a
// bare bool by design, since section 1.2.5 makes equality total and gives it
// no error channel. The only way to reach an expansion failure is a
// range_expr past the size bound, which cannot be materialized to compare
// against a list — or another differently-spelled range_expr — anyway.
//
// Anything else reaching here is a cross-type pair 1.2.5 makes unequal.
func listOrRangeEqual(l, r Value) bool {
	switch {
	case l.Type.Code == CodeList && r.Type.Code == CodeList:
		return listsEqual(l.AsList(), r.AsList())
	case l.Type.Code == CodeRangeExpr && r.Type.Code == CodeList:
		expanded, err := rangeInts(l)
		if err != nil {
			return false
		}
		return listsEqual(intValues(expanded), r.AsList())
	case l.Type.Code == CodeList && r.Type.Code == CodeRangeExpr:
		expanded, err := rangeInts(r)
		if err != nil {
			return false
		}
		return listsEqual(l.AsList(), intValues(expanded))
	case l.Type.Code == CodeRangeExpr && r.Type.Code == CodeRangeExpr:
		if l.s == r.s {
			return true
		}
		lInts, lErr := rangeInts(l)
		rInts, rErr := rangeInts(r)
		if lErr != nil || rErr != nil {
			return false
		}
		return listsEqual(intValues(lInts), intValues(rInts))
	}
	return false
}

// listsEqual compares two element slices with the language's own cross-type
// equality, so that [1] == [1.0] is true (section 1.2.5).
func listsEqual(a, b []Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// intValues wraps expanded range integers as list elements. It is the single
// copy of that two-line loop: coerce.go (range_expr -> list[int]) and slice.go
// (a range_expr slice that cannot come back as a range_expr) each hand-rolled
// their own before, which is three chances for one of them to drift.
func intValues(ints []int64) []Value {
	out := make([]Value, len(ints))
	for i, n := range ints {
		out[i] = Int(n)
	}
	return out
}

// pathEqual handles the equality rows where at least one operand is a path.
// Split out of numericOrStringEqual for the same reason null and bool are split
// out of valuesEqual: to keep each function under the repo's complexity
// threshold. Callers must have already excluded null and bool operands.
//
// A path compares equal to a string with the same text, which is section
// 1.2.5's rule that the path converts to string for the comparison, and equal
// to a path with the same text, which is not that section's business — 1.2.5
// covers CROSS-type comparison — but is required for a path to equal itself.
// Anything else is a cross-type pair 1.2.5 makes unequal.
//
// The path's payload is read directly (v.s) rather than through AsStr, which
// panics on anything but CodeString.
func pathEqual(l, r Value) bool {
	if l.Type.Code != CodeString && l.Type.Code != CodePath {
		return false
	}
	if r.Type.Code != CodeString && r.Type.Code != CodePath {
		return false
	}
	return l.s == r.s
}

// intEqualsFloat compares an int64 to a float64 exactly. Converting the int to
// float64 and comparing would lose precision above 2^53, making distinct
// integers look equal.
func intEqualsFloat(i int64, f float64) bool {
	if f != math.Trunc(f) {
		return false
	}
	// float64(math.MaxInt64) is exactly 2^63, one past the last representable
	// int64, so the upper bound is a >= test.
	if f < math.MinInt64 || f >= float64(math.MaxInt64) {
		return false
	}
	return int64(f) == i
}

// concatLists implements section 2.1.3's list concatenation. The result's
// element type is the common type of both operands' — the same unification
// section 1.2.6 uses for a list literal, reused rather than restated.
//
// unifyElemPair is called on the two LIST types themselves (l.Type, r.Type),
// not on their already-extracted element types: its empty-list rule ("rule
// 6", list.go) fires only when both of ITS OWN arguments are list types, so
// it can adopt the other side's element when one is list[nulltype]. Passing
// pre-stripped element types (nulltype vs int) would skip that rule entirely
// and wrongly reject "[] + [1]" as incompatible.
func concatLists(l, r Value) (Value, error) {
	combined, ok := unifyElemPair(l.Type, r.Type)
	if !ok {
		return Value{}, fmt.Errorf("cannot concatenate %s and %s: incompatible element types", l.Type, r.Type)
	}
	elem, _ := listElem(combined)
	left, right := l.AsList(), r.AsList()
	if err := checkElementCount(len(left) + len(right)); err != nil {
		return Value{}, err
	}
	// Both operands are ranged over in turn rather than joined into a
	// throwaway slice first: the join allocated a complete second copy of both
	// operands, on top of the correctly-sized result below, purely to have one
	// loop instead of two — doubling the peak memory of every concatenation,
	// with maxElements putting that at 10,000,000 values.
	out := make([]Value, 0, len(left)+len(right))
	for _, side := range [][]Value{left, right} {
		for _, v := range side {
			converted, err := coerce(v, elem)
			if err != nil {
				return Value{}, err
			}
			out = append(out, converted)
		}
	}
	return List(elem, out), nil
}

// concatRet is the generic list concatenation shape's RetOf: the type
// concatLists WOULD produce, computed from the matched element bindings.
//
// It calls unifyElemPair on the two LIST types rather than on the bound element
// types, exactly as concatLists does and for the same reason — the empty-list
// rule fires only between two list types (list.go), which is what lets
// list[nulltype] adopt the other side's element type in EITHER position.
//
// An unbound variable, or a pair with no common type, leaves the left operand's
// list type as the answer. Neither is reachable from the shape this is attached
// to: both parameters are list[var], so matching binds both, and a pair with no
// common type is one concatLists would reject — but a declared return type is
// read on the path where nothing executes, so it must still answer something,
// and the pre-RetOf answer is the conservative one.
func concatRet(b bindings) Type {
	left, leftOK := b.get(CodeVarT)
	right, rightOK := b.get(CodeVarT1)
	if !leftOK || !rightOK {
		return ListOf(left)
	}
	combined, ok := unifyElemPair(ListOf(left), ListOf(right))
	if !ok {
		return ListOf(left)
	}
	return combined
}

// concatRanges implements section 2.1.3's range_expr + range_expr -> list[int]
// for OpAdd's exact-match (TRangeExpr, TRangeExpr) row. That row is exact-match
// on purpose (see the OpAdd table's comment on why), which means callShape
// performs no coercion before calling it — so unlike concatLists's other
// callers, this one must expand each range itself before delegating.
func concatRanges(l, r Value) (Value, error) {
	ll, err := coerce(l, ListOf(TInt))
	if err != nil {
		return Value{}, err
	}
	rl, err := coerce(r, ListOf(TInt))
	if err != nil {
		return Value{}, err
	}
	return concatLists(ll, rl)
}

// repeatList implements section 2.1.3's list repetition. A non-positive count
// gives an empty list, as in Python.
//
// It takes ec so it can RESERVE the section 1.3.10 cost of the list it is about
// to build before building it -- see meter.reserve. The row's
// Cost{ResultElements: true} still levies the actual charge afterwards; this
// only refuses, and refuses on exactly the arithmetic the charge would report.
func repeatList(ec evalCtx, l, r Value) (Value, error) {
	elem, _ := listElem(l.Type)
	elems := l.AsList()
	n := r.AsInt()
	// Repeating an empty list is always empty, for ANY n, including one too
	// large to even loop over — checkRepeat treats a zero unitSize as
	// needing no bound (nothing to overflow), so it would let an arbitrarily
	// large n through, and "for range n" below would then spin, doing
	// nothing but burning CPU, for as long as n says. Short-circuiting here
	// avoids that hang entirely rather than relying on the bound to prevent
	// it.
	if n <= 0 || len(elems) == 0 {
		return List(elem, nil), nil
	}
	// checkRepeat multiplies len(elems) by n only after confirming the
	// product cannot overflow int64 — see its doc comment. Multiplying first
	// and checking the result, as this used to, is not a bound at all: the
	// product can wrap to a small or negative number and slip past it.
	total, err := checkRepeat(len(elems), n, maxElements)
	if err != nil {
		return Value{}, err
	}
	// BEFORE make() and the loop, not after: chargeResult's ResultElements
	// charge cannot stop this allocation, only bill for it once it has
	// happened.
	if err := ec.m.reserve(total); err != nil {
		return Value{}, err
	}
	out := make([]Value, 0, total)
	for range n {
		out = append(out, elems...)
	}
	return List(elem, out), nil
}

// repeatString implements section 2.1.2's string repetition, bounded by length
// rather than by element count.
//
// It takes ec for the same reason repeatList does: to reserve rule 3's
// ceil(length / 256) on the string it is about to build, before it exists.
func repeatString(ec evalCtx, l, r Value) (Value, error) {
	s := l.AsStr()
	n := r.AsInt()
	if n <= 0 || s == "" {
		return String(""), nil
	}
	// See repeatList's comment: checkRepeat's division-first bound is what
	// makes this multiplication-free of the overflow that let the previous,
	// multiply-then-check version through.
	total, err := checkRepeat(len(s), n, maxStringBytes)
	if err != nil {
		return Value{}, err
	}
	if err := ec.m.reserveBytes(total); err != nil {
		return Value{}, err
	}
	return String(strings.Repeat(s, int(n))), nil
}

// containsElem implements section 2.1.3's membership test. It uses the
// language's own cross-type equality (section 1.2.5) rather than Value.Equal, so
// that "2.0 in [1, 2, 3]" is true.
func containsElem(item, list Value) (Value, error) {
	for _, elem := range list.AsList() {
		if valuesEqual(item, elem) {
			return Bool(true), nil
		}
	}
	return Bool(false), nil
}

// containsRangeInt implements section 2.1.3's range membership test.
func containsRangeInt(item, rng Value) (Value, error) {
	ints, err := rangeInts(rng)
	if err != nil {
		return Value{}, err
	}
	return Bool(slices.Contains(ints, item.AsInt())), nil
}

// negate inverts a bool-returning operator implementation, which is how each
// "not in" row is built from its "in" counterpart.
func negate(f func(l, r Value) (Value, error)) func(l, r Value) (Value, error) {
	return func(l, r Value) (Value, error) {
		v, err := f(l, r)
		if err != nil {
			return Value{}, err
		}
		return Bool(!v.AsBool()), nil
	}
}
