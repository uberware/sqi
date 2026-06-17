// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestResolveParameterSpaceParams_NilInput(t *testing.T) {
	got, errs := openjd.ResolveParameterSpaceParams(nil, map[string]string{"X": "1"})
	if got != nil {
		t.Errorf("expected nil output for nil input, got %v", got)
	}
	if len(errs) != 0 {
		t.Errorf("expected no errors for nil input, got %v", errs)
	}
}

func TestResolveParameterSpaceParams(t *testing.T) {
	type tc struct {
		name      string
		ps        *openjd.StepParameterSpace
		jobParams map[string]string
		// expected outcomes:
		wantErrCount int
		wantErrPtr   []string // substring each error pointer must contain
		// if wantErrCount == 0:
		wantRangeExpr *string  // if non-nil, first def's RangeExpr after resolve
		wantRangeList []string // if non-nil, first def's RangeList after resolve
	}

	cases := []tc{
		{
			name: "RangeExpr resolved with Param",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Param.Start}}-{{Param.End}}"),
					},
				},
			},
			jobParams:     map[string]string{"Start": "1", "End": "5"},
			wantRangeExpr: ptr("1-5"),
		},
		{
			name: "RangeExpr resolved with RawParam",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{RawParam.Start}}-{{RawParam.End}}"),
					},
				},
			},
			jobParams:     map[string]string{"Start": "10", "End": "20"},
			wantRangeExpr: ptr("10-20"),
		},
		{
			name: "RangeList entries resolved",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Layer",
						Type:      openjd.TaskParamTypeString,
						RangeList: []string{"prefix-{{Param.X}}", "literal", "{{Param.X}}-suffix"},
					},
				},
			},
			jobParams:     map[string]string{"X": "bg"},
			wantRangeList: []string{"prefix-bg", "literal", "bg-suffix"},
		},
		{
			name: "no references — values unchanged",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("1-10"),
					},
				},
			},
			jobParams:     map[string]string{"Start": "1"},
			wantRangeExpr: ptr("1-10"),
		},
		{
			name: "unknown variable in RangeExpr",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Param.Nope}}-5"),
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 1,
			wantErrPtr:   []string{"/parameterSpace/taskParameterDefinitions/0/range"},
		},
		{
			name: "unknown variable Task.Param.X not available at submit",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Task.Param.X}}-5"),
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 1,
			wantErrPtr:   []string{"/parameterSpace/taskParameterDefinitions/0/range"},
		},
		{
			name: "unknown variable in RangeList entry",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Layer",
						Type:      openjd.TaskParamTypeString,
						RangeList: []string{"{{Param.Missing}}"},
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 1,
			wantErrPtr:   []string{"/parameterSpace/taskParameterDefinitions/0/range/0"},
		},
		{
			name: "multiple task params — errors accumulated",
			ps: &openjd.StepParameterSpace{
				TaskParameterDefinitions: []openjd.TaskParamDefinition{
					{
						Name:      "Frame",
						Type:      openjd.TaskParamTypeInt,
						RangeExpr: ptr("{{Param.MissingA}}-5"),
					},
					{
						Name:      "Layer",
						Type:      openjd.TaskParamTypeString,
						RangeList: []string{"{{Param.MissingB}}"},
					},
				},
			},
			jobParams:    map[string]string{},
			wantErrCount: 2,
			wantErrPtr: []string{
				"/parameterSpace/taskParameterDefinitions/0/range",
				"/parameterSpace/taskParameterDefinitions/1/range/0",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Snapshot the original RangeExpr pointer so we can assert no mutation.
			var originalRangeExpr *string
			if c.ps != nil && len(c.ps.TaskParameterDefinitions) > 0 {
				originalRangeExpr = c.ps.TaskParameterDefinitions[0].RangeExpr
			}

			got, errs := openjd.ResolveParameterSpaceParams(c.ps, c.jobParams)

			// Error-path assertions.
			if c.wantErrCount > 0 {
				if len(errs) != c.wantErrCount {
					t.Errorf("expected %d errors, got %d: %v", c.wantErrCount, len(errs), errs)
				}
				for i, wantPtr := range c.wantErrPtr {
					if i >= len(errs) {
						t.Errorf("missing error[%d] with pointer %q", i, wantPtr)
						continue
					}
					if !strings.Contains(errs[i].Pointer, wantPtr) {
						t.Errorf("errs[%d].Pointer = %q, want it to contain %q", i, errs[i].Pointer, wantPtr)
					}
				}
				if got != nil {
					t.Errorf("expected nil output on error, got %v", got)
				}
				return
			}

			// Success-path assertions.
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if got == nil {
				t.Fatal("expected non-nil output")
			}

			if c.wantRangeExpr != nil {
				if len(got.TaskParameterDefinitions) == 0 {
					t.Fatal("got has no task parameter definitions")
				}
				def := got.TaskParameterDefinitions[0]
				if def.RangeExpr == nil {
					t.Errorf("RangeExpr is nil, want %q", *c.wantRangeExpr)
				} else if *def.RangeExpr != *c.wantRangeExpr {
					t.Errorf("RangeExpr = %q, want %q", *def.RangeExpr, *c.wantRangeExpr)
				}
			}

			if c.wantRangeList != nil {
				if len(got.TaskParameterDefinitions) == 0 {
					t.Fatal("got has no task parameter definitions")
				}
				def := got.TaskParameterDefinitions[0]
				if len(def.RangeList) != len(c.wantRangeList) {
					t.Errorf("RangeList len = %d, want %d", len(def.RangeList), len(c.wantRangeList))
				} else {
					for i, want := range c.wantRangeList {
						if def.RangeList[i] != want {
							t.Errorf("RangeList[%d] = %q, want %q", i, def.RangeList[i], want)
						}
					}
				}
			}

			// Assert input not mutated: if there was an original RangeExpr, the
			// pointer must still point to the same underlying string.
			if originalRangeExpr != nil && c.ps != nil && len(c.ps.TaskParameterDefinitions) > 0 {
				if c.ps.TaskParameterDefinitions[0].RangeExpr != originalRangeExpr {
					t.Error("input parameter space was mutated (RangeExpr pointer changed)")
				}
			}
		})
	}
}

func TestResolveParameterSpaceParams_NoMutation(t *testing.T) {
	// Verify that the returned *StepParameterSpace is a distinct object and
	// that modifying the original does not affect the result.
	original := "{{Param.X}}-5"
	ps := &openjd.StepParameterSpace{
		TaskParameterDefinitions: []openjd.TaskParamDefinition{
			{Name: "Frame", Type: openjd.TaskParamTypeInt, RangeExpr: &original},
		},
	}

	got, errs := openjd.ResolveParameterSpaceParams(ps, map[string]string{"X": "1"})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got == ps {
		t.Error("ResolveParameterSpaceParams returned the same pointer — input was not copied")
	}
	// The original RangeExpr should still point to its unmodified value.
	if *ps.TaskParameterDefinitions[0].RangeExpr != original {
		t.Errorf("original RangeExpr was mutated: got %q", *ps.TaskParameterDefinitions[0].RangeExpr)
	}
	// The resolved result should have the substituted value.
	if got.TaskParameterDefinitions[0].RangeExpr == nil || *got.TaskParameterDefinitions[0].RangeExpr != "1-5" {
		t.Errorf("resolved RangeExpr = %v, want %q", got.TaskParameterDefinitions[0].RangeExpr, "1-5")
	}
}
