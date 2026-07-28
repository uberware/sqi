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
	// Fn computes the result. It is called only with arguments that matched
	// Params, so it may use the payload accessors without checking.
	Fn func(args []Value) (Value, error)
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
		cost, ok := argCost(s.Params[i], args[i], b)
		if !ok {
			return 0, false
		}
		total += cost
	}
	return total, true
}

// argCost reports what it costs to pass an argument of type arg to a parameter
// declared as param, and whether it is admissible at all.
func argCost(param, arg Type, b bindings) (int, bool) {
	// A placeholder is matched on its constraint. Its lack of a value is
	// irrelevant to selecting a signature — which is what lets an expression be
	// type-checked before any parameter value exists.
	if arg.Code == CodeUnresolved && len(arg.Params) == 1 {
		return argCost(param, arg.Params[0], b)
	}
	if isTypeVar(param.Code) {
		// A variable binds once: seeing it a second time requires the same type.
		if bound, ok := b[param.Code]; ok {
			if !bound.Equal(arg) {
				return 0, false
			}
			return costExact, true
		}
		b[param.Code] = arg
		return costExact, true
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
		return argCost(param.Params[0], arg.Params[0], b)
	}
	// A union parameter is scored member-wise, before the exact-match fallback
	// below: the union as a whole is never Equal to one of its own members, so
	// an exact member match (int against int | float | string) would otherwise
	// be missed entirely. This is also more precise than testing membership by
	// type code, which could not tell list[int] from list[string] inside the
	// union — argCost's own list-descent handles that correctly per member.
	if param.Code == CodeUnion {
		return unionArgCost(param, arg, b)
	}
	if param.Equal(arg) {
		return costExact, true
	}
	if losslesslyCoercible(arg, param) {
		return costWiden, true
	}
	return 0, false
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
func unionArgCost(param, arg Type, b bindings) (int, bool) {
	best := -1
	var bestBindings bindings
	for _, member := range param.Params {
		scratch := make(bindings, len(b))
		maps.Copy(scratch, b)
		cost, ok := argCost(member, arg, scratch)
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
