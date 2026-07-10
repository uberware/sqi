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

// TestExpand_TaskCountExplosionRejected verifies the resource-exhaustion guard:
// a Cartesian product whose task count exceeds the per-step cap is rejected
// BEFORE materialization, rather than allocating ~1M task rows.
func TestExpand_TaskCountExplosionRejected(t *testing.T) {
	// 1024 * 1024 = 1,048,576 tasks, just over the 1,000,000 cap.
	a, b := "1-1024", "1-1024"
	prod := "A*B"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeInt, RangeExpr: &a},
			{Name: "B", Type: openjd.TaskParamTypeInt, RangeExpr: &b},
		},
		Combination: &prod,
	}
	if _, err := openjd.ExpandParameterSpace(ps); err == nil {
		t.Fatal("expected error for task-count explosion (1024*1024 > cap), got nil")
	}
}

// TestExpand_TaskCountGuardRespectsZip verifies the guard counts via the
// combination tree, not a naive product: zipping two 1024-value parameters
// yields 1024 tasks (not ~1M), so it must be accepted.
func TestExpand_TaskCountGuardRespectsZip(t *testing.T) {
	a, b := "1-1024", "1-1024"
	zip := "(A,B)"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeInt, RangeExpr: &a},
			{Name: "B", Type: openjd.TaskParamTypeInt, RangeExpr: &b},
		},
		Combination: &zip,
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("zip of two 1024-value params should be accepted (1024 tasks), got error: %v", err)
	}
	if len(rows) != 1024 {
		t.Fatalf("zip expansion produced %d tasks, want 1024", len(rows))
	}
}

func TestDeriveChunkBounds(t *testing.T) {
	strp := func(s string) *string { return &s }
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name: "Frame", Type: openjd.TaskParamTypeChunkInt, RangeExpr: strp("1-10"),
				Chunks: &openjd.TaskChunks{DefaultTaskCount: 5, RangeConstraint: "CONTIGUOUS"},
			},
		},
	}
	rows := []openjd.TaskParams{
		{"Frame": "1-5"},
		{"Frame": "6-10"},
		{"Frame": "7"},
		{"Frame": "1-10:2"}, // 1,3,5,7,9 → Start 1, End 9
	}
	openjd.DeriveChunkBounds(rows, ps)

	want := []struct{ start, end string }{
		{"1", "5"}, {"6", "10"}, {"7", "7"}, {"1", "9"},
	}
	for i, w := range want {
		if got := rows[i]["Frame.Start"]; got != w.start {
			t.Errorf("row %d Frame.Start = %q, want %q", i, got, w.start)
		}
		if got := rows[i]["Frame.End"]; got != w.end {
			t.Errorf("row %d Frame.End = %q, want %q", i, got, w.end)
		}
	}
}

func TestDeriveChunkBounds_NilAndNonChunk(t *testing.T) {
	// nil ps is a no-op.
	openjd.DeriveChunkBounds([]openjd.TaskParams{{"X": "1"}}, nil)
	// A non-chunk param gets no derived keys.
	ps := &openjd.StepParameterSpace{TaskParameterDefinitions: []openjd.TaskParamDefinition{
		{Name: "X", Type: openjd.TaskParamTypeInt, RangeList: []string{"1"}},
	}}
	rows := []openjd.TaskParams{{"X": "1"}}
	openjd.DeriveChunkBounds(rows, ps)
	if _, ok := rows[0]["X.Start"]; ok {
		t.Error("non-chunk param should not get .Start")
	}
}
