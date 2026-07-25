// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

// Capability names must use the spec-mandated namespace prefix: amounts begin
// with "amount.", attributes with "attr.". A mis-namespaced name — e.g. an
// `amount.worker.vcpu` requirement mistakenly written as the attribute
// `worker.vcpu` — resolves to an empty worker value and so can never be
// satisfied, leaving the job's tasks permanently ready. This is structural
// correctness, not a size cap, so it always runs — even with EnforceLimits
// false (see the invariant documented on [openjd.ValidateOptions] and
// [openjd.ValidateWithOptions]'s split between validateHostRequirements and
// validateHostRequirementLimits).
func TestValidate_CapabilityNamePrefix_Structural(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*openjd.JobTemplate)
		wantPtr string // "" => mutation is valid; no prefix error expected
	}{
		{
			name: "amount without amount. prefix rejected",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "worker.vcpu", Min: ptr("1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/name",
		},
		{
			name: "amount with amount. prefix accepted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: ptr("1")}},
				}
			},
		},
		{
			name: "custom vendor amount prefix accepted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.acme.slots", Min: ptr("1")}},
				}
			},
		},
		{
			name: "attribute without attr. prefix rejected",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "worker.vcpu", AnyOf: []string{"1"}}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/name",
		},
		{
			name: "attribute with attr. prefix accepted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"linux"}}},
				}
			},
		},
		{
			// Capability names are case-insensitive (OpenJD jobtemplate-2023-09)
			// and the matcher now resolves them case-insensitively, so a mixed-case
			// namespace is valid — validation must accept it.
			name: "mixed-case namespace prefix accepted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "Amount.Worker.VCPU", Min: ptr("1")}},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ── EnforceLimits=true: the prefix error must fire ────────────────
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

			// ── EnforceLimits=false: the prefix check is structural and must
			// still fire ───────────────────────────────────────────────────
			tmplOff := mustParse(t, minimalValidYAML())
			tc.mutate(tmplOff)
			errsOff := openjd.ValidateWithOptions(tmplOff, openjd.ValidateOptions{EnforceLimits: false})

			if tc.wantPtr == "" {
				if len(errsOff) != 0 {
					t.Fatalf("EnforceLimits=false: expected no errors, got %v", errsOff)
				}
			} else if !containsPointer(errsOff, tc.wantPtr) {
				t.Fatalf("EnforceLimits=false: expected pointer %q, got %v", tc.wantPtr, errsOff)
			}
		})
	}
}
