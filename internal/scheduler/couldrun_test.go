// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

func TestCouldRun(t *testing.T) {
	t.Parallel()

	base := store.MachineProfile{
		CPUCount: 32, RAMMb: 65536, OS: "linux", Arch: "amd64",
		Tags: map[string]string{"blender": "4.2"},
	}
	group := func(mut func(*store.CapacityGroup)) store.CapacityGroup {
		g := store.CapacityGroup{FarmID: "f1", QueueIDs: []string{"q1"}, Profile: base}
		if mut != nil {
			mut(&g)
		}
		return g
	}
	attrStep := func(name string, anyOf ...string) store.Step {
		return store.Step{HostRequirements: &store.StepHostRequirements{
			Attributes: []store.StepAttributeRequirement{{Name: name, AnyOf: anyOf}},
		}}
	}
	poolStep := store.Step{HostRequirements: &store.StepHostRequirements{UsagePools: []string{"licenses"}}}
	exprJob := store.Job{FarmID: "f1", QueueID: "q1", ExtensionsRecorded: true, DeclaredExtensions: []string{"EXPR"}}
	job := store.Job{FarmID: "f1", QueueID: "q1"}
	fullPool := map[string]store.UsagePool{"licenses": {Name: "licenses", MaxConcurrent: 1}}

	raised := openjd.DefaultExprLimits()
	raised.TemplatePositions = openjd.MaxExprTemplatePositions
	raised = raised.Normalized()

	tests := []struct {
		name  string
		group store.CapacityGroup
		job   store.Job
		step  store.Step
		cores int
		pools map[string]store.UsagePool
		srv   *openjd.ExprLimits // nil = server defaults
		want  bool
	}{
		{"tag present", group(nil), job, attrStep("attr.worker.tag.blender", "4.2"), 1, nil, nil, true},
		{"tag absent", group(nil), job, attrStep("attr.worker.tag.maya", "2025"), 1, nil, nil, false},
		{"os family matches", group(nil), job, attrStep("attr.worker.os.family", "linux"), 1, nil, nil, true},
		{"os family mismatch", group(nil), job, attrStep("attr.worker.os.family", "windows"), 1, nil, nil, false},
		{"arch is translated", group(nil), job, attrStep("attr.worker.cpu.arch", "x86_64"), 1, nil, nil, true},
		{
			"compute location matches",
			group(func(g *store.CapacityGroup) { g.Profile.ComputeLocation = "cloud" }),
			job,
			store.Step{ComputeLocation: "cloud"},
			1, nil, nil, true,
		},
		{
			"compute location mismatch",
			group(func(g *store.CapacityGroup) { g.Profile.ComputeLocation = "onprem" }),
			job,
			store.Step{ComputeLocation: "cloud"},
			1, nil, nil, false,
		},
		{"one queue, job in it", group(nil), job, store.Step{}, 1, nil, nil, true},
		{
			"one queue, job in another",
			group(nil),
			store.Job{FarmID: "f1", QueueID: "q2"},
			store.Step{},
			1, nil, nil, false,
		},
		{
			"two queues, job in either",
			group(func(g *store.CapacityGroup) { g.QueueIDs = []string{"q1", "q2"} }),
			store.Job{FarmID: "f1", QueueID: "q2"},
			store.Step{},
			1, nil, nil, true,
		},
		{"other farm", group(nil), store.Job{FarmID: "f2", QueueID: "q1"}, store.Step{}, 1, nil, nil, false},
		{
			"EXPR job, profile declares no limits, default server",
			group(nil), exprJob,
			store.Step{},
			1, nil, nil, true,
		},
		{
			"EXPR job, profile declares no limits, server raised past what an unadvertising worker enforces",
			group(nil), exprJob,
			store.Step{},
			1, nil, &raised, false,
		},
		{
			"EXPR job, profile declares low limits",
			group(func(g *store.CapacityGroup) {
				g.Profile.ExprLimits = store.WorkerExprLimits{
					OperationLimit: 100, MemoryLimit: 100, AssignmentPositions: 100,
					AssignmentRetainedBytes: 100, LetRetainedBytes: 100,
				}
			}),
			exprJob,
			store.Step{},
			1, nil, nil, false,
		},
		{
			"non-EXPR job ignores the profile's low limits",
			group(func(g *store.CapacityGroup) {
				g.Profile.ExprLimits = store.WorkerExprLimits{OperationLimit: 100}
			}),
			job,
			store.Step{},
			1, nil, &raised, true,
		},
		{"usage pool at its cap is still runnable", group(nil), job, poolStep, 1, fullPool, nil, true},
		{"usage pool that does not exist is refused", group(nil), job, poolStep, 1, nil, nil, false},
		{"required cores above the profile", group(nil), job, store.Step{}, 64, nil, nil, false},
		{"required cores equal to the profile", group(nil), job, store.Step{}, 32, nil, nil, true},
		{
			"undeclared cores on a profile with none",
			group(func(g *store.CapacityGroup) { g.Profile.CPUCount = 0 }),
			job,
			store.Step{},
			64, nil, nil, true,
		},
		{"no required cores", group(nil), job, store.Step{}, 0, nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Scheduler{cfg: Config{ExprLimits: openjd.DefaultExprLimits().Normalized()}}
			if tc.srv != nil {
				s.cfg.ExprLimits = *tc.srv
			}
			if got := s.couldRun(tc.group.ProfileWorker(), tc.job, tc.step, tc.cores, tc.pools); got != tc.want {
				t.Errorf("couldRun = %v, want %v", got, tc.want)
			}
		})
	}
}

// A real worker that is disabled or offline could run the work if an operator
// re-enabled it, and couldRun answers that capability question, not the
// lease path's liveness one.
func TestCouldRun_IgnoresDisabledAndStatus(t *testing.T) {
	t.Parallel()

	s := &Scheduler{cfg: Config{ExprLimits: openjd.DefaultExprLimits().Normalized()}}
	job := store.Job{FarmID: "f1", QueueID: "q1"}
	for _, w := range []store.Worker{
		{FarmID: "f1", CPUCount: 8, Disabled: true, Status: store.WorkerStatusOnline},
		{FarmID: "f1", CPUCount: 8, Status: store.WorkerStatusOffline},
	} {
		if !s.couldRun(w, job, store.Step{}, 4, nil) {
			t.Errorf("couldRun(disabled=%v status=%v) = false, want true", w.Disabled, w.Status)
		}
	}
}
