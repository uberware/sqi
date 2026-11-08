// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/store"
)

// TestProfileMismatch pins each way a sized group's member can fall short of
// the declared profile, and that only what the profile declares is compared.
func TestProfileMismatch(t *testing.T) {
	// base is a member matching profile below exactly.
	base := func() store.Worker {
		return store.Worker{
			ID: "w1", CPUCount: 8, RAMMb: 16384, OS: "linux", Arch: "amd64",
			GPUInfo:         store.GPUInfo{Count: 2, VRAMMb: 8192},
			ComputeLocation: "aws-us-east-1",
			Tags:            map[string]string{"houdini": "20.5"},
			Extensions:      []string{"SQI_LOG_CHECKS"},
		}
	}
	profile := store.MachineProfile{
		CPUCount: 8, RAMMb: 16384, GPUCount: 2, GPUMemoryMb: 8192,
		OS: "linux", Arch: "x86_64", ComputeLocation: "aws-us-east-1",
		Tags:       map[string]string{"houdini": "20.5"},
		Extensions: []string{"SQI_LOG_CHECKS"},
	}
	tests := []struct {
		name    string
		profile func(*store.MachineProfile)
		queues  []string
		worker  func(*store.Worker)
		want    []string // substrings of the reason; none means a match
	}{
		{name: "exact match"},
		{name: "more than declared is no mismatch", worker: func(w *store.Worker) {
			w.CPUCount, w.RAMMb, w.GPUInfo = 64, 1<<20, store.GPUInfo{Count: 8, VRAMMb: 81920}
			w.Tags["extra"] = "x"
			w.Extensions = append(w.Extensions, "OTHER")
		}},
		{name: "undeclared fields are not compared", profile: func(p *store.MachineProfile) {
			*p = store.MachineProfile{}
		}, worker: func(w *store.Worker) { *w = store.Worker{ID: "w1"} }},
		{
			name: "cpu count", worker: func(w *store.Worker) { w.CPUCount = 4 },
			want: []string{"cpu_count 4 is below the profile's 8"},
		},
		{
			name: "ram", worker: func(w *store.Worker) { w.RAMMb = 8192 },
			want: []string{"ram_mb 8192 is below the profile's 16384"},
		},
		{
			name: "gpu count", worker: func(w *store.Worker) { w.GPUInfo.Count = 1 },
			want: []string{"gpu_count 1 is below the profile's 2"},
		},
		{
			name: "gpu memory", worker: func(w *store.Worker) { w.GPUInfo.VRAMMb = 4096 },
			want: []string{"gpu_memory_mb 4096 is below the profile's 8192"},
		},
		{
			name: "darwin matches macos", profile: func(p *store.MachineProfile) { p.OS = "macos" },
			worker: func(w *store.Worker) { w.OS = "darwin" },
		},
		{
			name: "os differs", worker: func(w *store.Worker) { w.OS = "windows" },
			want: []string{`os "windows" differs from the profile's "linux"`},
		},
		{
			name: "amd64 matches x86_64", profile: func(p *store.MachineProfile) { p.Arch = "x86_64" },
			worker: func(w *store.Worker) { w.Arch = "amd64" },
		},
		{
			name: "x86_64 matches amd64", profile: func(p *store.MachineProfile) { p.Arch = "amd64" },
			worker: func(w *store.Worker) { w.Arch = "x86_64" },
		},
		{
			name: "arch differs", worker: func(w *store.Worker) { w.Arch = "arm64" },
			want: []string{`arch "arm64" differs from the profile's "x86_64"`},
		},
		{
			name: "compute location differs", worker: func(w *store.Worker) { w.ComputeLocation = "onprem" },
			want: []string{`compute_location "onprem" differs from the profile's "aws-us-east-1"`},
		},
		{name: "tag key and value case fold", profile: func(p *store.MachineProfile) {
			p.Tags = map[string]string{"renderer": "Karma"}
		}, worker: func(w *store.Worker) {
			w.Tags = map[string]string{"Renderer": "karma"}
		}},
		{
			name: "tag value differs", worker: func(w *store.Worker) { w.Tags["houdini"] = "19.5" },
			want: []string{`tag houdini is "19.5", the profile declares "20.5"`},
		},
		{
			name: "tag missing", worker: func(w *store.Worker) { w.Tags = nil },
			want: []string{`tag houdini is missing (the profile declares "20.5")`},
		},
		{
			name: "extension missing", worker: func(w *store.Worker) { w.Extensions = nil },
			want: []string{"extension SQI_LOG_CHECKS is not advertised"},
		},
		{name: "expr cap below the profile's", profile: func(p *store.MachineProfile) {
			p.ExprLimits = store.WorkerExprLimits{OperationLimit: 50_000, MemoryLimit: 1 << 20}
		}, worker: func(w *store.Worker) {
			w.ExprLimits = store.WorkerExprLimits{OperationLimit: 20_000, MemoryLimit: 1 << 20}
		}, want: []string{"expr.operation_limit 20000 is below the profile's 50000"}},
		{name: "expr cap the worker does not advertise reads as legacy", profile: func(p *store.MachineProfile) {
			p.ExprLimits = store.WorkerExprLimits{OperationLimit: legacyWorkerExprCaps.OperationLimit + 1}
		}, want: []string{"expr.operation_limit"}},
		{name: "expr cap the profile does not declare is not compared", worker: func(w *store.Worker) {
			w.ExprLimits = store.WorkerExprLimits{OperationLimit: 1}
		}},
		{name: "no queue affinity covers any group", queues: []string{"q1"}},
		{
			name: "affinity to the group's only queue", queues: []string{"q1"},
			worker: func(w *store.Worker) { w.QueueID = "q1" },
		},
		{
			name: "affinity against every queue of the farm", worker: func(w *store.Worker) { w.QueueID = "q1" },
			want: []string{"queue affinity q1 does not cover the group's queues (every queue in the farm)"},
		},
		{
			name: "affinity covering a subset", queues: []string{"q1", "q2"},
			worker: func(w *store.Worker) { w.QueueID = "q1" },
			want:   []string{"queue affinity q1 does not cover the group's queues q1, q2"},
		},
		{
			name: "affinity to a queue the group does not serve", queues: []string{"q2"},
			worker: func(w *store.Worker) { w.QueueID = "q1" },
			want:   []string{"queue affinity q1 does not cover the group's queues q2"},
		},
		{name: "several shortfalls are all reported", worker: func(w *store.Worker) {
			w.CPUCount, w.Tags = 2, nil
		}, want: []string{"cpu_count 2", "tag houdini is missing", "; "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := sizedGroup("burst", nil)
			g.Profile = profile
			g.Profile.Tags = map[string]string{"houdini": "20.5"}
			g.QueueIDs = tc.queues
			if tc.profile != nil {
				tc.profile(&g.Profile)
			}
			w := base()
			if tc.worker != nil {
				tc.worker(&w)
			}
			got := profileMismatch(g, w)
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("profileMismatch = %q, want a match", got)
				}
				return
			}
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("profileMismatch = %q, want it to contain %q", got, sub)
				}
			}
		})
	}
}

// A named group declares no profile its members are held to: however its
// members register, it reports no mismatch.
func TestRecordMismatches_NamedGroupGetsNone(t *testing.T) {
	_, s, logs := newCapacityFixture(t, nil)
	sized := sizedGroup("burst", func(g *store.CapacityGroup) {
		g.Profile.Tags = map[string]string{"houdini": "20.5"}
	})
	named := sized
	named.ID, named.Name, named.Kind = "named-id", "onprem", store.CapacityGroupNamed
	small := capacity.Member{Worker: store.Worker{
		ID: "w1", Status: store.WorkerStatusOnline, CPUCount: 1, OS: "windows", Arch: "arm64",
	}}
	groups := []store.CapacityGroup{named, sized}
	statuses := make([]capacity.Status, len(groups))
	members := map[string][]capacity.Member{named.ID: {small}, sized.ID: {small}}

	s.recordMismatches(t.Context(), groups, members, statuses)

	if len(statuses[0].Mismatches) != 0 {
		t.Errorf("named group mismatches = %+v, want none", statuses[0].Mismatches)
	}
	if len(statuses[1].Mismatches) != 1 {
		t.Errorf("sized group mismatches = %+v, want the one member", statuses[1].Mismatches)
	}
	if n := strings.Count(logs.String(), "group=onprem"); n != 0 {
		t.Errorf("named group logged %d mismatch warnings, want none:\n%s", n, logs.String())
	}
}
