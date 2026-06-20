// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// Parsing must reject a non-scalar anyOf/allOf element rather than silently
// stringifying a nested mapping into a bogus value like "map[min:1]". That
// silent coercion produced a permanently-unschedulable job: an
// `amount.worker.vcpu` reservation mistakenly written as the attribute
// `worker.vcpu` with `anyOf: [{min: 1}]` matched no worker, so its tasks sat
// ready forever while the job stayed pending.
func TestParse_HostRequirements_RejectsNonScalarAttributeValues(t *testing.T) {
	cases := []struct {
		name    string
		hostReq string // YAML body under the step's hostRequirements key (6-space indent)
		wantErr string // substring expected in the parse error; "" => expect success
	}{
		{
			name: "anyOf element is a mapping",
			hostReq: `
      attributes:
        - name: attr.worker.os.family
          anyOf:
            - min: 1`,
			wantErr: "anyOf",
		},
		{
			name: "allOf element is a mapping",
			hostReq: `
      attributes:
        - name: attr.worker.os.family
          allOf:
            - foo: bar`,
			wantErr: "allOf",
		},
		{
			name: "anyOf element is a sequence",
			hostReq: `
      attributes:
        - name: attr.worker.os.family
          anyOf:
            - [nested]`,
			wantErr: "anyOf",
		},
		{
			name: "scalar string anyOf values parse successfully",
			hostReq: `
      attributes:
        - name: attr.worker.os.family
          anyOf:
            - linux
            - windows`,
			wantErr: "",
		},
		{
			name: "numeric and boolean scalar values parse successfully",
			hostReq: `
      attributes:
        - name: attr.worker.custom
          anyOf:
            - 7
            - true`,
			wantErr: "",
		},
	}

	runHostRequirementParseCases(t, cases)
}

// An amount min/max must be a scalar (number, or a template-reference string)
// — never a nested mapping or sequence silently stringified into "map[...]". A
// non-scalar bound on a non-reserved capability name would otherwise be stored
// verbatim and never numerically validated, silently corrupting the reservation.
func TestParse_HostRequirements_RejectsNonScalarAmountBounds(t *testing.T) {
	cases := []struct {
		name    string
		hostReq string
		wantErr string
	}{
		{
			name: "min is a mapping",
			hostReq: `
      amounts:
        - name: amount.worker.vcpu
          min:
            nested: 1`,
			wantErr: "min",
		},
		{
			name: "max is a mapping",
			hostReq: `
      amounts:
        - name: amount.worker.vcpu
          max:
            nested: 1`,
			wantErr: "max",
		},
		{
			name: "min is a sequence",
			hostReq: `
      amounts:
        - name: amount.worker.vcpu
          min:
            - 1`,
			wantErr: "min",
		},
		{
			name: "numeric scalar bounds parse successfully",
			hostReq: `
      amounts:
        - name: amount.worker.vcpu
          min: 1
          max: 8`,
			wantErr: "",
		},
		{
			name: "template-reference string bound parses successfully",
			hostReq: `
      amounts:
        - name: amount.worker.vcpu
          min: "{{Param.MinCPU}}"`,
			wantErr: "",
		},
	}

	runHostRequirementParseCases(t, cases)
}

// runHostRequirementParseCases parses a minimal single-step template with the
// given hostRequirements body and asserts the expected parse outcome.
func runHostRequirementParseCases(t *testing.T, cases []struct {
	name    string
	hostReq string
	wantErr string
},
) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
specificationVersion: jobtemplate-2023-09
name: TestJob
steps:
  - name: Step1
    hostRequirements:` + tc.hostReq + `
    script:
      actions:
        onRun:
          command: echo
`
			_, err := openjd.Parse([]byte(yaml), openjd.FormatYAML)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected successful parse, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected parse error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected parse error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
