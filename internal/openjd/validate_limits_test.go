// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/openjd"
)

// makeStrings returns a slice of n short distinct strings.
func makeStrings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("v%d", i)
	}
	return out
}

// makeJobParams returns n valid STRING job parameters with unique names.
func makeJobParams(n int) []openjd.JobParameter {
	out := make([]openjd.JobParameter, n)
	for i := range out {
		out[i] = openjd.JobParameter{Name: fmt.Sprintf("P%d", i), Type: openjd.JobParamTypeString}
	}
	return out
}

// makeTaskParams returns n valid INT task parameters with unique names.
func makeTaskParams(n int) []openjd.TaskParamDefinition {
	out := make([]openjd.TaskParamDefinition, n)
	for i := range out {
		out[i] = openjd.TaskParamDefinition{
			Name:      fmt.Sprintf("T%d", i),
			Type:      openjd.TaskParamTypeInt,
			RangeList: []string{"1"},
		}
	}
	return out
}

// makeAmounts returns n valid amount requirements.
func makeAmounts(n int) []openjd.AmountRequirement {
	out := make([]openjd.AmountRequirement, n)
	for i := range out {
		out[i] = openjd.AmountRequirement{Name: fmt.Sprintf("amount.x%d", i), Min: new("1")}
	}
	return out
}

// TestValidate_Limits_Gated is the canonical table covering every quantitative
// limit at its boundary. Each case mutates a valid base template and asserts
// that with EnforceLimits=true the expected pointer error appears (or, for "ok"
// boundary cases, no error appears), while with EnforceLimits=false no limit
// error appears — proving the gate toggles behavior.
func TestValidate_Limits_Gated(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*openjd.JobTemplate)
		wantPtr string // "" means the mutation is valid (no error even when enforcing)
	}{
		// ── job parameterDefinitions: upper bound 50 ──
		{
			name:   "job params 50 ok",
			mutate: func(t *openjd.JobTemplate) { t.ParameterDefinitions = makeJobParams(50) },
		},
		{
			name:    "job params 51 error",
			mutate:  func(t *openjd.JobTemplate) { t.ParameterDefinitions = makeJobParams(51) },
			wantPtr: "/parameterDefinitions",
		},

		// ── job name length: <= 128 ──
		{
			name:   "job name 128 ok",
			mutate: func(t *openjd.JobTemplate) { t.Name = strings.Repeat("a", 128) },
		},
		{
			name:    "job name 129 error",
			mutate:  func(t *openjd.JobTemplate) { t.Name = strings.Repeat("a", 129) },
			wantPtr: "/name",
		},

		// ── step name length: <= 64 ──
		{
			name:   "step name 64 ok",
			mutate: func(t *openjd.JobTemplate) { t.Steps[0].Name = strings.Repeat("a", 64) },
		},
		{
			name:    "step name 65 error",
			mutate:  func(t *openjd.JobTemplate) { t.Steps[0].Name = strings.Repeat("a", 65) },
			wantPtr: "/steps/0/name",
		},

		// ── job environment name length: <= 64 ──
		{
			name: "job env name 64 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.JobEnvironments = []openjd.Environment{{Name: strings.Repeat("e", 64)}}
			},
		},
		{
			name: "job env name 65 error",
			mutate: func(t *openjd.JobTemplate) {
				t.JobEnvironments = []openjd.Environment{{Name: strings.Repeat("e", 65)}}
			},
			wantPtr: "/jobEnvironments/0/name",
		},

		// ── step environment name length: <= 64 ──
		{
			name: "step env name 64 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].StepEnvironments = []openjd.Environment{{Name: strings.Repeat("e", 64)}}
			},
		},
		{
			name: "step env name 65 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].StepEnvironments = []openjd.Environment{{Name: strings.Repeat("e", 65)}}
			},
			wantPtr: "/steps/0/stepEnvironments/0/name",
		},

		// ── name lengths count CHARACTERS (runes), not bytes ──
		// A multi-byte CJK name within the character limit but over it in bytes
		// must be ACCEPTED. "好" is 3 bytes; 64 of them = 192 bytes ≤ 64 chars.
		{
			name:   "job name 64 CJK chars ok (192 bytes)",
			mutate: func(t *openjd.JobTemplate) { t.Name = strings.Repeat("好", 64) },
		},
		{
			name:    "job name 129 CJK chars error",
			mutate:  func(t *openjd.JobTemplate) { t.Name = strings.Repeat("好", 129) },
			wantPtr: "/name",
		},
		{
			name: "step name 64 CJK chars ok (192 bytes)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].Name = strings.Repeat("好", 64)
			},
		},
		{
			name: "step name 65 CJK chars error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].Name = strings.Repeat("好", 65)
			},
			wantPtr: "/steps/0/name",
		},

		// ── taskParameterDefinitions count: 1–16 ──
		{
			name: "task params 16 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: makeTaskParams(16),
				}
			},
		},
		{
			name: "task params 17 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: makeTaskParams(17),
				}
			},
			wantPtr: "/steps/0/parameterSpace/taskParameterDefinitions",
		},

		// ── per-task-parameter value count: 1–1024 ──
		{
			name: "task param 1024 values ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-1024")},
					},
				}
			},
		},
		{
			name: "task param 1025 values error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-1025")},
					},
				}
			},
			wantPtr: "/steps/0/parameterSpace/taskParameterDefinitions/0/range",
		},
		{
			name: "task param 1025 list values error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "S", Type: openjd.TaskParamTypeString, RangeList: makeStrings(1025)},
					},
				}
			},
			wantPtr: "/steps/0/parameterSpace/taskParameterDefinitions/0/range",
		},
		{
			name: "task param range with template ref skips value-count",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-{{Param.N}}")},
					},
				}
			},
		},

		// ── hostRequirements: combined count <= 50, present => >= 1 ──
		{
			name: "host req 50 combined ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{Amounts: makeAmounts(50)}
			},
		},
		{
			name: "host req 51 combined error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{Amounts: makeAmounts(51)}
			},
			wantPtr: "/steps/0/hostRequirements",
		},
		{
			name: "host req present but empty error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{}
			},
			wantPtr: "/steps/0/hostRequirements",
		},

		// ── capability name length: 1–100 ──
		{
			name: "amount name 100 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: strings.Repeat("a", 100), Min: new("1")}},
				}
			},
		},
		{
			name: "amount name 101 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: strings.Repeat("a", 101), Min: new("1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/name",
		},
		{
			name: "amount name empty error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "", Min: new("1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/name",
		},
		{
			name: "attribute name 101 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: strings.Repeat("a", 101), AnyOf: []string{"x"}}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/name",
		},

		// ── attribute anyOf/allOf element count: 1–50 ──
		{
			name: "attribute anyOf 50 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.x", AnyOf: makeStrings(50)}},
				}
			},
		},
		{
			name: "attribute anyOf 51 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.x", AnyOf: makeStrings(51)}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/anyOf",
		},
		{
			name: "attribute allOf 51 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.x", AllOf: makeStrings(51)}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/allOf",
		},
		{
			name: "attribute no values error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.x"}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0",
		},

		// ── INT range overlap ──
		{
			name: "int range overlap error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-5,3-7")},
					},
				}
			},
			wantPtr: "/steps/0/parameterSpace/taskParameterDefinitions/0/range",
		},
		{
			name: "int range non-overlapping ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-5,6-10")},
					},
				}
			},
		},
		{
			name: "int range overlap with template ref skipped",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new("1-5,{{Param.N}}")},
					},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Enforcing.
			tmplOn := mustParse(t, minimalValidYAML())
			tc.mutate(tmplOn)
			errsOn := openjd.ValidateWithOptions(tmplOn, openjd.ValidateOptions{EnforceLimits: true})

			if tc.wantPtr == "" {
				if len(errsOn) != 0 {
					t.Fatalf("EnforceLimits=true: expected no errors, got %v", errsOn)
				}
			} else if !containsPointer(errsOn, tc.wantPtr) {
				t.Fatalf("EnforceLimits=true: expected pointer %q, got %v", tc.wantPtr, errsOn)
			}

			// Not enforcing: the gated limit must not fire.
			tmplOff := mustParse(t, minimalValidYAML())
			tc.mutate(tmplOff)
			errsOff := openjd.ValidateWithOptions(tmplOff, openjd.ValidateOptions{EnforceLimits: false})
			if tc.wantPtr != "" && containsPointer(errsOff, tc.wantPtr) {
				t.Fatalf("EnforceLimits=false: limit pointer %q should be gated off, got %v", tc.wantPtr, errsOff)
			}
			if tc.wantPtr == "" && len(errsOff) != 0 {
				t.Fatalf("EnforceLimits=false: expected no errors, got %v", errsOff)
			}
		})
	}
}

// TestValidate_HugeRange_NoOOM is the resource-exhaustion regression guard at
// the template-validation boundary (the path reachable from POST
// /api/v1/jobs). A range whose arithmetic count exceeds the hard cap must be
// rejected at the UNGATED structural parse check — regardless of EnforceLimits
// — and must do so quickly, without materializing billions of integers.
func TestValidate_HugeRange_NoOOM(t *testing.T) {
	const rangePtr = "/steps/0/parameterSpace/taskParameterDefinitions/0/range"

	for _, expr := range []string{"1-2000000000", "1-2000000000:2"} {
		for _, enforce := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s_enforce=%v", expr, enforce), func(t *testing.T) {
				tmpl := mustParse(t, minimalValidYAML())
				tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
					TaskParameterDefinitions: []openjd.TaskParamDefinition{
						{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: new(expr)},
					},
				}

				done := make(chan openjd.ValidationErrors, 1)
				go func() {
					done <- openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: enforce})
				}()

				select {
				case errs := <-done:
					if !containsPointer(errs, rangePtr) {
						t.Fatalf("expected error at %q for %q (enforce=%v), got %v", rangePtr, expr, enforce, errs)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("validation of %q did not complete promptly (possible OOM)", expr)
				}
			})
		}
	}
}
