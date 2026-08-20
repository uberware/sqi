// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

// TestCoerceDestinationOrder pins RFC 0005's "Implicit Type Coercion" as it was
// restated by openjd-specifications#175 (merged 2026-08-19): coercion asks
// SATISFACTION first, and only then converts toward the target's DESTINATIONS,
// in an order fixed by the result's type.
//
// The old wording had no answer when a target offered more than one candidate
// of the same kind, and sqi's reading of it -- singleScalarTarget, which gives
// up the moment two members disagree -- therefore REJECTED those targets
// outright. Every "converts" case below is one the merged text accepts and this
// package used to refuse, so this test is an acceptance-widening test: nothing
// sqi accepts today may start failing.
//
// Each case is either quoted from the merged section's own examples or derived
// from its destination-order table, and the "why" column says which.
func TestCoerceDestinationOrder(t *testing.T) {
	tests := []struct {
		name   string
		value  Value
		target string // ParseType, so the table reads as the specification does
		want   string // Value.String() of the result
		wantTy string // the result's type, so a pass-through is distinguishable
	}{
		// ── Satisfaction comes first, so an admitted type is never converted ──
		{
			name:   "int satisfies a union naming int and is not stringified",
			value:  Int(5),
			target: "int | string",
			want:   "5", wantTy: "int",
		},
		{
			name:   "list[int] satisfies list[any] by element satisfaction",
			value:  List(TInt, []Value{Int(1), Int(2)}),
			target: "list[any]",
			want:   "[1, 2]", wantTy: "list[int]",
		},
		{
			name:   "null satisfies an optional target",
			value:  Null(),
			target: "string?",
			want:   "null", wantTy: "nulltype",
		},

		// ── Conversion: the destination table, one row at a time ─────────────
		// int -> float, then string. The spec states this example verbatim.
		{
			name:   "int prefers float over string",
			value:  Int(5),
			target: "float | string",
			want:   "5.0", wantTy: "float",
		},
		// float -> int, then string. Both halves are the spec's own example.
		{
			name:   "a whole float takes the int destination",
			value:  Float(3.0),
			target: "int | string",
			want:   "3", wantTy: "int",
		},
		{
			name:   "a fractional float fails int and falls through to string",
			value:  Float(3.5),
			target: "int | string",
			want:   "3.5", wantTy: "string",
		},
		// string -> int, float, bool, range_expr, path. The first two are the
		// spec's example; the string's own lexical form routes it.
		{
			name:   "a string that parses as an int takes int before float",
			value:  String("5"),
			target: "int | float",
			want:   "5", wantTy: "int",
		},
		{
			name:   "a string that parses only as a float takes float",
			value:  String("5.0"),
			target: "int | float",
			want:   "5.0", wantTy: "float",
		},
		{
			name:   "a string reaches bool, a destination the old rules had no conversion for",
			value:  String("yes"),
			target: "bool",
			want:   "true", wantTy: "bool",
		},
		{
			name:   "a string reaches range_expr, also new",
			value:  String("1-5"),
			target: "range_expr",
			want:   "1-5", wantTy: "range_expr",
		},
		{
			name:   "bool and range_expr are tried before path",
			value:  String("1-5"),
			target: "bool | path | range_expr",
			want:   "1-5", wantTy: "range_expr",
		},
		{
			name:   "path is the universal fallback, so a word reaches it",
			value:  String("shot010"),
			target: "int | path",
			want:   "shot010", wantTy: "path",
		},

		// ── nulltype is never a destination ──────────────────────────────────
		{
			name:   "a string is not converted toward a nulltype member",
			value:  String("null"),
			target: "int | nulltype",
			want:   "", wantTy: "", // expected to fail
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
					t.Fatalf("coerce(%s, %s) = %s; want an error", tt.value, tt.target, got)
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
