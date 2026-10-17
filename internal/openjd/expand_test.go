// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Expansion tests beyond the basic cases in openjd_test.go (nil, INT/FLOAT/STRING
// ranges, Cartesian product, zip, zip mismatch, undeclared parameter): a
// malformed combination expression, the per-step task-count guard, chunk
// bounds, and how CONTIGUOUS and NONCONTIGUOUS chunks are spelled.

import (
	"math"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestExpand_MalformedCombinationExpr(t *testing.T) {
	// "A**B" has two consecutive stars; the parser must return an error.
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
// before its ~1M task rows are materialized.
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

// TestExpand_TaskCountGuardRespectsZip verifies the guard counts through the
// combination tree: zipping two 1024-value parameters yields 1024 tasks, so it
// must be accepted.
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

// TestExpand_ChunkContiguousNeverSpansAGap pins RFC 0001's definition of a
// CONTIGUOUS chunk: "always a contiguous range of integers", spelled
// "<start>-<end>" even for one frame. The first row is the RFC's example.
func TestExpand_ChunkContiguousNeverSpansAGap(t *testing.T) {
	tests := []struct {
		name       string
		rangeExpr  string
		chunkSize  int
		constraint string
		want       []string
	}{
		{
			// The RFC illustrates this as 18-27, 28-37, 38-47, 48-50; the
			// reference implementation balances the run instead.
			"RFC 0001 example", "1,10-12,18-50", 10, "CONTIGUOUS",
			[]string{"1-1", "10-12", "18-26", "27-34", "35-42", "43-50"},
		},
		{
			// Balanced: the fewest chunks of at most 5, with the values spread
			// evenly (contiguous-uneven.test.yaml).
			"uneven range is balanced", "1-12", 5, "CONTIGUOUS",
			[]string{"1-4", "5-8", "9-12"},
		},
		{
			// Chunk i ends at ceil((i+1)*n/k), so the sizes alternate
			// (3,2,3,2) instead of front-loading the remainder (3,3,2,2).
			"balanced sizes alternate", "1-10", 3, "CONTIGUOUS",
			[]string{"1-3", "4-5", "6-8", "9-10"},
		},
		{
			"stepped range is one frame per chunk", "1-9:2", 5, "CONTIGUOUS",
			[]string{"1-1", "3-3", "5-5", "7-7", "9-9"},
		},
		{
			"list with a run longer than the chunk", "1,5,10-20", 5, "CONTIGUOUS",
			[]string{"1-1", "5-5", "10-13", "14-17", "18-20"},
		},
		{
			"single frames are start-end", "1-3", 1, "CONTIGUOUS",
			[]string{"1-1", "2-2", "3-3"},
		},
		{
			"negative frames", "-3-2", 3, "CONTIGUOUS",
			[]string{"-3--1", "0-2"},
		},
		{
			"chunk larger than range", "1-5", 100, "CONTIGUOUS",
			[]string{"1-5"},
		},
		{
			// defaultTaskCount has no upper bound, so "everything in one task"
			// may be spelled as the largest INT; counting chunks must not
			// overflow into zero of them.
			"largest chunk size", "1-5", math.MaxInt, "CONTIGUOUS",
			[]string{"1-5"},
		},
		{
			"largest chunk size noncontiguous", "1-9:2", math.MaxInt, "NONCONTIGUOUS",
			[]string{"1-9:2"},
		},
		{
			"empty constraint defaults to contiguous", "1,3", 2, "",
			[]string{"1-1", "3-3"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ps := &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{{
					Name: "Frame", Type: openjd.TaskParamTypeChunkInt, RangeExpr: new(tc.rangeExpr),
					Chunks: &openjd.TaskChunks{DefaultTaskCount: tc.chunkSize, RangeConstraint: tc.constraint},
				}},
			}
			rows, err := openjd.ExpandParameterSpace(ps)
			if err != nil {
				t.Fatalf("ExpandParameterSpace: %v", err)
			}
			got := make([]string, len(rows))
			for i, r := range rows {
				got[i] = r["Frame"]
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("chunks = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExpand_ChunkNoncontiguousIsCompact pins the spelling of a NONCONTIGUOUS
// chunk. Every row but the last matches the output of the reference
// implementation (openjd-model's StepParameterSpaceIterator, at the pinned
// OPENJD_MODEL_VERSION); the first two are also the TASK_CHUNKING conformance
// fixtures noncontiguous.test.yaml and noncontiguous-format.test.yaml, which
// test/conformance scores. The last row has no reference equivalent: the
// reference orders a range's values increasingly where internal/openjd keeps
// them first-seen, so a chunk can descend, and a descent never forms a range.
func TestExpand_ChunkNoncontiguousIsCompact(t *testing.T) {
	tests := []struct {
		rangeExpr string
		chunkSize int
		want      []string
	}{
		{"1-10", 5, []string{"1-5", "6-10"}},
		{"1-8", 3, []string{"1-3", "4-6", "7,8"}},
		{"1-9:2", 3, []string{"1-5:2", "7,9"}},
		{"1-20:2", 10, []string{"1-19:2"}},
		{"5", 1, []string{"5"}},
		{"1,3", 2, []string{"1,3"}},
		{"1-3,10", 4, []string{"1-3,10"}},
		{"1-3,7-8", 5, []string{"1-3,7,8"}},
		{"1,3,4,5,7", 5, []string{"1,3,4,5,7"}},
		{"1,2,4,5,7,8", 6, []string{"1,2,4,5,7,8"}},
		{"1,3,5,6,7,8", 6, []string{"1-5:2,6-8"}},
		{"2,4,5,6,8,10,12", 7, []string{"2,4,5,6,8-12:2"}},
		{"1-30:5,31,32,33", 20, []string{"1-31:5,32,33"}},
		{"10-15:2,1-5", 4, []string{"10-14:2,1", "2-5"}},
	}
	for _, tc := range tests {
		t.Run(tc.rangeExpr, func(t *testing.T) {
			got := chunkParam(t, tc.rangeExpr, tc.chunkSize, "NONCONTIGUOUS")
			if !slices.Equal(got, tc.want) {
				t.Fatalf("chunks = %v, want %v", got, tc.want)
			}
			// Each chunk must read back, through the same parser a template's
			// range goes through, as exactly the values it was made from.
			var back []string
			for _, chunk := range got {
				back = append(back, chunkParam(t, chunk, 1, "NONCONTIGUOUS")...)
			}
			if want := chunkParam(t, tc.rangeExpr, 1, "NONCONTIGUOUS"); !slices.Equal(back, want) {
				t.Errorf("chunks %v read back as %v, want %v", got, back, want)
			}
		})
	}
}

// chunkParam expands rangeExpr as a CHUNK[INT] parameter and returns each
// task's value.
func chunkParam(t *testing.T, rangeExpr string, chunkSize int, constraint string) []string {
	t.Helper()
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{{
			Name: "Frame", Type: openjd.TaskParamTypeChunkInt, RangeExpr: new(rangeExpr),
			Chunks: &openjd.TaskChunks{DefaultTaskCount: chunkSize, RangeConstraint: constraint},
		}},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("ExpandParameterSpace(%q): %v", rangeExpr, err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r["Frame"]
	}
	return out
}

// TestDeriveChunkBounds_GappedRangeGivesPerRunBounds checks that .Start/.End
// over a gapped CONTIGUOUS range never enclose a frame the range excluded.
func TestDeriveChunkBounds_GappedRangeGivesPerRunBounds(t *testing.T) {
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{{
			Name: "Frame", Type: openjd.TaskParamTypeChunkInt, RangeExpr: new("1,10-12"),
			Chunks: &openjd.TaskChunks{DefaultTaskCount: 10, RangeConstraint: "CONTIGUOUS"},
		}},
	}
	rows, err := openjd.ExpandParameterSpace(ps)
	if err != nil {
		t.Fatalf("ExpandParameterSpace: %v", err)
	}
	openjd.DeriveChunkBounds(rows, ps)
	want := [][2]string{{"1", "1"}, {"10", "12"}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %d", rows, len(want))
	}
	for i, w := range want {
		if rows[i]["Frame.Start"] != w[0] || rows[i]["Frame.End"] != w[1] {
			t.Errorf("row %d bounds = %s..%s, want %s..%s",
				i, rows[i]["Frame.Start"], rows[i]["Frame.End"], w[0], w[1])
		}
	}
}
