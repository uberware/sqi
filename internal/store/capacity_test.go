// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
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
	if w.FarmID != "f1" || w.QueueID != "q1" {
		t.Errorf("farm/queue = %q/%q, want f1/q1", w.FarmID, w.QueueID)
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

	for _, tc := range []struct {
		name   string
		queues []string
		want   string
	}{
		{"no queues", nil, ""},
		{"one queue", []string{"q1"}, "q1"},
		{"two queues", []string{"q1", "q2"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := (store.CapacityGroup{QueueIDs: tc.queues}).ProfileWorker().QueueID; got != tc.want {
				t.Errorf("QueueID = %q, want %q", got, tc.want)
			}
		})
	}
}
