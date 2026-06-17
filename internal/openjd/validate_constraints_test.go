// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for constraint-definition syntax validation in validateJobParams.
//
// Fix 1: malformed constraint strings in parameterDefinitions must be rejected
// at template-validation time (not silently ignored during binding).
//
// Covered here:
//   - INT  minValue/maxValue/allowedValues must parse as int64
//   - FLOAT minValue/maxValue/allowedValues must parse as float64 and be finite
//   - INT / FLOAT min > max is rejected
//   - STRING / PATH minLength > maxLength is rejected
//   - Valid baseline cases still pass

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestValidateJobParams_ConstraintSyntax(t *testing.T) {
	type tc struct {
		name    string
		param   openjd.JobParameter
		wantErr bool
		errPtr  string // exact JSON pointer expected in one of the errors
		errMsg  string // substring expected in that error's message
	}

	cases := []tc{
		// ── valid baselines ───────────────────────────────────────────────────

		{
			name: "INT valid min/max/allowedValues",
			param: openjd.JobParameter{
				Name:          "Count",
				Type:          openjd.JobParamTypeInt,
				MinValue:      ptr("1"),
				MaxValue:      ptr("100"),
				AllowedValues: []string{"1", "10", "100"},
			},
		},
		{
			name: "INT equal min and max",
			param: openjd.JobParameter{
				Name:     "Count",
				Type:     openjd.JobParamTypeInt,
				MinValue: ptr("42"),
				MaxValue: ptr("42"),
			},
		},
		{
			name: "FLOAT valid min/max/allowedValues",
			param: openjd.JobParameter{
				Name:          "Scale",
				Type:          openjd.JobParamTypeFloat,
				MinValue:      ptr("0.0"),
				MaxValue:      ptr("1.0"),
				AllowedValues: []string{"0.25", "0.5", "1.0"},
			},
		},
		{
			name: "FLOAT equal min and max",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				MinValue: ptr("0.5"),
				MaxValue: ptr("0.5"),
			},
		},
		{
			name: "STRING equal minLength and maxLength",
			param: openjd.JobParameter{
				Name:      "Label",
				Type:      openjd.JobParamTypeString,
				MinLength: ptr(5),
				MaxLength: ptr(5),
			},
		},
		{
			name: "PATH valid minLength < maxLength",
			param: openjd.JobParameter{
				Name:      "Output",
				Type:      openjd.JobParamTypePath,
				MinLength: ptr(1),
				MaxLength: ptr(255),
			},
		},

		// ── INT malformed constraint syntax ───────────────────────────────────

		{
			name: "INT malformed minValue",
			param: openjd.JobParameter{
				Name:     "Count",
				Type:     openjd.JobParamTypeInt,
				MinValue: ptr("abc"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minValue",
			errMsg:  "not a valid INT",
		},
		{
			name: "INT malformed maxValue",
			param: openjd.JobParameter{
				Name:     "Count",
				Type:     openjd.JobParamTypeInt,
				MaxValue: ptr("xyz"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/maxValue",
			errMsg:  "not a valid INT",
		},
		{
			name: "INT malformed allowedValues entry at index 1",
			param: openjd.JobParameter{
				Name:          "Count",
				Type:          openjd.JobParamTypeInt,
				AllowedValues: []string{"1", "two", "3"},
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/allowedValues/1",
			errMsg:  "not a valid INT",
		},
		{
			name: "INT minValue greater than maxValue",
			param: openjd.JobParameter{
				Name:     "Count",
				Type:     openjd.JobParamTypeInt,
				MinValue: ptr("100"),
				MaxValue: ptr("10"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minValue",
			errMsg:  "<=",
		},

		// ── FLOAT malformed constraint syntax ─────────────────────────────────

		{
			name: "FLOAT malformed minValue",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				MinValue: ptr("abc"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minValue",
			errMsg:  "not a valid FLOAT",
		},
		{
			name: "FLOAT malformed maxValue",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				MaxValue: ptr("xyz"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/maxValue",
			errMsg:  "not a valid FLOAT",
		},
		{
			name: "FLOAT NaN in minValue",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				MinValue: ptr("NaN"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minValue",
			errMsg:  "finite",
		},
		{
			name: "FLOAT +Inf in maxValue",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				MaxValue: ptr("+Inf"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/maxValue",
			errMsg:  "finite",
		},
		{
			name: "FLOAT -Inf in minValue",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				MinValue: ptr("-Inf"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minValue",
			errMsg:  "finite",
		},
		{
			name: "FLOAT NaN in allowedValues",
			param: openjd.JobParameter{
				Name:          "Scale",
				Type:          openjd.JobParamTypeFloat,
				AllowedValues: []string{"0.5", "NaN"},
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/allowedValues/1",
			errMsg:  "finite",
		},
		{
			name: "FLOAT malformed allowedValues entry",
			param: openjd.JobParameter{
				Name:          "Scale",
				Type:          openjd.JobParamTypeFloat,
				AllowedValues: []string{"0.5", "notafloat"},
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/allowedValues/1",
			errMsg:  "not a valid FLOAT",
		},
		{
			name: "FLOAT minValue greater than maxValue",
			param: openjd.JobParameter{
				Name:     "Scale",
				Type:     openjd.JobParamTypeFloat,
				MinValue: ptr("1.0"),
				MaxValue: ptr("0.5"),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minValue",
			errMsg:  "<=",
		},

		// ── STRING / PATH length constraints ──────────────────────────────────

		{
			name: "STRING minLength greater than maxLength",
			param: openjd.JobParameter{
				Name:      "Label",
				Type:      openjd.JobParamTypeString,
				MinLength: ptr(10),
				MaxLength: ptr(5),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minLength",
			errMsg:  "<=",
		},
		{
			name: "PATH minLength greater than maxLength",
			param: openjd.JobParameter{
				Name:      "Output",
				Type:      openjd.JobParamTypePath,
				MinLength: ptr(100),
				MaxLength: ptr(50),
			},
			wantErr: true,
			errPtr:  "/parameterDefinitions/0/minLength",
			errMsg:  "<=",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, minimalValidYAML())
			tmpl.ParameterDefinitions = []openjd.JobParameter{tc.param}
			errs := openjd.Validate(tmpl)

			if !tc.wantErr {
				if len(errs) != 0 {
					t.Errorf("expected no errors; got: %v", errs)
				}
				return
			}

			if len(errs) == 0 {
				t.Fatalf("expected a validation error; got none")
			}

			found := false
			for _, e := range errs {
				if e.Pointer == tc.errPtr && strings.Contains(e.Message, tc.errMsg) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected error at pointer %q containing %q; got:\n%v",
					tc.errPtr, tc.errMsg, errs)
			}
		})
	}
}
