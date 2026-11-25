// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestCapacityGroup_ProfileWorker(t *testing.T) {
	t.Parallel()

	tags := map[string]string{"houdini": "20.5"}
	exts := []string{"SQI_LOG_CHECKS"}
	g := store.CapacityGroup{
		FarmID:   "f1",
		QueueIDs: []string{"q1"},
		Profile: store.MachineProfile{
			CPUCount: 32, RAMMb: 65536, GPUCount: 2, GPUMemoryMb: 24576,
			OS: "linux", Arch: "amd64", ComputeLocation: "onprem",
			Tags: tags, Extensions: exts,
			ExprLimits: store.WorkerExprLimits{OperationLimit: 7},
		},
	}

	w := g.ProfileWorker()
	if w.FarmID != "f1" || !slices.Equal(w.QueueIDs, []string{"q1"}) {
		t.Errorf("farm/queues = %q/%v, want f1/[q1]", w.FarmID, w.QueueIDs)
	}
	if w.CPUCount != 32 || w.RAMMb != 65536 || w.GPUInfo.Count != 2 || w.GPUInfo.VRAMMb != 24576 {
		t.Errorf("hardware = %d cpu %d ram %+v gpu", w.CPUCount, w.RAMMb, w.GPUInfo)
	}
	if w.OS != "linux" || w.Arch != "amd64" || w.ComputeLocation != "onprem" {
		t.Errorf("os/arch/location = %q/%q/%q", w.OS, w.Arch, w.ComputeLocation)
	}
	if w.Tags["houdini"] != "20.5" || len(w.Extensions) != 1 || w.ExprLimits.OperationLimit != 7 {
		t.Errorf("tags/extensions/limits not carried: %+v %v %+v", w.Tags, w.Extensions, w.ExprLimits)
	}

	// The worker owns copies: editing the group's profile afterwards must not
	// reach it.
	tags["houdini"] = "changed"
	exts[0] = "changed"
	if w.Tags["houdini"] != "20.5" || w.Extensions[0] != "SQI_LOG_CHECKS" {
		t.Error("ProfileWorker shares the profile's tag map or extension slice")
	}

	// The profile worker serves exactly the group's queues, as a member
	// registered with the same list does, and owns its copy of the list.
	for _, tc := range []struct {
		name   string
		queues []string
	}{
		{"no queues", nil},
		{"one queue", []string{"q1"}},
		{"two queues", []string{"q1", "q2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			queues := slices.Clone(tc.queues)
			got := (store.CapacityGroup{QueueIDs: queues}).ProfileWorker().QueueIDs
			if !slices.Equal(got, tc.queues) || (tc.queues == nil && got != nil) {
				t.Fatalf("QueueIDs = %#v, want %#v", got, tc.queues)
			}
			if len(queues) > 0 {
				queues[0] = "changed"
				if got[0] == "changed" {
					t.Error("ProfileWorker shares the group's queue slice")
				}
			}
		})
	}
}
