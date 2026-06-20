// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// Capability names are case-insensitive (OpenJD jobtemplate-2023-09), so the
// CPU reservation must be derived from amount.worker.vcpu regardless of case.
func TestRequiredCoresFromAmounts_CaseInsensitiveName(t *testing.T) {
	min2 := "2"
	got := requiredCoresFromAmounts([]store.StepAmountRequirement{
		{Name: "Amount.Worker.VCPU", Min: &min2},
	})
	if got == nil || *got != 2 {
		t.Fatalf("mixed-case vcpu min=2 -> %v, want 2", got)
	}
}

// The usagepool and computelocation namespace prefixes must be recognized
// case-insensitively when converting host requirements for the store.
func TestToStoreHostRequirements_CaseInsensitiveNamespaces(t *testing.T) {
	min1 := "1"
	hr := &HostRequirements{
		Amounts: []AmountRequirement{
			{Name: "Amount.Worker.UsagePool.maya", Min: &min1},
		},
		Attributes: []AttributeRequirement{
			{Name: "Attr.Worker.ComputeLocation", AnyOf: []string{"onprem"}},
		},
	}
	reqs, computeLoc := toStoreHostRequirements(hr)
	if computeLoc != "onprem" {
		t.Errorf("computeLoc = %q, want %q", computeLoc, "onprem")
	}
	if len(reqs.UsagePools) != 1 || reqs.UsagePools[0] != "maya" {
		t.Errorf("UsagePools = %v, want [maya]", reqs.UsagePools)
	}
}
