// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// TestBindJobParameters_RangeExprIsCanonical pins Template Schemas
// §3.4.1.1.1 as RFC 0007's RANGE_EXPR parameter uses it: a range expression
// value is its list of values, written in its canonical form wherever it is
// written as a string, so the value bound for a RANGE_EXPR parameter, whether
// given at submission or taken from its default, is canonical.
func TestBindJobParameters_RangeExprIsCanonical(t *testing.T) {
	def := "1,2,3,10-15:2"
	params := []openjd.JobParameter{
		{Name: "Given", Type: openjd.JobParamTypeRangeExpr},
		{Name: "Default", Type: openjd.JobParamTypeRangeExpr, Default: &def},
	}
	got, errs := openjd.BindJobParameters(params, map[string]string{"Given": "10-15:2,1-5"})
	if len(errs) != 0 {
		t.Fatalf("BindJobParameters() errors = %v", errs)
	}
	if got["Given"] != "1-5,10-14:2" || got["Default"] != "1-3,10-14:2" {
		t.Errorf("bound = %q, want Given 1-5,10-14:2 and Default 1-3,10-14:2", got)
	}
}
