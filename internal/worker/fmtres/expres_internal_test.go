// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// White-box test for FIX ROUND 2, item A: expres_test.go's
// TestExprEvalOptions_CarriesNativePathFlavor (package fmtres_test) can only
// build its expectation through expr.WithPathFormat(expr.PathNative), which
// resolves to expr.PathPOSIX on every host this suite runs on -- the SAME
// resolution expr.Eval's own unconfigured default uses. That test's own doc
// comment discloses, honestly, that deleting expr.WithPathFormat(pathFlavor)
// from ExprEvalOptions produces IDENTICAL output on this host either way, so
// it cannot behaviorally prove the call survives.
//
// This test closes that gap by going around pathFlavor's runtime.GOOS-driven
// resolution entirely: exprEvalOptionsFor takes the flavor as a direct
// parameter, so this test can force expr.PathWindows regardless of what host
// it runs on, independent of exprsyms.go's own pathFlavor constant.
func TestExprEvalOptionsFor_CarriesRequestedPathFlavor(t *testing.T) {
	opts := exprEvalOptionsFor(ExprLimits{}, nil, expr.PathWindows)
	v, err := expr.Eval("path('/a/b') / 'c'", expr.MapSymbols{}, expr.TString, opts...)
	if err != nil {
		t.Fatalf("expr.Eval: %v", err)
	}
	const want = `\a\b\c`
	if v.AsStr() != want {
		t.Fatalf("options do not carry the flavor: %q, want %q", v.AsStr(), want)
	}
}
