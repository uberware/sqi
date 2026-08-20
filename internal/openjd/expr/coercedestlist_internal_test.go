// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestCoerceListDestinations pins the list half of RFC 0005's destination
// table, as restated by openjd-specifications#175:
//
//	| list[S] | list destinations in S's order, applied to their element types |
//
// and, for the empty list, "the nominal element type it carries follows the
// first list destination in the union's normalized member order" -- an order
// the RFC defines outright (type parameters sorted alphabetically, nulltype
// last), so it is a rule and not an implementation detail.
//
// The old reading had no list-destination ORDER because it had no notion of
// more than one list destination: listElem() reports a target naming two
// different list types as not list-shaped at all, which is why the cases below
// either failed or, worse, passed the value through unconverted.
func TestCoerceListDestinations(t *testing.T) {
	tests := []struct {
		name   string
		value  Value
		target string
		want   string
		wantTy string
	}{
		{
			name:   "list[float] tries list[int] first, because int leads float's own order",
			value:  List(TFloat, []Value{Float(1.0), Float(2.0)}),
			target: "list[int] | list[string]",
			want:   "[1, 2]", wantTy: "list[int]",
		},
		{
			name:   "a fractional element fails list[int] and the next destination takes it",
			value:  List(TFloat, []Value{Float(1.5)}),
			target: "list[int] | list[string]",
			want:   `["1.5"]`, wantTy: "list[string]",
		},
		{
			name:   "a failed list destination is not an error when a later one converts",
			value:  List(TString, []Value{String("1"), String("x")}),
			target: "list[int] | list[path]",
			want:   `["1", "x"]`, wantTy: "list[path]",
		},
		{
			name:   "a single list destination still converts elementwise",
			value:  List(TInt, []Value{Int(1), Int(2)}),
			target: "list[string]",
			want:   `["1", "2"]`, wantTy: "list[string]",
		},
		{
			name:   "the empty list takes the first list destination in normalized order",
			value:  List(TNull, nil),
			target: "list[string] | list[int]",
			want:   "[]", wantTy: "list[int]",
		},
		{
			name:   "no list destination converts, so the coercion fails",
			value:  List(TString, []Value{String("x")}),
			target: "list[int]",
			want:   "", wantTy: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseType(tt.target)
			if err != nil {
				t.Fatalf("ParseType(%q) = %v", tt.target, err)
			}
			got, err := coerce(tt.value, target)
			if tt.wantTy == "" {
				if err == nil {
					t.Fatalf("coerce(%s, %s) = %s : %s; want an error", tt.value, tt.target, got, got.Type)
				}
				return
			}
			if err != nil {
				t.Fatalf("coerce(%s, %s) = %v; want %s : %s", tt.value, tt.target, err, tt.want, tt.wantTy)
			}
			if got.String() != tt.want || got.Type.String() != tt.wantTy {
				t.Errorf("coerce(%s, %s) = %s : %s; want %s : %s",
					tt.value, tt.target, got, got.Type, tt.want, tt.wantTy)
			}
		})
	}
}

// TestCoerceRejectsUnusableDestinations pins RFC 0005's exclusion list: "a type
// variable, noreturn, unresolved[T], or a list parameterized by any of those
// contributes no destination, so a target composed only of such types cannot be
// coerced to at all."
//
// The list[T1] row is the one the specification states as a MUST, and it says
// why: the symmetric matching that binds type variables during signature
// matching would accept such a target by binding T1 and then discarding the
// binding. Satisfaction is directional and must not be used for that.
func TestCoerceRejectsUnusableDestinations(t *testing.T) {
	tests := []struct {
		name   string
		value  Value
		target Type
	}{
		{"a bare type variable offers nothing", Int(1), Type{Code: CodeVarT1}},
		{"noreturn offers nothing", Int(1), TNoReturn},
		{"a list of a type variable offers nothing", List(TInt, []Value{Int(1)}), ListOf(Type{Code: CodeVarT1})},
		{"a union of only unusable types offers nothing", Int(1), UnionOf(TNoReturn, Type{Code: CodeVarT})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := coerce(tt.value, tt.target)
			if err == nil {
				t.Fatalf("coerce(%s, %s) = %s : %s; want an error", tt.value, tt.target, got, got.Type)
			}
		})
	}
}

// TestCoerceUnresolvedNarrowing pins the constraint a PLACEHOLDER carries away
// from a coercion, which #175 restated: "conversion narrows to the union of
// every destination with a type-level rule, rather than betting on any one of
// them -- anything narrower would misdescribe some resolved value."
//
// The invariant behind it, stated by the RFC and worth keeping in mind when
// reading these cases: the narrowed constraint always satisfies the target, AND
// the concrete result's type always satisfies the narrowed constraint. Narrowing
// unresolved[float] against "int | string" to unresolved[int] would break the
// second half, because a 3.5 payload fails float->int and lands on string.
func TestCoerceUnresolvedNarrowing(t *testing.T) {
	tests := []struct {
		name       string
		constraint Type
		target     string
		want       string
	}{
		{
			name:       "satisfaction keeps the source constraint",
			constraint: ListOf(TInt),
			target:     "list[any]",
			want:       "unresolved[list[int]]",
		},
		{
			name:       "a single destination narrows to exactly that type",
			constraint: TInt,
			target:     "string",
			want:       "unresolved[string]",
		},
		{
			name:       "two destinations narrow to their union, not to a guess",
			constraint: TFloat,
			target:     "int | string",
			want:       "unresolved[int | string]",
		},
		{
			name:       "range_expr against a list target narrows to what materializing produces",
			constraint: TRangeExpr,
			target:     "list[any]",
			want:       "unresolved[list[int]]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseType(tt.target)
			if err != nil {
				t.Fatalf("ParseType(%q) = %v", tt.target, err)
			}
			got, err := coerce(Unresolved(tt.constraint), target)
			if err != nil {
				t.Fatalf("coerce(unresolved[%s], %s) = %v; want %s", tt.constraint, tt.target, err, tt.want)
			}
			if got.Type.String() != tt.want {
				t.Errorf("coerce(unresolved[%s], %s) = %s; want %s",
					tt.constraint, tt.target, got.Type, tt.want)
			}
		})
	}
}
