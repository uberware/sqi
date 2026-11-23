// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// TestResolveFormatStringExpr_NonStringLoneTargetDoesNotPanic guards
// resolveFormatStringExpr's lone-reference branch against the panic
// Value.AsStr raises (Value.mustBe) for any payload code other than string.
//
// loneTarget is supplied by the caller: resolveRangeListEntry passes
// rangeExprElemType(typ), which is TInt, TFloat or TPath for an INT, FLOAT or
// PATH entry. Calling v.AsStr() on the lone branch would crash the process,
// inside a synchronous submit handler, for any non-string target.
// Value.String() is total over every Value and is what evalRangeExprField's
// elements use; this test pins that resolveFormatStringExpr uses it rather
// than AsStr, for each scalar and list shape a target can produce.
func TestResolveFormatStringExpr_NonStringLoneTargetDoesNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		target expr.Type
		want   string
	}{
		{name: "TInt target, lone int literal", body: "{{ 5 }}", target: expr.TInt, want: "5"},
		{name: "TFloat target, lone computed float", body: "{{ 2.5 * 2 }}", target: expr.TFloat, want: "5.0"},
		{name: "TPath target, lone string literal coerced to path", body: `{{ "a/b" }}`, target: expr.TPath, want: "a/b"},
		{name: "TAny target, lone list literal (no scalar coercion at all)", body: "{{ [1, 2] }}", target: expr.TAny, want: "[1, 2]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("resolveFormatStringExpr panicked: %v", r)
				}
			}()
			got, err := resolveFormatStringExpr(tc.body, expr.MapSymbols{}, tc.target)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveFormatStringExpr(%q, target=%s) = %q, want %q", tc.body, tc.target, got, tc.want)
			}
		})
	}
}
