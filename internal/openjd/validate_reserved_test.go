// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for reserved capability-name and attribute-value checks.
//
// These are value-domain correctness checks, not size/count caps, so they
// run unconditionally from validateHostRequirements regardless of
// EnforceLimits — see the invariant documented on ValidateOptions. Each
// table case is exercised under both EnforceLimits=true and
// EnforceLimits=false and must produce the identical result either way: if
// wantPtr != "", the error must appear at that pointer in both modes.

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
)

func TestValidate_ReservedNames_AlwaysEnforced(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*openjd.JobTemplate)
		wantPtr string // "" → mutation is valid; no error expected
	}{
		// ── Reserved AMOUNT minimums ──────────────────────────────────────────

		{
			name: "amount.worker.vcpu min 1 ok (at reserved minimum)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("1")}},
				}
			},
		},
		{
			// min is <nonnegativefloat>, so 0 is valid even on a reserved
			// capability. The spec's per-capability "Minimum Value" table is
			// the default used when min is OMITTED, not a floor on an explicit
			// value. Conformance: 3.3.1--amount-min-zero-valid.yaml.
			name: "amount.worker.vcpu min 0 ok (explicit zero is non-negative)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("0")}},
				}
			},
		},
		{
			name: "amount.worker.vcpu min 4 ok (above reserved minimum 1)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("4")}},
				}
			},
		},

		{
			name: "amount.worker.memory min 0 ok (at reserved minimum 0)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.memory", Min: new("0")}},
				}
			},
		},
		{
			name: "amount.worker.memory min -1 error (below reserved minimum 0)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.memory", Min: new("-1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/min",
		},

		{
			name: "amount.worker.gpu min 0 ok (at reserved minimum 0)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.gpu", Min: new("0")}},
				}
			},
		},
		{
			name: "amount.worker.gpu.memory min 0 ok (at reserved minimum 0)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.gpu.memory", Min: new("0")}},
				}
			},
		},
		{
			name: "amount.worker.disk.scratch min 0 ok (at reserved minimum 0)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.disk.scratch", Min: new("0")}},
				}
			},
		},

		// Non-reserved names are unconstrained — any value is accepted.
		{
			// A CUSTOM scope is unconstrained. "amount.worker.custom" is not a
			// valid stand-in: "worker" is a reserved scope, so an undefined name
			// under it is rejected by validateReservedScope — see
			// TestValidate_ReservedCapabilityScope.
			name: "non-reserved amount.custom.thing min 0 ok (unconstrained)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.custom.thing", Min: new("0")}},
				}
			},
		},

		// Reserved names are matched case-insensitively.
		{
			name: "reserved name case-insensitive Amount.Worker.VCPU min -1 error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "Amount.Worker.VCPU", Min: new("-1")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/min",
		},

		// Template references in Min/Max cannot be evaluated; skip the check.
		{
			name: "amount.worker.vcpu min with template ref skips reserved check",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("{{Param.MinCPU}}")}},
				}
			},
		},

		// Max is checked independently when present.
		{
			name: "amount.worker.vcpu min 1 max 0 error (max below reserved minimum)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("1"), Max: new("0")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/max",
		},
		{
			name: "amount.worker.vcpu max 1 only (no min) ok (at reserved minimum)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Max: new("1")}},
				}
			},
		},
		{
			name: "amount.worker.vcpu max 0 only (no min) error (max below reserved minimum)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Max: new("0")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/max",
		},

		// ── Reserved ATTRIBUTE allowed values ─────────────────────────────────

		{
			name: "attr.worker.os.family anyOf linux ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"linux"}}},
				}
			},
		},
		{
			name: "attr.worker.os.family anyOf windows ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"windows"}}},
				}
			},
		},
		{
			name: "attr.worker.os.family anyOf macos ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"macos"}}},
				}
			},
		},
		{
			name: "attr.worker.os.family anyOf linux windows ok (multiple valid values)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"linux", "windows"}}},
				}
			},
		},
		{
			name: "attr.worker.os.family anyOf solaris error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"solaris"}}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/anyOf/0",
		},
		{
			name: "attr.worker.os.family anyOf linux then solaris: second entry error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"linux", "solaris"}}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/anyOf/1",
		},

		{
			name: "attr.worker.cpu.arch allOf x86_64 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.cpu.arch", AllOf: []string{"x86_64"}}},
				}
			},
		},
		{
			name: "attr.worker.cpu.arch allOf arm64 ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.cpu.arch", AllOf: []string{"arm64"}}},
				}
			},
		},
		{
			name: "attr.worker.cpu.arch allOf sparc error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.cpu.arch", AllOf: []string{"sparc"}}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/allOf/0",
		},

		// Non-reserved attribute names are unconstrained — any value is accepted.
		{
			// As above: a custom scope, not the reserved "worker" one.
			name: "non-reserved attr.custom.thing any value ok",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.custom.thing", AnyOf: []string{"anything"}}},
				}
			},
		},

		// Reserved attribute names are matched case-insensitively.
		{
			name: "reserved attr name case-insensitive Attr.Worker.OS.Family anyOf solaris error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "Attr.Worker.OS.Family", AnyOf: []string{"solaris"}}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/attributes/0/anyOf/0",
		},

		// Attribute VALUES are compared case-insensitively (OpenJD
		// jobtemplate-2023-09: "This comparison is case-insensitive"), matching the
		// matcher's EqualFold value comparison: "Linux" is accepted as "linux".
		{
			name: "attr.worker.os.family anyOf Linux (uppercase) ok (values are case-insensitive)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Attributes: []openjd.AttributeRequirement{{Name: "attr.worker.os.family", AnyOf: []string{"Linux"}}},
				}
			},
		},

		// ── NaN/Inf escape in reserved amounts ─────────────────────────────────

		{
			name: "amount.worker.vcpu min NaN error (non-finite value)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("NaN")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/min",
		},
		{
			name: "amount.worker.vcpu min +Inf error (non-finite value)",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("+Inf")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/min",
		},

		// ── Non-numeric reserved-amount bound is a validation error ────────────
		// A non-numeric bound (that is not a template reference) is a structural
		// error, not a silently-skipped value.  Ensures the nilerr fix is correct.

		{
			name: "amount.worker.vcpu min non-numeric string error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu", Min: new("abc")}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0/min",
		},

		// ── Reserved amount with neither min nor max ───────────────────────────

		{
			// The spec requires at least one of min or max on every amount
			// ("Subject to the constraint that at least one of min or max must
			// be provided"). Conformance: 3.3.1--neither-min-nor-max.invalid.yaml.
			name: "amount with neither min nor max is an error",
			mutate: func(t *openjd.JobTemplate) {
				t.Steps[0].HostRequirements = &openjd.HostRequirements{
					Amounts: []openjd.AmountRequirement{{Name: "amount.worker.vcpu"}},
				}
			},
			wantPtr: "/steps/0/hostRequirements/amounts/0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reserved-value checks are structural correctness, not a gated
			// limit, so both EnforceLimits settings must behave identically.
			for _, enforce := range []bool{true, false} {
				tmpl := mustParse(t, minimalValidYAML())
				tc.mutate(tmpl)
				errs := openjd.ValidateWithOptions(tmpl, openjd.ValidateOptions{EnforceLimits: enforce})

				if tc.wantPtr == "" {
					if len(errs) != 0 {
						t.Fatalf("EnforceLimits=%v: expected no errors, got %v", enforce, errs)
					}
					continue
				}
				if !containsPointer(errs, tc.wantPtr) {
					t.Fatalf("EnforceLimits=%v: expected pointer %q, got %v", enforce, tc.wantPtr, errs)
				}
			}
		})
	}
}
