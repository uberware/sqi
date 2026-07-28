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
