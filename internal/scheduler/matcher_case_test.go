// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler_test

import (
	"testing"

	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
)

// Per OpenJD jobtemplate-2023-09, capability NAMES are case-insensitive ("All
// comparisons between strings of this type must be case-insensitive"). The
// matcher must therefore resolve well-known amount/attribute names and the
// usagepool/tag namespace prefixes regardless of the case used in the template.
func TestEligible_CapabilityNameCaseInsensitive(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(w *store.Worker, s *store.Step)
		eligible bool
	}{
		{
			name: "mixed-case amount.worker.vcpu resolves",
			mutate: func(w *store.Worker, s *store.Step) {
				w.CPUCount = 8
				s.HostRequirements = &store.StepHostRequirements{
					Amounts: []store.StepAmountRequirement{
						{Name: "Amount.Worker.VCPU", Min: new("4")},
					},
				}
			},
			eligible: true,
		},
		{
			name: "mixed-case attr.worker.os.family resolves",
			mutate: func(w *store.Worker, s *store.Step) {
				w.OS = "linux"
				s.HostRequirements = &store.StepHostRequirements{
					Attributes: []store.StepAttributeRequirement{
						{Name: "Attr.Worker.OS.Family", AnyOf: []string{"linux"}},
					},
				}
			},
			eligible: true,
		},
		{
			name: "mixed-case tag namespace prefix recognized",
			mutate: func(w *store.Worker, s *store.Step) {
				w.Tags = map[string]string{"software": "houdini"}
				s.HostRequirements = &store.StepHostRequirements{
					Attributes: []store.StepAttributeRequirement{
						{Name: "Attr.Worker.Tag.software", AnyOf: []string{"houdini"}},
					},
				}
			},
			eligible: true,
		},
		{
			name: "tag key matched case-insensitively against worker tags",
			mutate: func(w *store.Worker, s *store.Step) {
				w.Tags = map[string]string{"Renderer": "arnold"}
				s.HostRequirements = &store.StepHostRequirements{
					Attributes: []store.StepAttributeRequirement{
						{Name: "attr.worker.tag.renderer", AnyOf: []string{"arnold"}},
					},
				}
			},
			eligible: true,
		},
		{
			name: "mixed-case usagepool prefix is skipped, not treated as an unknown amount",
			mutate: func(_ *store.Worker, s *store.Step) {
				s.HostRequirements = &store.StepHostRequirements{
					Amounts: []store.StepAmountRequirement{
						{Name: "Amount.Worker.UsagePool.maya", Min: new("1")},
					},
				}
			},
			eligible: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := baseWorker()
			s := baseStep()
			tc.mutate(&w, &s)
			if got := scheduler.WorkerEligible(w, baseJob(), s, nil, nil); got != tc.eligible {
				t.Errorf("WorkerEligible = %v, want %v", got, tc.eligible)
			}
		})
	}
}
