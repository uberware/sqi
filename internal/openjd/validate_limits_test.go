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
		// structural marks a case whose check was split out of validateLimits
		// (host-requirement structural correctness, always runs regardless of
		// EnforceLimits — see the invariant documented on
		// openjd.ValidateOptions). For these, wantPtr must fire even with
		// EnforceLimits=false instead of being gated off.
		structural bool
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
				t.JobEnvironments = []openjd.Environment{{
					Name:      strings.Repeat("e", 64),
					Variables: map[string]string{"K": "V"},
				}}
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
				t.Steps[0].StepEnvironments = []openjd.Environment{{
					Name:      strings.Repeat("e", 64),
					Variables: map[string]string{"K": "V"},
				}}
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
			wantPtr:    "/steps/0/hostRequirements",
			structural: true,
		},

		// ── capability name length: 1–100 ──
		{
			name: "amount name 100 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount." + strings.Repeat("a", 93), Min: new("1")}},
				}
			},
		},
		{
			name: "amount name 101 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount." + strings.Repeat("a", 94), Min: new("1")}},
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
			wantPtr:    "/steps/0/hostRequirements/amounts/0/name",
			structural: true,
		},
		{
			name: "attribute name 101 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr." + strings.Repeat("a", 96), AnyOf: []string{"x"}}},
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
			wantPtr:    "/steps/0/hostRequirements/attributes/0",
			structural: true,
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

			// Not enforcing: a gated limit must not fire, but a structural
			// check (split out of validateLimits into the always-run path)
			// must still fire.
			tmplOff := mustParse(t, minimalValidYAML())
			tc.mutate(tmplOff)
			errsOff := openjd.ValidateWithOptions(tmplOff, openjd.ValidateOptions{EnforceLimits: false})
			switch {
			case tc.structural:
				if !containsPointer(errsOff, tc.wantPtr) {
					t.Fatalf("EnforceLimits=false: structural pointer %q should still fire, got %v", tc.wantPtr, errsOff)
				}
			case tc.wantPtr != "" && containsPointer(errsOff, tc.wantPtr):
				t.Fatalf("EnforceLimits=false: limit pointer %q should be gated off, got %v", tc.wantPtr, errsOff)
			case tc.wantPtr == "" && len(errsOff) != 0:
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

// ─── E4c Task 1: the parameter-space caps must gate the expression walk ──────
//
// The expression walk (checkTemplateExpressions) is the more expensive of the
// two walks the checker runs over a step's parameter space -- validated
// directly: one step at exactly maxTaskParameterDefinitions x
// 1024 entries of `{{ ("x" * 900000).upper() }}` cost ~97s of
// CPU in this walk alone before this task, unbounded by anything but the 4
// MiB request body. These tests assert the PROPERTY the fix establishes --
// the cap error is present and a walk-only error is absent -- rather than a
// wall-clock threshold, which flakes on shared CI.

// exprEnabledYAML is minimalValidYAML with the EXPR extension declared, so
// checkTemplateExpressions actually walks the template instead of no-oping
// (it returns nil immediately for a template that does not declare EXPR --
// see its own doc comment in exprcheck.go).
func exprEnabledYAML() string {
	return `
specificationVersion: jobtemplate-2023-09
name: TestJob
extensions: [EXPR]
steps:
  - name: Step1
    script:
      actions:
        onRun:
          command: echo
          args: ["hello"]
`
}

// walkOnlySyntaxError is a `{{ ... }}` expression with a syntax error. Only
// the expression walk (checkTemplateExpressions -> checkParameterSpaceExpressions
// -> checkFormatString) can ever report it: the parameter-space cap checks
// (validateParameterSpaceLimits) never look inside a RangeList entry's text,
// only at counts. Its presence or absence at a RangeList entry's pointer is
// therefore a clean proxy for whether the walk ran over that position.
const walkOnlySyntaxError = "{{ 1 + }}"

// makeStringTaskParams returns n STRING task-parameter definitions with
// unique names, each carrying a single-entry RangeList of "ok" -- except
// index badIdx, whose entry is walkOnlySyntaxError.
func makeStringTaskParams(n, badIdx int) []openjd.TaskParamDefinition {
	out := make([]openjd.TaskParamDefinition, n)
	for i := range out {
		v := "ok"
		if i == badIdx {
			v = walkOnlySyntaxError
		}
		out[i] = openjd.TaskParamDefinition{
			Name:      fmt.Sprintf("S%d", i),
			Type:      openjd.TaskParamTypeString,
			RangeList: []string{v},
		}
	}
	return out
}

// walkOnlyErrorPtr is the pointer at which a RangeList entry's own
// (in-)validity is reported (exprcheck.go's checkParameterSpaceExpressions:
// "%s/taskParameterDefinitions/%d/range/%d"). No cap check ever produces an
// error at this shape -- the count cap stops at ".../range" with no trailing
// list index -- so the two pointer shapes never collide.
func walkOnlyErrorPtr(defIdx, entryIdx int) string {
	return fmt.Sprintf("/steps/0/parameterSpace/taskParameterDefinitions/%d/range/%d", defIdx, entryIdx)
}

func TestValidate_OverCapParameterSpace_SkipsExpressionWalk(t *testing.T) {
	const defCountCapPtr = "/steps/0/parameterSpace/taskParameterDefinitions"

	t.Run("baseline: at the cap, the walk runs and catches the bad expression", func(t *testing.T) {
		tmpl := mustParse(t, exprEnabledYAML())
		tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
			TaskParameterDefinitions: makeStringTaskParams(16, 15),
		}
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
		if containsPointer(errs, defCountCapPtr) {
			t.Fatalf("16 definitions must not trip the count cap, got %v", errs)
		}
		if !containsPointer(errs, walkOnlyErrorPtr(15, 0)) {
			t.Fatalf("expected the walk to report the syntax error at %q, got %v", walkOnlyErrorPtr(15, 0), errs)
		}
	})

	t.Run("baseline: value dimension at the cap (1024), the walk runs", func(t *testing.T) {
		// Pins the OTHER boundary: a future ">=" typo in the value-count half
		// of parameterSpaceOverCaps would silently stop checking expressions
		// for the largest legal parameter lists, with nothing else in the
		// suite failing (the definition-count baseline above only exercises
		// 1 value per definition).
		vals := make([]string, 1024)
		for i := range vals {
			vals[i] = "ok"
		}
		vals[1024-1] = walkOnlySyntaxError
		tmpl := mustParse(t, exprEnabledYAML())
		tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
			TaskParameterDefinitions: []openjd.TaskParamDefinition{
				{Name: "S", Type: openjd.TaskParamTypeString, RangeList: vals},
			},
		}
		valueCountCapPtr := "/steps/0/parameterSpace/taskParameterDefinitions/0/range"
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
		if containsPointer(errs, valueCountCapPtr) {
			t.Fatalf("%d values must not trip the value-count cap, got %v", 1024, errs)
		}
		if !containsPointer(errs, walkOnlyErrorPtr(0, 1024-1)) {
			t.Fatalf("expected the walk to report the syntax error at %q, got %v",
				walkOnlyErrorPtr(0, 1024-1), errs)
		}
	})

	t.Run("over the definition-count cap, EnforceLimits=true", func(t *testing.T) {
		tmpl := mustParse(t, exprEnabledYAML())
		tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
			TaskParameterDefinitions: makeStringTaskParams(17, 16),
		}
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
		if !containsPointer(errs, defCountCapPtr) {
			t.Fatalf("expected the definition-count cap error at %q, got %v", defCountCapPtr, errs)
		}
		if containsPointer(errs, walkOnlyErrorPtr(16, 0)) {
			t.Fatalf("walk-only error must be ABSENT -- the walk must not have run over an over-cap parameter space, got %v", errs)
		}
	})

	t.Run("over the definition-count cap, EnforceLimits=false", func(t *testing.T) {
		// The submit pipeline runs with EnforceLimits: false. The cap
		// ValidationError itself stays gated off (unchanged from before this
		// task), but the walk must STILL be skipped -- gating the walk on
		// EnforceLimits would leave exactly the pipeline that matters
		// unprotected, which is the entire point of this task.
		tmpl := mustParse(t, exprEnabledYAML())
		tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
			TaskParameterDefinitions: makeStringTaskParams(17, 16),
		}
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        false,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
		if containsPointer(errs, defCountCapPtr) {
			t.Fatalf("EnforceLimits=false: cap error must stay gated off, got %v", errs)
		}
		if containsPointer(errs, walkOnlyErrorPtr(16, 0)) {
			t.Fatalf("walk-only error must be ABSENT even with EnforceLimits=false, got %v", errs)
		}
	})

	t.Run("over the per-parameter value-count cap, EnforceLimits=true", func(t *testing.T) {
		vals := make([]string, 1025)
		for i := range vals {
			vals[i] = "ok"
		}
		vals[1024] = walkOnlySyntaxError
		tmpl := mustParse(t, exprEnabledYAML())
		tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
			TaskParameterDefinitions: []openjd.TaskParamDefinition{
				{Name: "S", Type: openjd.TaskParamTypeString, RangeList: vals},
			},
		}
		valueCountCapPtr := "/steps/0/parameterSpace/taskParameterDefinitions/0/range"
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
		if !containsPointer(errs, valueCountCapPtr) {
			t.Fatalf("expected the value-count cap error at %q, got %v", valueCountCapPtr, errs)
		}
		if containsPointer(errs, walkOnlyErrorPtr(0, 1024)) {
			t.Fatalf("walk-only error must be ABSENT -- the walk must not have run over an over-cap parameter space, got %v", errs)
		}
	})

	t.Run("an overlap-only violation (no count problem) does not silence the walk", func(t *testing.T) {
		// "1-5,3-8" overlaps but is nowhere near either count cap -- 1
		// definition, 8 values. parameterSpaceOverCaps must not treat this
		// as over-cap (it checks only maxTaskParameterDefinitions and
		// 1024, not INT range overlap -- see its own doc
		// comment), so a second, unrelated task-parameter definition's
		// walk-only syntax error in the SAME step must still be reported.
		overlapping := "1-5,3-8"
		tmpl := mustParse(t, exprEnabledYAML())
		tmpl.Steps[0].ParameterSpace = &openjd.StepParameterSpace{
			TaskParameterDefinitions: []openjd.TaskParamDefinition{
				{Name: "F", Type: openjd.TaskParamTypeInt, RangeExpr: &overlapping},
				{Name: "S0", Type: openjd.TaskParamTypeString, RangeList: []string{walkOnlySyntaxError}},
			},
		}
		overlapCapPtr := "/steps/0/parameterSpace/taskParameterDefinitions/0/range"
		errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{
			EnforceLimits:                        true,
			CheckEXPRExpressionsWhileUnsupported: true,
		})
		if !containsPointer(errs, overlapCapPtr) {
			t.Fatalf("expected the overlap error at %q, got %v", overlapCapPtr, errs)
		}
		if !containsPointer(errs, walkOnlyErrorPtr(1, 0)) {
			t.Fatalf("an overlap-only violation must not silence the walk over the rest of the step; "+
				"expected the walk-only error at %q, got %v", walkOnlyErrorPtr(1, 0), errs)
		}
	})
}
