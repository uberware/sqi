// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "sort"

// Code is the base of a Type: which type constructor it is, independent of any
// type parameters. Section 1.2.1 lists the language's types; this is the
// discriminant for all of them.
type Code int

// The type codes. Params carries the type parameters each one takes: none for
// the scalars, an element type for CodeList, the members for CodeUnion, and the
// constraint for CodeUnresolved.
const (
	CodeNull Code = iota // nulltype
	CodeBool
	CodeInt
	CodeFloat
	CodeString
	CodePath
	CodeRangeExpr
	CodeList       // Params[0] is the element type
	CodeUnion      // Params are the members
	CodeUnresolved // Params[0] is the constraint
	CodeAny
	CodeNoReturn
	CodeVarT // type variables: function signatures only, never a concrete value
	CodeVarT1
	CodeVarT2
	CodeVarT3
)

// codeNames renders a code. These names appear in error messages, so every code
// needs one; an unnamed code would report a defect as "unknown type".
var codeNames = map[Code]string{
	CodeNull:       "nulltype",
	CodeBool:       "bool",
	CodeInt:        "int",
	CodeFloat:      "float",
	CodeString:     "string",
	CodePath:       "path",
	CodeRangeExpr:  "range_expr",
	CodeList:       "list",
	CodeUnion:      "union",
	CodeUnresolved: "unresolved",
	CodeAny:        "any",
	CodeNoReturn:   "noreturn",
	CodeVarT:       "T",
	CodeVarT1:      "T1",
	CodeVarT2:      "T2",
	CodeVarT3:      "T3",
}

func (c Code) String() string {
	if name, ok := codeNames[c]; ok {
		return name
	}
	return "unknown type"
}

// Type is a type in the expression language: a code plus its type parameters.
//
// Because Params is a slice, Type is NOT comparable — "==" on two Types is a
// compile error and a struct containing a Type cannot be a Go map key. Use
// Equal. That single fact is why operator dispatch is a list of shapes
// (shape.go) rather than the map keyed on kinds that sub-project A used.
//
// Construct Types through the constructors below rather than as literals: they
// normalize, and everything downstream relies on structural equality being
// sufficient, which only holds for normalized types.
type Type struct {
	// Code is which type constructor this is. Always meaningful.
	Code Code
	// Params are the type parameters, whose meaning depends on Code. Empty for
	// the scalars.
	Params []Type
}

// The predeclared types that take no parameters.
var (
	TNull      = Type{Code: CodeNull}
	TBool      = Type{Code: CodeBool}
	TInt       = Type{Code: CodeInt}
	TFloat     = Type{Code: CodeFloat}
	TString    = Type{Code: CodeString}
	TPath      = Type{Code: CodePath}
	TRangeExpr = Type{Code: CodeRangeExpr}
	TAny       = Type{Code: CodeAny}
	TNoReturn  = Type{Code: CodeNoReturn}
)

// Equal reports whether two types are structurally identical.
//
// For normalized types this is also semantic equality — that is the property
// the constructors exist to guarantee, and it is what lets dispatch, coercion
// and the conformance path avoid a semantic type comparison entirely.
func (t Type) Equal(o Type) bool {
	if t.Code != o.Code || len(t.Params) != len(o.Params) {
		return false
	}
	for i := range t.Params {
		if !t.Params[i].Equal(o.Params[i]) {
			return false
		}
	}
	return true
}

// String renders the type in the spec's own notation, and is the exact inverse
// of ParseType. Type errors quote it, declared parameter types arrive as it, and
// UnionOf orders a union's members by it.
func (t Type) String() string {
	switch t.Code {
	case CodeList:
		return "list[" + t.paramString(0) + "]"
	case CodeUnresolved:
		// The spec's shorthand: an unconstrained unresolved value is written
		// "unresolved", not "unresolved[any]".
		if len(t.Params) == 1 && t.Params[0].Code == CodeAny {
			return "unresolved"
		}
		return "unresolved[" + t.paramString(0) + "]"
	case CodeUnion:
		return t.unionString()
	}
	return t.Code.String()
}

// paramString renders the i'th type parameter, or a placeholder if a hand-built
// Type is missing it. A malformed Type is a bug, but rendering must not panic
// inside an error message.
func (t Type) paramString(i int) string {
	if i >= len(t.Params) {
		return "?"
	}
	return t.Params[i].String()
}

// unionString renders a union. A two-member union ending in nulltype is the
// optional shorthand "T?" — and because UnionOf sorts nulltype last, that is the
// only place it can appear. A longer union containing nulltype is spelled out in
// full: the spec defines no shorthand for it, and inventing one would break the
// round trip with ParseType.
func (t Type) unionString() string {
	if len(t.Params) == 2 && t.Params[1].Code == CodeNull {
		return t.Params[0].String() + "?"
	}
	out := make([]byte, 0, 16*len(t.Params))
	for i := range t.Params {
		if i > 0 {
			out = append(out, " | "...)
		}
		out = append(out, t.Params[i].String()...)
	}
	return string(out)
}

// UnionOf builds a union type, applying every normalization rule the ExprType
// section requires: nested unions flatten, noreturn collapses to nothing, any
// absorbs everything, duplicates are removed, members sort with nulltype last,
// a single member unwraps, and an unresolved member hoists to wrap the whole
// union.
//
// Normalizing here rather than at each use site is what makes Equal sufficient
// downstream. Dispatch, coercion and the conformance path all rely on two types
// that mean the same thing having the same shape.
func UnionOf(ts ...Type) Type {
	var members []Type
	unresolved := false
	for _, t := range ts {
		collectUnionMembers(t, &members, &unresolved)
	}
	out := normalizeUnionMembers(members)
	// Re-wrap last, not first: absorbing "any" before every member has been
	// seen would lose an unresolved member that comes after it.
	if unresolved {
		return UnresolvedOf(out)
	}
	return out
}

// collectUnionMembers appends t's contribution to a union's member list,
// flattening nested unions, dropping noreturn, and stripping an unresolved
// wrapper while recording that the union as a whole is unresolved. It recurses
// so that a hand-built Type cannot smuggle a nested union past normalization.
func collectUnionMembers(t Type, out *[]Type, unresolved *bool) {
	switch t.Code {
	case CodeUnresolved:
		*unresolved = true
		if len(t.Params) == 1 {
			collectUnionMembers(t.Params[0], out, unresolved)
		}
	case CodeUnion:
		for _, p := range t.Params {
			collectUnionMembers(p, out, unresolved)
		}
	case CodeNoReturn:
		// Collapses to nothing, so that "x if c else fail()" is x, not x?.
	default:
		*out = append(*out, t)
	}
}

// normalizeUnionMembers applies the rules that do not involve unresolved: any
// absorbs, duplicates collapse, members sort with nulltype last, and zero or one
// member is not a union at all.
func normalizeUnionMembers(members []Type) Type {
	for _, m := range members {
		if m.Code == CodeAny {
			return TAny
		}
	}

	uniq := make([]Type, 0, len(members))
	for _, m := range members {
		if !containsType(uniq, m) {
			uniq = append(uniq, m)
		}
	}

	switch len(uniq) {
	case 0:
		// Every member collapsed, which can only mean they were all noreturn.
		return TNoReturn
	case 1:
		return uniq[0]
	}

	sort.Slice(uniq, func(i, j int) bool { return unionLess(uniq[i], uniq[j]) })
	return Type{Code: CodeUnion, Params: uniq}
}

// containsType reports whether ts already holds a type equal to t.
func containsType(ts []Type, t Type) bool {
	for _, x := range ts {
		if x.Equal(t) {
			return true
		}
	}
	return false
}

// unionLess is the canonical member order: nulltype last, everything else
// alphabetically by rendered name. Sorting by the rendering rather than by code
// keeps the order stable as codes are added, and makes list members order
// predictably against each other.
func unionLess(a, b Type) bool {
	if an, bn := a.Code == CodeNull, b.Code == CodeNull; an != bn {
		return bn
	}
	return a.String() < b.String()
}

// UnresolvedOf builds the type of a value whose type is known but whose value is
// not, per the spec's static-type-checking model. Nested unresolved types
// flatten, so unresolved is idempotent.
//
// The value-level counterpart is Unresolved, which builds a Value; the -Of
// suffix on the type constructors keeps the two apart.
func UnresolvedOf(t Type) Type {
	if t.Code == CodeUnresolved {
		return t
	}
	return Type{Code: CodeUnresolved, Params: []Type{t}}
}

// ListOf builds a list type. An unresolved element type hoists outward — a list
// of unresolved elements is an unresolved list — which keeps unresolved as the
// outermost constructor, the canonical form the ExprType section requires.
//
// Section 1.2.1's constraints on the element type (no third level of nesting, no
// optional element) are enforced by ParseType, where untrusted text enters.
// Internal callers pass programmer-controlled types, so an error return here
// would add an unfailing check to every call site.
func ListOf(elem Type) Type {
	if elem.Code == CodeUnresolved {
		return UnresolvedOf(ListOf(elem.Params[0]))
	}
	return Type{Code: CodeList, Params: []Type{elem}}
}

// OptionalOf builds the type of a value that may also be null. The spec has no
// separate optional code: "int?" is exactly the union of int and nulltype, and
// UnionOf's normalization means applying this twice changes nothing.
func OptionalOf(t Type) Type { return UnionOf(t, TNull) }
