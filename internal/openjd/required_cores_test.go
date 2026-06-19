// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestSubmit_PopulatesRequiredCoresFromVcpuMin(t *testing.T) {
	// A step declaring amount.worker.vcpu min: 2 -> tasks carry RequiredCores=2.
	// A step with no vcpu amount -> tasks carry RequiredCores=nil.
	min2 := "2"
	got := requiredCoresFromAmounts([]store.StepAmountRequirement{
		{Name: "amount.worker.vcpu", Min: &min2},
	})
	if got == nil || *got != 2 {
		t.Fatalf("vcpu min=2 -> %v, want 2", got)
	}
	if requiredCoresFromAmounts(nil) != nil {
		t.Error("no amounts -> want nil")
	}
	maxOnly := "8"
	if requiredCoresFromAmounts([]store.StepAmountRequirement{
		{Name: "amount.worker.vcpu", Max: &maxOnly},
	}) != nil {
		t.Error("max-only (no min) -> want nil (undeclared)")
	}
}
