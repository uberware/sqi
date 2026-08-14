// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"errors"
	"testing"
	"time"
)

// TestLenRangeExpr_DoesNotMaterialize pins section 8.2 of the E1 design.
//
// len(range_expr('1-20000000')) has an arithmetic answer that intrange already
// computes without allocating anything. Before this fix it expanded the range to
// count it and failed the element bound, refusing a legitimate query. The
// reference answers the same shape of query in 2 operations, which is direct
// evidence it does not expand either.
func TestLenRangeExpr_DoesNotMaterialize(t *testing.T) {
	v, err := Eval("len(range_expr('1-20000000'))", nil, TInt)
	if err != nil {
		t.Fatalf("len over a large range: %v", err)
	}
	if v.AsInt() != 20_000_000 {
		t.Errorf("= %d; want 20000000", v.AsInt())
	}
}

func TestLenRangeExpr_SmallCasesUnchanged(t *testing.T) {
	tests := []struct {
		src  string
		want int64
	}{
		{"len(range_expr('1-10'))", 10},
		{"len(range_expr('1-10:2'))", 5},
		{"len(range_expr('1-5,10'))", 6},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			v, err := Eval(tt.src, nil, TInt)
			if err != nil {
				t.Fatalf("%s: %v", tt.src, err)
			}
			if v.AsInt() != tt.want {
				t.Errorf("%s = %d; want %d", tt.src, v.AsInt(), tt.want)
			}
		})
	}
}

// TestUnique_IsBoundedByTheOperationLimit is the test C1 asked for by name.
//
// unique's scan is O(n^2) in valuesEqual calls. This asserts that a large input
// fails with errOperationLimit in bounded time rather than hanging. The deadline
// is what makes it a real test: without it, a hang looks like a slow pass.
//
// THE DEADLINE IS DELIBERATELY GENEROUS, and was raised from 30s after CI
// failed on it. What the test distinguishes is "bounded" from "hangs forever",
// and a hang is unbounded — so any finite deadline serves, while a tight one
// only adds a way to fail for reasons that have nothing to do with the bound.
// The operation limit lets ~10 million valuesEqual calls run before it fires;
// that is ~2s locally under -race and was over 30s on a loaded CI runner. Do
// not tighten this back down to "make the test faster": it does not run long
// unless something is already wrong, because the limit stops it either way.
func TestUnique_IsBoundedByTheOperationLimit(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		// 20000 distinct elements is 2*10^8 comparisons at O(n^2) -- far past
		// the 10-million default operation limit, and far under maxElements, so
		// the floor cannot be what stops it.
		_, err := Eval("unique(range(20000))", nil, TAny)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errOperationLimit) {
			t.Fatalf("unique over 20000 elements = %v; want errOperationLimit", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("unique over 20000 elements did not return within 2m: the operation limit " +
			"is not counting the quadratic work, only the linear input")
	}
}

func TestUnique_SmallCasesUnchanged(t *testing.T) {
	v, err := Eval("unique([1, 1, 2, 3, 2])", nil, TAny)
	if err != nil {
		t.Fatalf("unique: %v", err)
	}
	if got := v.String(); got != "[1, 2, 3]" {
		t.Errorf("unique([1,1,2,3,2]) = %s; want [1, 2, 3]", got)
	}
}
