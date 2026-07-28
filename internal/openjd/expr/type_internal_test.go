// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "testing"

func TestType_Equal(t *testing.T) {
	tests := []struct {
		name string
		a, b Type
		want bool
	}{
		{"same scalar", TInt, TInt, true},
		{"different scalar", TInt, TFloat, false},
		{"scalar against a parameterized type", TInt, Type{Code: CodeList, Params: []Type{TInt}}, false},
		{
			name: "same element type",
			a:    Type{Code: CodeList, Params: []Type{TInt}},
			b:    Type{Code: CodeList, Params: []Type{TInt}},
			want: true,
		},
		{
			name: "different element type",
			a:    Type{Code: CodeList, Params: []Type{TInt}},
			b:    Type{Code: CodeList, Params: []Type{TString}},
			want: false,
		},
		{
			name: "nested element type",
			a:    Type{Code: CodeList, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			b:    Type{Code: CodeList, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			want: true,
		},
		{
			name: "same code but different parameter count",
			a:    Type{Code: CodeUnion, Params: []Type{TInt, TString}},
			b:    Type{Code: CodeUnion, Params: []Type{TInt}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("%v.Equal(%v) = %v; want %v", tt.a.Code, tt.b.Code, got, tt.want)
			}
			if got := tt.b.Equal(tt.a); got != tt.want {
				t.Errorf("Equal is not symmetric for %v and %v", tt.a.Code, tt.b.Code)
			}
		})
	}
}

func TestCode_String(t *testing.T) {
	// Every code needs a name: the names appear in "unsupported operand types"
	// messages, so a missing one renders a defect as "unknown type".
	codes := []Code{
		CodeNull, CodeBool, CodeInt, CodeFloat, CodeString, CodePath,
		CodeRangeExpr, CodeList, CodeUnion, CodeUnresolved, CodeAny,
		CodeNoReturn, CodeVarT, CodeVarT1, CodeVarT2, CodeVarT3,
	}
	for _, c := range codes {
		if _, ok := codeNames[c]; !ok {
			t.Errorf("code %d has no name in codeNames", int(c))
		}
	}
	if len(codeNames) != len(codes) {
		t.Errorf("codeNames has %d entries; want %d — a code was added without a test case", len(codeNames), len(codes))
	}
}

func TestType_String(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
		want string
	}{
		{"scalar", TInt, "int"},
		{"null", TNull, "nulltype"},
		{"any", TAny, "any"},
		{"noreturn", TNoReturn, "noreturn"},
		{"range expression", TRangeExpr, "range_expr"},
		{"list", Type{Code: CodeList, Params: []Type{TString}}, "list[string]"},
		{
			name: "nested list",
			typ:  Type{Code: CodeList, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			want: "list[list[int]]",
		},
		{
			name: "two member union renders as a union",
			typ:  Type{Code: CodeUnion, Params: []Type{TInt, TString}},
			want: "int | string",
		},
		{
			name: "optional is the two member union ending in nulltype",
			typ:  Type{Code: CodeUnion, Params: []Type{TInt, TNull}},
			want: "int?",
		},
		{
			name: "optional list",
			typ:  Type{Code: CodeUnion, Params: []Type{{Code: CodeList, Params: []Type{TPath}}, TNull}},
			want: "list[path]?",
		},
		{
			name: "three member union with null spells nulltype out",
			typ:  Type{Code: CodeUnion, Params: []Type{TInt, TString, TNull}},
			want: "int | string | nulltype",
		},
		{"unresolved with a constraint", Type{Code: CodeUnresolved, Params: []Type{TInt}}, "unresolved[int]"},
		{"unresolved of any is bare", Type{Code: CodeUnresolved, Params: []Type{TAny}}, "unresolved"},
		{
			name: "unresolved list",
			typ:  Type{Code: CodeUnresolved, Params: []Type{{Code: CodeList, Params: []Type{TInt}}}},
			want: "unresolved[list[int]]",
		},
		{"type variable", Type{Code: CodeVarT}, "T"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.typ.String(); got != tt.want {
				t.Errorf("String() = %q; want %q", got, tt.want)
			}
		})
	}
}
