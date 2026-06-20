// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// A usage-pool name is the trailing segment of a (case-insensitive) capability
// name, so a step requirement must match a registered pool regardless of case:
// an operator pool "Maya" satisfies a template requirement "maya".
func TestUsageContext_PoolNameCaseInsensitive(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "")

	if _, err := st.CreateUsagePool(
		t.Context(),
		store.UsagePool{ID: uuid.NewString(), Name: "Maya", MaxConcurrent: 2},
	); err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	step := store.Step{HostRequirements: &store.StepHostRequirements{UsagePools: []string{"maya"}}}

	pools, counts, err := s.buildUsageContext(t.Context(), step)
	if err != nil {
		t.Fatalf("buildUsageContext: %v", err)
	}
	if rej, ok := checkUsagePools(step.HostRequirements.UsagePools, pools, counts); !ok {
		t.Errorf("requirement %q should match registered pool \"Maya\"; got rejection %v",
			"maya", rej)
	}
}
