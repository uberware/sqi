// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "maps"

// Shape is one accepted signature of an operator or function: the types it
// takes, the type it gives back, and the code that computes it.
//
// Sub-project A dispatched on a map keyed by operand kind. Two facts retired
// that. A Type contains a slice, so it cannot be a Go map key at all. And a map
// entry does not record what it returns, which makes it impossible to answer
// "what would this produce?" for an operand that has no value — the question at
// the center of the spec's static type checking. A declared Ret answers it
// without executing anything.
//
// Params and Ret may contain type variables (CodeVarT and friends), bound at
// match time and substituted into Ret afterward, so that a signature like
// __getitem__(list[T], int) -> T returns the element type. Sub-project C
// registers its function library into this same structure.
type Shape struct {
	// Params are the declared parameter types, one per argument.
	Params []Type
	// Ret is the declared result type. It may be a union: the spec's own
	// __pow__(int, int) -> float | int is one.
	Ret Type
	// RetOf, when set, computes the declared result from the match's variable
	// bindings INSTEAD of substituting them into Ret.
	//
	// It exists for a result type that is a UNIFICATION of the operands rather
	// than a copy of one of them. List concatenation is the case: its two
	// element variables are independent (they must be, or "[1] + [2.0]" would
	// not match at all), so no substitution into Ret can say what section
	// 2.1.3's "common type" of the two is — "[] + list[int]" reads the LEFT
	// variable and answers list[nulltype], and "list[int] + []" answers
	// list[int], for the same concatenation. concatLists computes the real
	// answer from the VALUES, which is no help on the path where there are no
	// values; this is that same computation over types.
	RetOf func(b bindings) Type
	// Fn computes the result. It is called only with arguments that matched
	// Params, so it may use the payload accessors without checking.
	Fn func(args []Value) (Value, error)
	// Promote narrows which conversions this shape's parameters will accept to
	// admit an argument of a different type. The zero value accepts every
	// lossless conversion coerce.go allows, which is right for most operators
	// and wrong for ordering — see promotion.
	Promote promotion
}

// promotion selects the set of conversions a shape's parameters accept.
//
// It exists because admissibility is NOT uniform across operators, which
// sub-project B1 assumed and documented as a divergence when it turned out
// false. Section 1.2.3's range_expr -> string coercion is real, and section
// 2.1.2 has an explicit string + range_expr row that depends on it; but section
// 2.1.4 restricts ORDERING to int, float, string, path and bool, crossing type
// only between int/float and string/path. With one global predicate, "r < 'a'"
// was accepted.
type promotion int

const (
	// promoteDefault accepts every lossless conversion, which is what section
	// 1.2.3 permits generally.
	promoteDefault promotion = iota
	// promoteOrdering accepts only section 2.1.4's two named compatible pairs,
	// int/float and string/path, plus the elementwise form of each for lists.
	promoteOrdering
	// promoteNoRangeText accepts every lossless conversion EXCEPT
	// range_expr -> string, which section 1.2.3 defines but which the operator
	// tables grant to ONE operator only. See the OpMul table's comment on why
	// that is a reading of the spec rather than a restriction invented here.
	promoteNoRangeText
)

// promotableUnder is promotable, restricted by a shape's promotion set.
func promotableUnder(pr promotion, from, to Type) bool {
	if !promotable(from, to) {
		return false
	}
	if pr == promoteNoRangeText {
		return unwrapUnresolved(from).Code != CodeRangeExpr
	}
	if pr != promoteOrdering {
		return true
	}
	f, t := unwrapUnresolved(from), unwrapUnresolved(to)
	// Section 2.1.4's compatible pairs, and only these. Both are pairs of
	// SCALARS, which is all this function ever has to answer for: a list
	// argument against a list parameter never reaches here at all, because
	// argCost intercepts that pair first and descends elementwise through
	// argCostList. The elementwise form of these pairs — section 1.2.5's
	// "[1] < [1.0]" — is therefore applied there, by typeVarCost's ordering
	// unification, not here.
	//
	// An earlier revision carried a list branch of its own for that case,
	// unreachable on every path and flagged as such; it was removed once the
	// elementwise rule landed for real, rather than left as an untestable
	// second implementation of it. A list argument reaching a SCALAR parameter
	// is the only list-shaped call this function sees, and promotable() above
	// has already answered it false.
	switch {
	case f.Code == CodeInt && t.Code == CodeFloat:
		return true
	case f.Code == CodePath && t.Code == CodeString:
		return true
	}
	return false
}

// bindings records what each type variable in a signature bound to.
type bindings map[Code]Type

// Argument costs. A candidate's cost is the sum over its parameters, and the
// cheapest admissible candidate wins.
const (
	// costExact is an argument that already has the declared parameter type.
	costExact = 0
	// costWiden is an argument reaching its parameter by a conversion that
	// cannot fail on any value.
	costWiden = 1
)

// matchShapes selects the shape for a call with the given argument types, and
// returns the variable bindings the match produced.
//
// Selection is by COST, not by position. Each argument scores costExact when it
// already has the declared type, costWiden when it gets there by a conversion
// that can never fail, and is INADMISSIBLE when the only route is a conversion
// that can fail on some value. A candidate with any inadmissible argument is not
// a candidate at all; among the rest, the lowest total cost wins, and an exact
// tie breaks to the earliest shape.
//
// Two things fall out of that, and both are the point:
//
// An all-costExact candidate always beats a converting one, so "1 + 1" can never
// land on the (float, float) shape however the list is ordered — shape authors
// never have to reason about position.
//
// A conversion that can fail is never chosen to SELECT an overload. Section
// 1.2.3 does permit float -> int and string -> int, but only where a context
// demanded that type and is prepared for the value not to fit. Letting the
// matcher use them would make "1 + 2.5" pick (int, int) by discarding the .5,
// and would let "'a' + 'b'" match (int, int) by parsing both strings. Section
// 2.1.1 says the opposite happens: "the int is promoted to float and the float
// overload is used". Ranking with an inadmissible tier is what produces that,
// and it is not a separate promotion rule.
func matchShapes(shapes []Shape, args []Type) (Shape, bindings, bool) {
	best := -1
	var bestShape Shape
	var bestBindings bindings
	for _, s := range shapes {
		if len(s.Params) != len(args) {
			continue
		}
		b := bindings{}
		cost, ok := shapeCost(s, args, b)
		if !ok {
			continue
		}
		// Strictly less, so an exact tie keeps the earliest shape.
		if best < 0 || cost < best {
			best, bestShape, bestBindings = cost, s, b
		}
	}
	if best < 0 {
		return Shape{}, nil, false
	}
	return bestShape, bestBindings, true
}

// shapeCost sums the cost of passing each argument to its declared parameter,
// accumulating type-variable bindings. It reports false as soon as any argument
// is inadmissible.
func shapeCost(s Shape, args []Type, b bindings) (int, bool) {
	total := 0
	for i := range s.Params {
		cost, ok := argCost(s.Params[i], args[i], b, s.Promote)
		if !ok {
			return 0, false
		}
		total += cost
	}
	return total, true
}

// argCost reports what it costs to pass an argument of type arg to a parameter
// declared as param, and whether it is admissible at all. pr narrows which
// conversions the enclosing shape accepts — see promotion.
func argCost(param, arg Type, b bindings, pr promotion) (int, bool) {
	// A placeholder is matched on its constraint. Its lack of a value is
	// irrelevant to selecting a signature — which is what lets an expression be
	// type-checked before any parameter value exists.
	if c, ok := unresolvedConstraint(arg); ok {
		return argCost(param, c, b, pr)
	}
	if isTypeVar(param.Code) {
		return typeVarCost(param.Code, arg, b, pr)
	}
	// "any" accepts anything, but costs more than an exact match so that a
	// specific shape wins over a generic one when both fit.
	if param.Code == CodeAny {
		return costWiden, true
	}
	// Descend into a parameterized parameter so a variable inside it can bind —
	// list[T] against list[int] must bind T to int.
	if param.Code == CodeList && arg.Code == CodeList &&
		len(param.Params) == 1 && len(arg.Params) == 1 {
		return argCostList(param, arg, b, pr)
	}
	// A union parameter is scored member-wise, before the exact-match fallback
	// below: the union as a whole is never Equal to one of its own members, so
	// an exact member match (int against int | float | string) would otherwise
	// be missed entirely. This is also more precise than testing membership by
	// type code, which could not tell list[int] from list[string] inside the
	// union — argCost's own list-descent handles that correctly per member.
	if param.Code == CodeUnion {
		return unionArgCost(param, arg, b, pr)
	}
	// A union ARGUMENT is the dual case: it is SOME ONE of its members,
	// decided at runtime, so it is admissible only where EVERY member would
	// be — the caller must be prepared to pay for whichever member actually
	// shows up, so the cost is the worst of them rather than the best.
	if arg.Code == CodeUnion {
		return unionArgValueCost(param, arg, b, pr)
	}
	if param.Equal(arg) {
		return costExact, true
	}
	if promotableUnder(pr, arg, param) {
		return costWiden, true
	}
	return 0, false
}

// typeVarCost binds a type variable to an argument, or scores the argument
// against the type the variable already bound to.
//
// A variable binds once: seeing it a second time requires the same type…
// …with one exception, and it is the empty list literal's. Section 1.2.6 rule 6
// makes list[nulltype] "implicitly convertible to list[T] for any T", so a
// binding that came from an empty list names no type the caller actually chose
// — it is a placeholder for whatever the OTHER occurrence turns out to be.
// Holding it fixed made the shared-variable ordering shape (list[T], list[T])
// ORDER-DEPENDENT: "[1] < []" matched, because T was already int when
// argCostList's own empty-list branch declined to rebind it, while "[] < [1]"
// did not, because T had been pinned to nulltype first and int then mismatched.
// Section 1.2.5 says the shorter list is less, so both are answerable, and the
// reference implementation agrees ("[] < [1]" is true, "[1] < []" false).
//
// The exception has to be SYMMETRIC, and was not. One layer down, where the
// empty literal reaches this function as a plain argument rather than through
// argCostList's own branch, "[[]] < [[1]]" bound T to list[nulltype] and then
// replaced it with list[int], while "[[1]] < [[]]" bound T to list[int] and met
// list[nulltype] — the same pair, the same rule, and an "unsupported operand
// types" error for want of the second direction. The reference returns false
// for it, queried directly.
//
// There is a SECOND exception, and it belongs to ordering alone (pr,
// promoteOrdering): section 2.1.4's compatible pairs, applied elementwise. See
// orderingUnified below for the adjudication; the mechanism is that the two
// occurrences are unified rather than required equal, so the variable ends up
// naming the type BOTH operands reach — list[float] for "[1] < [1.0]" — and
// callShape then coerces each operand to it before compareLists runs. Nothing
// downstream has to learn about cross-type element comparison, because by the
// time it runs there is no longer a cross-type pair.
func typeVarCost(code Code, arg Type, b bindings, pr promotion) (int, bool) {
	bound, ok := b[code]
	if !ok {
		b[code] = arg
		return costExact, true
	}
	if bound.Equal(arg) {
		return costExact, true
	}
	// The empty side may be EITHER of the two, and the fix that made "[] < [1]"
	// work handled only the first of these. When the BINDING is the empty one it
	// is replaced, so the variable ends up naming the type the caller actually
	// chose. When the INCOMING ARGUMENT is the empty one the binding already
	// names that type, so it stands and the argument is merely admitted:
	// rebinding to it would throw the real element type away and pin the
	// variable to nulltype, which is what the first branch exists to undo.
	if emptyListBinding(bound, arg) {
		b[code] = arg
		return costWiden, true
	}
	if emptyListBinding(arg, bound) {
		return costWiden, true
	}
	if unified, ok := orderingUnified(pr, bound, arg); ok {
		b[code] = unified
		return costWiden, true
	}
	return 0, false
}

// orderingUnified answers section 2.1.4's compatible pairs applied ELEMENTWISE,
// for the shared element variable of the list ordering shape: given the type one
// operand's elements have and the type the other's have, the type both reach, or
// false when there is none.
//
// The adjudication, since this reverses a documented behavior. Section 1.2.5
// defines list ordering as "elements are compared pairwise from the start, and
// the first unequal pair determines the result" — it constrains the two lists'
// LENGTHS and nothing about their element types. Section 2.1.4 then says an
// ordering operator's operands "may differ for compatible pairs (int/float and
// string/path)". Composing the two, an elementwise comparison of an int against
// a float is an ordering comparison of a compatible pair, which section 2.1.4
// permits; so "[1] < [1.0]" is false, not an error. It was an error here, and
// that error was the artifact of an implementation choice rather than a reading
// of the spec: one shared type variable across both parameters, which requires
// EXACT equality by construction.
//
// unifyElemPair (list.go) is reused rather than restated because it already
// computes exactly this set — section 1.2.6's int/float and path/string rules,
// applied through nested lists — and section 1.2.6's compatible pairs are
// section 2.1.4's, named twice in the same spec. Anything outside it stays
// inadmissible, which is what keeps "['a'] < [1]" and "[1] < [[]]" reported as
// unsupported operand types rather than failing later inside coerce().
//
// The reference implementation agrees on the result ("[1] < [1.0]" is false
// there, "[1.0] < [1]" false, "[1] <= [1.0]" true), which is corroboration and
// not the reason; it disagrees with sqi in the other direction on the adjacent
// membership question ("2.0 in [1, 2, 3]" errors there and is true here, see
// test/oracle/baseline.txt), and the spec is what decides both.
func orderingUnified(pr promotion, bound, arg Type) (Type, bool) {
	if pr != promoteOrdering {
		return Type{}, false
	}
	return unifyElemPair(bound, arg)
}

// emptyListBinding reports whether a type variable's existing binding came from
// an empty list literal and may therefore be replaced by arg.
//
// The test is not simply "is the binding nulltype": a nested empty literal
// binds the variable to list[nulltype] ("[[]] < [[1]]" binds T to
// list[nulltype], then meets list[int]), so matching list layers are peeled off
// both sides until the bound side bottoms out at nulltype. Requiring arg to
// have a list layer wherever bound has one is what keeps this from being a
// license to rebind anything: "[[]] < [1]" leaves bound at list[nulltype] with
// arg at int, which is NOT convertible and stays inadmissible — otherwise the
// match would succeed and then fail in coerce(), turning a clean "unsupported
// operand types" into a coercion error.
func emptyListBinding(bound, arg Type) bool {
	if bound.Code == CodeNull {
		return true
	}
	boundElem, boundIsList := listElem(bound)
	argElem, argIsList := listElem(arg)
	if !boundIsList || !argIsList {
		return false
	}
	return emptyListBinding(boundElem, argElem)
}

// argCostList scores a list[T]-shaped argument against a list[T]-shaped
// parameter, both already confirmed single-parameter list types by argCost's
// caller. Split out to keep argCost itself under the repo's complexity cap.
func argCostList(param, arg Type, b bindings, pr promotion) (int, bool) {
	// list[nulltype] is the empty-list literal's type (section 1.2.3) and is
	// compatible with any list type — promotable's own list branch already
	// says so — but descending elementwise below would instead ask whether
	// nulltype itself reaches the element type, which it never does
	// (isScalarCode excludes it deliberately: null coerces to nothing). That
	// seam is inert while B1 has no list shapes to match against, but the
	// first list[T] parameter B2 registers would reject "[]" outright
	// without this.
	if arg.Params[0].Code == CodeNull {
		// When the parameter's element is itself a type variable (B2's
		// concatLists/repeatList shapes), it must still bind — otherwise
		// callShape later substitutes the UNBOUND variable into the param
		// type and coerces the empty-list argument to a bogus "list[T]"
		// value instead of leaving it list[nulltype], corrupting whatever
		// the shape's Fn inspects. Binding to nulltype is exactly what a
		// normal argCost(varT, nulltype, b) call would have done; only the
		// costWiden override (nulltype is a WIDENING match, not exact) is
		// special about this branch. A variable already bound by another
		// occurrence is left alone: the empty list is compatible with
		// whatever that binding resolved to, per the same empty-list rule.
		if elem := param.Params[0]; isTypeVar(elem.Code) {
			if _, bound := b[elem.Code]; !bound {
				b[elem.Code] = TNull
			}
		}
		return costWiden, true
	}
	return argCost(param.Params[0], arg.Params[0], b, pr)
}

// unionArgValueCost scores a union-typed ARGUMENT against a (non-union)
// parameter: the dual of unionArgCost just above, which scores a union
// PARAMETER. Every member is scored, not just the cheapest, because the
// union's actual runtime value could be any one of them; the argument is
// admissible only if all of them are, at the cost of the most expensive.
//
// Each member is scored against its own scratch copy of the bindings
// accumulated so far, exactly as unionArgCost's members are. But unlike
// unionArgCost, every member's bindings must survive, not just a winner's:
// if two members would bind the same type variable to different types there
// is no single binding that is correct regardless of which member shows up
// at runtime, so the whole argument is inadmissible rather than picking one
// arbitrarily. Bindings the members agree on are merged back into b.
func unionArgValueCost(param, arg Type, b bindings, pr promotion) (int, bool) {
	worst := 0
	merged := make(bindings, len(b))
	maps.Copy(merged, b)
	for _, member := range arg.Params {
		scratch := make(bindings, len(b))
		maps.Copy(scratch, b)
		cost, ok := argCost(param, member, scratch, pr)
		if !ok {
			return 0, false
		}
		if cost > worst {
			worst = cost
		}
		for code, t := range scratch {
			if existing, seen := merged[code]; seen {
				if !existing.Equal(t) {
					return 0, false
				}
				continue
			}
			merged[code] = t
		}
	}
	maps.Copy(b, merged)
	return worst, true
}

// unionArgCost scores arg against every member of a union parameter and keeps
// the lowest-cost admissible member; the argument is inadmissible only if no
// member is.
//
// Each member is tried against its own scratch copy of the bindings
// accumulated so far, not the caller's b directly: trying member two must not
// see a variable binding left behind by a failed or losing attempt at member
// one. Only the winning member's bindings are folded back into b, so a type
// variable never ends up bound to a type from a member that did not win.
func unionArgCost(param, arg Type, b bindings, pr promotion) (int, bool) {
	best := -1
	var bestBindings bindings
	for _, member := range param.Params {
		scratch := make(bindings, len(b))
		maps.Copy(scratch, b)
		cost, ok := argCost(member, arg, scratch, pr)
		if !ok {
			continue
		}
		if best < 0 || cost < best {
			best, bestBindings = cost, scratch
		}
	}
	if best < 0 {
		return 0, false
	}
	maps.Copy(b, bestBindings)
	return best, true
}

// isTypeVar reports whether c is one of the type-variable codes, which appear
// only in signatures and never as the type of a value.
func isTypeVar(c Code) bool {
	switch c {
	case CodeVarT, CodeVarT1, CodeVarT2, CodeVarT3:
		return true
	}
	return false
}

// substitute replaces every bound type variable in t with the type it bound to,
// rebuilding through the constructors so the result stays normalized. An unbound
// variable is left as it is.
func substitute(t Type, b bindings) Type {
	if isTypeVar(t.Code) {
		if bound, ok := b[t.Code]; ok {
			return bound
		}
		return t
	}
	if len(t.Params) == 0 {
		return t
	}
	params := make([]Type, len(t.Params))
	for i, p := range t.Params {
		params[i] = substitute(p, b)
	}
	switch t.Code {
	case CodeList:
		return ListOf(params[0])
	case CodeUnion:
		return UnionOf(params...)
	case CodeUnresolved:
		return UnresolvedOf(params[0])
	}
	return Type{Code: t.Code, Params: params}
}
