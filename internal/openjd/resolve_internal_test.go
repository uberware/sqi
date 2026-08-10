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
// EXPR sub-project E4b Task 2's fix-round review found the original
// implementation called v.AsStr() there. That was unreachable in production
// TODAY only because every current caller happens to pass a target that
// either evaluates a string (resolveRangeListEntry's TargetString) or never
// reaches the lone branch at all for the target it passes
// (resolveRangeExprField's non-lone RangeExpr call always supplies a raw
// value fmtstring.LoneRef has already rejected as non-lone). But loneTarget
// is a caller-supplied PARAMETER, not a constant baked into this function —
// it exists precisely so a future caller can vary it (design spec §3's
// per-element-type RangeList target, not yet implemented — see
// resolveRangeListEntry's own doc comment), and a parameter that exists to
// be varied must not crash the process — inside a synchronous submit
// handler, no less — the first time it is. Value.String() is total over
// every Value and is already what evalRangeExprList's elements use; this
// test pins that resolveFormatStringExpr never regresses back to AsStr, for
// every scalar/list shape a target might plausibly produce.
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
