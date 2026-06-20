// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestSubmit_PopulatesRequiredCoresFromVcpuMin(t *testing.T) {
	// A step declaring amount.worker.vcpu min: 2 -> tasks carry RequiredCores=2.
	min2 := "2"
	got := requiredCoresFromAmounts([]store.StepAmountRequirement{
		{Name: "amount.worker.vcpu", Min: &min2},
	})
	if got == nil || *got != 2 {
		t.Fatalf("vcpu min=2 -> %v, want 2", got)
	}

	// Omitting amount.worker.vcpu entirely -> no reservation (nil).
	// Per OpenJD jobtemplate-2023-09, an omitted amount imposes no requirement.
	if requiredCoresFromAmounts(nil) != nil {
		t.Error("no amounts -> want nil")
	}
	if requiredCoresFromAmounts([]store.StepAmountRequirement{
		{Name: "amount.worker.memory", Min: &min2},
	}) != nil {
		t.Error("non-vcpu amount only -> want nil")
	}

	// A present amount.worker.vcpu with no explicit min defaults to the reserved
	// minimum of 1 vCPU (OpenJD: an omitted min defaults to the capability's
	// reserved minimum), so it reserves one core rather than the whole machine.
	one := 1
	wantOne := &one
	bare := requiredCoresFromAmounts([]store.StepAmountRequirement{
		{Name: "amount.worker.vcpu"},
	})
	if bare == nil || *bare != *wantOne {
		t.Errorf("vcpu present, no min/max -> %v, want 1", bare)
	}

	maxOnly := "8"
	withMax := requiredCoresFromAmounts([]store.StepAmountRequirement{
		{Name: "amount.worker.vcpu", Max: &maxOnly},
	})
	if withMax == nil || *withMax != *wantOne {
		t.Errorf("vcpu max-only (no min) -> %v, want 1 (spec default)", withMax)
	}
}
