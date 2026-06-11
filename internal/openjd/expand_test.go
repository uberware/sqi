// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Additional expand tests — item 7 of the test roadmap.
//
// openjd_test.go already covers nil, INT/FLOAT/STRING ranges, Cartesian
// product, zip, zip mismatch, undeclared param, and chunk expansion.
// This file adds the one gap: a syntactically malformed combination expression.

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestExpand_MalformedCombinationExpr(t *testing.T) {
	// "A**B" has two consecutive stars — the parser should return an error
	// rather than panicking or silently producing wrong results.
	comb := "A**B"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeString, RangeList: []string{"a1"}},
			{Name: "B", Type: openjd.TaskParamTypeString, RangeList: []string{"b1"}},
		},
		Combination: &comb,
	}
	_, err := openjd.ExpandParameterSpace(ps)
	if err == nil {
		t.Fatal("expected error for malformed combination expression 'A**B', got nil")
	}
}
