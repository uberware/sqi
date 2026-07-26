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

// A capability name may carry an optional vendor-specific prefix. The spec
// (2023-09 Template Schemas §3.3.1.1 and §3.3.2.1) defines the format as
// "[<Identifier>:]amount.<Identifier>[.<Identifier>]*" — the bracketed
// "<Identifier>:" is an optional vendor namespace, and the same shape applies
// to attributes with "attr.".
//
// sqi rejected every vendor-prefixed name outright, so a studio could not
// namespace its own capabilities and conforming templates from other OpenJD
// tools were refused. Surfaced by the official conformance suite fixtures
// 3.3--vendor-prefix-amount.yaml and 3.3--vendor-prefix-attribute.yaml.
func TestValidate_CapabilityNameVendorPrefix(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*openjd.JobTemplate)
		wantPtr string // "" => valid; no prefix error expected
	}{
		{
			name: "vendor-prefixed amount accepted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "mycompany:amount.licenses", Min: ptr("1")}},
				}
			},
		},
		{
			name: "vendor-prefixed attribute accepted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{
						Name: "mycompany:attr.software", AnyOf: []string{"maya"},
					}},
				}
			},
		},
		{
			name: "vendor prefix with underscore accepted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "_my_co:amount.licenses", Min: ptr("1")}},
				}
			},
		},
		{
			name: "vendor prefix is case-insensitive like the rest of the name",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "MyCompany:AMOUNT.Licenses", Min: ptr("1")}},
				}
			},
		},
		{
			name: "vendor prefix does not excuse a missing namespace",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "mycompany:worker.vcpu", Min: ptr("1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/name",
		},
		{
			name: "vendor prefix does not excuse the wrong namespace",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "mycompany:attr.software", Min: ptr("1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/name",
		},
		{
			name: "malformed vendor identifier rejected",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "123bad:amount.licenses", Min: ptr("1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/name",
		},
		{
			name: "only one vendor prefix is permitted",
			mutate: func(tp *openjd.JobTemplate) {
				tp.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "a:b:amount.licenses", Min: ptr("1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustParse(t, minimalValidYAML())
			tc.mutate(tmpl)
			errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: true})

			if tc.wantPtr == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got %v", errs)
				}
				return
			}
			if !containsPointer(errs, tc.wantPtr) {
				t.Fatalf("expected pointer %q, got %v", tc.wantPtr, errs)
			}
		})
	}
}
