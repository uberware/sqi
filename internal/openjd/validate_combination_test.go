// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for combination-expression validation, including the OpenJD rule that a
// CHUNK[INT] task parameter must not be associated with other parameters.

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func chunkStep(t *testing.T, comb string) *openjd.JobTemplate {
	t.Helper()
	tmpl := mustParse(t, minimalValidYAML())
	// Declare TASK_CHUNKING so the extension gate does not fire — this helper
	// is intended to test combination semantics, not extension gating.
	tmpl.Extensions = []string{"TASK_CHUNKING"}
	c := comb
	tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{
				Name:      "Chunked",
				Type:      openjd.TaskParamTypeChunkInt,
				RangeExpr: new("1-10"),
				Chunks:    &openjd.TaskChunks{DefaultTaskCount: 2, RangeConstraint: "CONTIGUOUS"},
			},
			{Name: "Other", Type: openjd.TaskParamTypeString, RangeList: []string{"x", "y", "z", "w", "v"}},
		},
		Combination: &c,
	}
	return tmpl
}

func TestValidate_ChunkAssociatedWithOtherParam_Error(t *testing.T) {
	tmpl := chunkStep(t, "(Chunked, Other)")
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "associated") {
		t.Errorf("expected chunk-association error; got %v", errs)
	}
}

func TestValidate_ChunkAssociatedNested_Error(t *testing.T) {
	// Chunked appears in a product that is itself a comma-separated item of an
	// association group, so it is still zipped with the group's other items.
	tmpl := mustParse(t, minimalValidYAML())
	// Declare TASK_CHUNKING so the extension gate does not fire — this test is
	// checking combination semantics, not extension gating.
	tmpl.Extensions = []string{"TASK_CHUNKING"}
	c := "(Chunked * Other, Extra)"
	tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "Chunked", Type: openjd.TaskParamTypeChunkInt, RangeExpr: new("1-10"), Chunks: &openjd.TaskChunks{DefaultTaskCount: 2, RangeConstraint: "CONTIGUOUS"}},
			{Name: "Other", Type: openjd.TaskParamTypeString, RangeList: []string{"x"}},
			{Name: "Extra", Type: openjd.TaskParamTypeString, RangeList: []string{"e"}},
		},
		Combination: &c,
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "associated") {
		t.Errorf("expected chunk-association error for nested group; got %v", errs)
	}
}

func TestValidate_ChunkCombinedWithProduct_OK(t *testing.T) {
	tmpl := chunkStep(t, "Chunked * Other")
	errs := openjd.Validate(tmpl)
	if containsMessage(errs, "associated") {
		t.Errorf("did not expect chunk-association error for product combination; got %v", errs)
	}
}

func TestValidate_MultipleChunkParams_Error(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	// Declare TASK_CHUNKING so the extension gate does not fire — this test is
	// checking the at-most-one-CHUNK[INT] structural rule, not extension gating.
	tmpl.Extensions = []string{"TASK_CHUNKING"}
	c := "ChunkA * ChunkB"
	tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "ChunkA", Type: openjd.TaskParamTypeChunkInt, RangeExpr: new("1-10"), Chunks: &openjd.TaskChunks{DefaultTaskCount: 2, RangeConstraint: "CONTIGUOUS"}},
			{Name: "ChunkB", Type: openjd.TaskParamTypeChunkInt, RangeExpr: new("1-10"), Chunks: &openjd.TaskChunks{DefaultTaskCount: 2, RangeConstraint: "CONTIGUOUS"}},
		},
		Combination: &c,
	}
	errs := openjd.Validate(tmpl)
	if !containsMessage(errs, "at most one CHUNK[INT]") {
		t.Errorf("expected at-most-one-chunk error; got %v", errs)
	}
}

// A recursive combination over plain params must still validate cleanly.
func TestValidate_RecursiveCombination_OK(t *testing.T) {
	tmpl := mustParse(t, minimalValidYAML())
	c := "(A*B, C)"
	tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "A", Type: openjd.TaskParamTypeString, RangeList: []string{"a"}},
			{Name: "B", Type: openjd.TaskParamTypeString, RangeList: []string{"b"}},
			{Name: "C", Type: openjd.TaskParamTypeString, RangeList: []string{"c"}},
		},
		Combination: &c,
	}
	errs := openjd.Validate(tmpl)
	if len(errs) != 0 {
		t.Errorf("expected no errors for valid recursive combination; got %v", errs)
	}
}
