// SPDX-License-Identifier: AGPL-3.0-or-later

package fmtres

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// White-box test for the path flavor in ExprEvalOptions: expres_test.go's
// TestExprEvalOptions_CarriesNativePathFlavor (package fmtres_test) can only
// build its expectation through expr.WithPathFormat(expr.PathNative), which
// resolves to expr.PathPOSIX on every POSIX host, the same resolution
// expr.Eval's unconfigured default uses. As that test's doc comment states,
// removing expr.WithPathFormat(pathFlavor) from ExprEvalOptions gives
// identical output on such a host, so it cannot prove the call is there.
//
// This test bypasses pathFlavor's runtime.GOOS-driven resolution:
// exprEvalOptionsFor takes the flavor as a parameter, so the test can force
// expr.PathWindows on any host, independent of exprsyms.go's pathFlavor
// constant.
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
