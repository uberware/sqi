// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler_test

import (
	"testing"

	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// baseWorker returns a minimal online worker in farm "farm1" with generic
// hardware values that satisfy common test requirements.
func baseWorker() store.Worker {
	return store.Worker{
		ID:              "w1",
		FarmID:          "farm1",
		Hostname:        "host1",
		ComputeLocation: "onprem",
		OS:              "linux",
		OSVersion:       "6.1",
		CPUCount:        16,
		RAMMb:           32768,
		GPUInfo:         store.GPUInfo{Count: 2, VRAMMb: 24576},
		Tags:            map[string]string{},
		Status:          store.WorkerStatusOnline,
	}
}

// baseJob returns a minimal job in farm "farm1" and queue "q1".
func baseJob() store.Job {
	return store.Job{
		ID:      "job1",
		FarmID:  "farm1",
		QueueID: "q1",
	}
}

// baseStep returns a step with no host requirements and no compute location
// constraint.
func baseStep() store.Step {
	return store.Step{
		ID:    "s1",
		JobID: "job1",
		Name:  "Render",
	}
}

// ── Farm membership ───────────────────────────────────────────────────────────

func TestEligible_FarmMatch(t *testing.T) {
	w := baseWorker()
	j := baseJob()
	s := baseStep()
	if !scheduler.WorkerEligible(w, j, s, nil, nil) {
		t.Error("worker in same farm should be eligible")
	}
}

func TestEligible_FarmMismatch(t *testing.T) {
	w := baseWorker()
	j := baseJob()
	j.FarmID = "other-farm"
	s := baseStep()
	if scheduler.WorkerEligible(w, j, s, nil, nil) {
		t.Error("worker in different farm should not be eligible")
	}
}

func TestEligible_FarmEmpty_MatchesAnyFarm(t *testing.T) {
	w := baseWorker()
	w.FarmID = "" // unaffiliated worker
	j := baseJob()
	j.FarmID = "any-farm"
	s := baseStep()
	if !scheduler.WorkerEligible(w, j, s, nil, nil) {
		t.Error("unaffiliated worker (empty FarmID) should be eligible for any farm's tasks")
	}
}

// ── Queue affinity ────────────────────────────────────────────────────────────

// TestEligible_QueueAffinity pins that a worker's queue list admits exactly
// the jobs in the queues it names, and that only an empty list means every
// queue. A list naming only queues that do not exist (a deleted queue) admits
// nothing.
func TestEligible_QueueAffinity(t *testing.T) {
	tests := []struct {
		name     string
		queues   []string
		eligible bool
	}{
		{"no list accepts any queue", nil, true},
		{"single matching queue", []string{"qA"}, true},
		{"single other queue", []string{"qB"}, false},
		{"job queue second in list", []string{"qB", "qA"}, true},
		{"two other queues", []string{"qB", "qC"}, false},
		{"only a deleted queue", []string{"gone"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := baseWorker()
			w.QueueIDs = tt.queues
			j := baseJob()
			j.QueueID = "qA"
			reason, ok := scheduler.WorkerEligibleWithReason(w, j, baseStep(), nil, nil)
			if ok != tt.eligible {
				t.Fatalf("eligible = %v (reason %q), want %v", ok, reason, tt.eligible)
			}
			if !ok && reason != "queue affinity" {
				t.Errorf("reason = %q, want %q", reason, "queue affinity")
			}
		})
	}
}

// ── Compute-location affinity ─────────────────────────────────────────────────

func TestEligible_ComputeLocationEmpty_AcceptsAny(t *testing.T) {
	w := baseWorker()
	w.ComputeLocation = "cloud-aws"
	s := baseStep()
	s.ComputeLocation = "" // no constraint
	j := baseJob()
	if !scheduler.WorkerEligible(w, j, s, nil, nil) {
		t.Error("step with empty ComputeLocation should accept any worker location")
	}
}

func TestEligible_ComputeLocationMatch(t *testing.T) {
	w := baseWorker()
	w.ComputeLocation = "cloud-aws"
	s := baseStep()
	s.ComputeLocation = "cloud-aws"
	j := baseJob()
	if !scheduler.WorkerEligible(w, j, s, nil, nil) {
		t.Error("matching compute location should be eligible")
	}
}

func TestEligible_ComputeLocationMismatch(t *testing.T) {
	w := baseWorker()
	w.ComputeLocation = "onprem"
	s := baseStep()
	s.ComputeLocation = "cloud-aws"
	j := baseJob()
	if scheduler.WorkerEligible(w, j, s, nil, nil) {
		t.Error("mismatched compute location should not be eligible")
	}
}

// ── Amount requirements ───────────────────────────────────────────────────────

func TestEligible_AmountVCPU_Sufficient(t *testing.T) {
	w := baseWorker()
	w.CPUCount = 8
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.vcpu", Min: new("4")},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("worker with 8 vCPUs should satisfy min=4")
	}
}

func TestEligible_AmountVCPU_Insufficient(t *testing.T) {
	w := baseWorker()
	w.CPUCount = 2
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.vcpu", Min: new("4")},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("worker with 2 vCPUs should not satisfy min=4")
	}
}

func TestEligible_AmountRAM_Sufficient(t *testing.T) {
	w := baseWorker()
	w.RAMMb = 65536
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.memory.mb", Min: new("32768")},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("worker with 64 GiB should satisfy min=32 GiB")
	}
}

func TestEligible_AmountRAM_Insufficient(t *testing.T) {
	w := baseWorker()
	w.RAMMb = 8192
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.memory.mb", Min: new("32768")},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("worker with 8 GiB should not satisfy min=32 GiB")
	}
}

func TestEligible_AmountGPUCount_Sufficient(t *testing.T) {
	w := baseWorker()
	w.GPUInfo = store.GPUInfo{Count: 4, VRAMMb: 8192}
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.gpu.count", Min: new("2")},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("worker with 4 GPUs should satisfy min=2")
	}
}

func TestEligible_AmountGPUVRAM_Insufficient(t *testing.T) {
	w := baseWorker()
	w.GPUInfo = store.GPUInfo{Count: 1, VRAMMb: 8192}
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.gpu.memory.mb", Min: new("24576")},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("worker with 8 GiB VRAM should not satisfy min=24 GiB")
	}
}

func TestEligible_AmountMax_Enforced(t *testing.T) {
	w := baseWorker()
	w.CPUCount = 32
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			// Only workers with exactly 16 vCPUs.
			{Name: "amount.worker.vcpu", Min: new("16"), Max: new("16")},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("worker with 32 vCPUs should not satisfy max=16")
	}
}

func TestEligible_UnknownAmount_TreatedAsZero(t *testing.T) {
	w := baseWorker()
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.custom.thing", Min: new("1")},
		},
	}
	// Unknown capability is treated as zero; min=1 should not be satisfied.
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("unknown amount capability should be treated as 0")
	}
}

// ── Attribute requirements ────────────────────────────────────────────────────

func TestEligible_AttrOSFamily_AnyOf_Match(t *testing.T) {
	w := baseWorker()
	w.OS = "Linux" // case-insensitive
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AnyOf: []string{"linux", "windows"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("linux OS should match anyOf [linux, windows]")
	}
}

// A macOS worker self-reports runtime.GOOS, which is "darwin", but OpenJD's
// reserved attr.worker.os.family takes the token "macos", and the template
// validator (internal/openjd, reservedAttributeAllowed) accepts only
// {linux, windows, macos}, rejecting "darwin". So "macos" is the only spelling
// a template can use for a Mac, and it must match here, or no valid template
// could schedule on a Mac.
func TestEligible_AttrOSFamily_DarwinSatisfiesMacos_AnyOf(t *testing.T) {
	w := baseWorker()
	w.OS = "darwin"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AnyOf: []string{"macos", "windows"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("a darwin worker should satisfy anyOf [macos, windows]")
	}
}

func TestEligible_AttrOSFamily_DarwinSatisfiesMacos_AllOf(t *testing.T) {
	w := baseWorker()
	w.OS = "Darwin" // case-insensitive, as every other value here is
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AllOf: []string{"macos"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("a darwin worker should satisfy allOf [macos]")
	}
}

// The mapping is one-way: this attribute carries the specification's os
// family, not the Go runtime's. "darwin" is not a legal value for it — the
// validator rejects a template containing it — so a worker must not match it
// either, or the two halves disagree in the opposite direction. A
// template that wants the Go spelling has attr.worker.tag.os, which
// capabilities.Detect populates with runtime.GOOS verbatim.
func TestEligible_AttrOSFamily_DarwinDoesNotSatisfyDarwin(t *testing.T) {
	w := baseWorker()
	w.OS = "darwin"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AnyOf: []string{"darwin"}},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("attr.worker.os.family should carry the spec token, not runtime.GOOS")
	}
}

// Linux and Windows need no mapping — GOOS already spells them the spec's way.
// Pinned so a future normalization cannot quietly rewrite them too.
func TestEligible_AttrOSFamily_NonDarwinPassThrough(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			w := baseWorker()
			w.OS = goos
			s := baseStep()
			s.HostRequirements = &store.StepHostRequirements{
				Attributes: []store.StepAttributeRequirement{
					{Name: "attr.worker.os.family", AnyOf: []string{goos}},
				},
			}
			if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
				t.Errorf("a %s worker should satisfy anyOf [%s]", goos, goos)
			}
		})
	}
}

// ── attr.worker.cpu.arch ──────────────────────────────────────────────────────
//
// Same shape as os.family above: cpu.arch is a reserved attribute the template
// validator accepts with the enum {x86_64, arm64}, while the worker reports
// runtime.GOARCH. Without the mapping a template gating on it would validate,
// submit, and then wait forever.

func TestEligible_AttrCPUArch_Amd64SatisfiesX86_64(t *testing.T) {
	w := baseWorker()
	w.Arch = "amd64" // runtime.GOARCH
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.cpu.arch", AnyOf: []string{"x86_64"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("an amd64 worker should satisfy anyOf [x86_64]: GOARCH says amd64, OpenJD says x86_64")
	}
}

func TestEligible_AttrCPUArch_Arm64PassesThrough(t *testing.T) {
	w := baseWorker()
	w.Arch = "arm64" // GOARCH and the specification agree here
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.cpu.arch", AllOf: []string{"arm64"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("an arm64 worker should satisfy allOf [arm64]")
	}
}

func TestEligible_AttrCPUArch_Mismatch(t *testing.T) {
	w := baseWorker()
	w.Arch = "arm64"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.cpu.arch", AnyOf: []string{"x86_64"}},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("an arm64 worker must not satisfy anyOf [x86_64]")
	}
}

// A worker that predates the arch field reports nothing, and "" must not match
// anything — the alternative, treating unknown as universally acceptable, would
// dispatch x86_64 work to an arm64 host. Such a worker starts reporting as soon
// as it restarts.
func TestEligible_AttrCPUArch_UnreportedNeverMatches(t *testing.T) {
	w := baseWorker()
	w.Arch = ""
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.cpu.arch", AnyOf: []string{"x86_64", "arm64"}},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("a worker that has not reported its architecture must not match any arch requirement")
	}
}

// The mapping is one-way, exactly as os.family's is: "amd64" is Go's spelling,
// the validator rejects it in a template, so a worker must not match it either.
func TestEligible_AttrCPUArch_DoesNotSatisfyGoSpelling(t *testing.T) {
	w := baseWorker()
	w.Arch = "amd64"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.cpu.arch", AnyOf: []string{"amd64"}},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("attr.worker.cpu.arch should carry the spec token, not runtime.GOARCH")
	}
}

func TestEligible_AttrOSFamily_AnyOf_Mismatch(t *testing.T) {
	w := baseWorker()
	w.OS = "darwin"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AnyOf: []string{"linux", "windows"}},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("darwin OS should not match anyOf [linux, windows]")
	}
}

func TestEligible_AttrComputeLocation_Match(t *testing.T) {
	w := baseWorker()
	w.ComputeLocation = "cloud-aws"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.computelocation", AnyOf: []string{"cloud-aws", "cloud-gcp"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("cloud-aws compute location should match anyOf [cloud-aws, cloud-gcp]")
	}
}

func TestEligible_AttrCustomTag_Match(t *testing.T) {
	w := baseWorker()
	w.Tags = map[string]string{"software": "houdini"}
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.tag.software", AnyOf: []string{"houdini", "maya"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("tag software=houdini should match anyOf [houdini, maya]")
	}
}

func TestEligible_AttrCustomTag_Missing(t *testing.T) {
	w := baseWorker()
	w.Tags = map[string]string{} // no 'software' tag
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.tag.software", AnyOf: []string{"houdini"}},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("missing tag should not satisfy anyOf requirement")
	}
}

// TestEligible_AttrAutoDetectedTag_ValueTrue mirrors the DCC preset convention
// (presets/sqi/*.yaml declare attr.worker.tag.maya anyOf: ["true"]): a
// detected tag carries the value "true", and a tag recorded with an empty
// value would reject every auto-detected worker.
func TestEligible_AttrAutoDetectedTag_ValueTrue(t *testing.T) {
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.tag.maya", AnyOf: []string{"true"}},
		},
	}

	detected := baseWorker()
	detected.Tags = map[string]string{"maya": "true"}
	if !scheduler.WorkerEligible(detected, baseJob(), s, nil, nil) {
		t.Error("worker with tag maya=true should satisfy anyOf [true]")
	}

	presenceOnly := baseWorker()
	presenceOnly.Tags = map[string]string{"maya": ""}
	if scheduler.WorkerEligible(presenceOnly, baseJob(), s, nil, nil) {
		t.Error("worker with tag maya=\"\" (presence-only) must not satisfy anyOf [true]")
	}
}

func TestEligible_AttrAllOf_Satisfied(t *testing.T) {
	// AllOf with a single value that matches exactly.
	w := baseWorker()
	w.OS = "linux"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AllOf: []string{"linux"}},
		},
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("allOf [linux] should be satisfied when OS is linux")
	}
}

func TestEligible_AttrAllOf_NotSatisfied(t *testing.T) {
	w := baseWorker()
	w.OS = "linux"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AllOf: []string{"windows"}},
		},
	}
	if scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("allOf [windows] should not be satisfied when OS is linux")
	}
}

// ── Usage pool gating ─────────────────────────────────────────────────────────

func TestEligible_UsagePool_Available(t *testing.T) {
	pools := map[string]store.UsagePool{
		"maya": {ID: "p1", Name: "maya", MaxConcurrent: 5},
	}
	active := map[string]int{"maya": 2}
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		UsagePools: []string{"maya"},
	}
	if !scheduler.WorkerEligible(baseWorker(), baseJob(), s, pools, active) {
		t.Error("pool with 2/5 active should be available")
	}
}

func TestEligible_UsagePool_AtCapacity(t *testing.T) {
	pools := map[string]store.UsagePool{
		"maya": {ID: "p1", Name: "maya", MaxConcurrent: 3},
	}
	active := map[string]int{"maya": 3}
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		UsagePools: []string{"maya"},
	}
	if scheduler.WorkerEligible(baseWorker(), baseJob(), s, pools, active) {
		t.Error("pool at capacity (3/3) should block assignment")
	}
}

func TestEligible_UsagePool_NotConfigured(t *testing.T) {
	// Pool required but not present in the pools map — treat as at capacity.
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		UsagePools: []string{"unknown-pool"},
	}
	if scheduler.WorkerEligible(baseWorker(), baseJob(), s, nil, nil) {
		t.Error("unconfigured pool should block assignment")
	}
}

func TestEligible_UsagePool_Unlimited(t *testing.T) {
	// MaxConcurrent == 0 means unlimited; any active count is acceptable.
	pools := map[string]store.UsagePool{
		"unlimited": {ID: "p1", Name: "unlimited", MaxConcurrent: 0},
	}
	active := map[string]int{"unlimited": 9999}
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		UsagePools: []string{"unlimited"},
	}
	if !scheduler.WorkerEligible(baseWorker(), baseJob(), s, pools, active) {
		t.Error("unlimited pool (MaxConcurrent=0) should never block assignment")
	}
}

// ── Usage pool amounts via amount.worker.usagepool.* ─────────────────────────

func TestEligible_UsagePoolAmount_Skipped(t *testing.T) {
	// amount.worker.usagepool.* entries in Amounts are skipped by
	// checkAmounts and delegated to checkUsagePools.
	// Without a matching UsagePools entry the amount check passes (no
	// usage pool requirement is checked via Amounts alone).
	w := baseWorker()
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.usagepool.maya", Min: new("1")},
		},
		// UsagePools is empty — the amount is silently skipped.
	}
	if !scheduler.WorkerEligible(w, baseJob(), s, nil, nil) {
		t.Error("usagepool amount without UsagePools entry should not block")
	}
}

// ── WorkerEligibleWithReason ──────────────────────────────────────────────────

func TestEligibleWithReason_EligibleReason(t *testing.T) {
	reason, ok := scheduler.WorkerEligibleWithReason(baseWorker(), baseJob(), baseStep(), nil, nil)
	if !ok {
		t.Errorf("expected eligible; reason: %q", reason)
	}
	if reason != "eligible" {
		t.Errorf("reason: got %q, want %q", reason, "eligible")
	}
}

func TestEligibleWithReason_FarmMismatch(t *testing.T) {
	w := baseWorker()
	j := baseJob()
	j.FarmID = "other"
	reason, ok := scheduler.WorkerEligibleWithReason(w, j, baseStep(), nil, nil)
	if ok {
		t.Error("expected not eligible")
	}
	if reason != "farm mismatch" {
		t.Errorf("reason: got %q, want %q", reason, "farm mismatch")
	}
}

func TestEligibleWithReason_ComputeLocationMismatch(t *testing.T) {
	w := baseWorker()
	w.ComputeLocation = "onprem"
	s := baseStep()
	s.ComputeLocation = "cloud"
	reason, ok := scheduler.WorkerEligibleWithReason(w, baseJob(), s, nil, nil)
	if ok {
		t.Error("expected not eligible")
	}
	if reason != "compute location mismatch" {
		t.Errorf("reason: got %q, want %q", reason, "compute location mismatch")
	}
}

func TestEligibleWithReason_AmountNotMet(t *testing.T) {
	w := baseWorker()
	w.CPUCount = 1
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.vcpu", Min: new("8")},
		},
	}
	reason, ok := scheduler.WorkerEligibleWithReason(w, baseJob(), s, nil, nil)
	if ok {
		t.Error("expected not eligible")
	}
	if reason != "amount requirement not met" {
		t.Errorf("reason: got %q, want %q", reason, "amount requirement not met")
	}
}

func TestEligibleWithReason_AttributeNotMet(t *testing.T) {
	w := baseWorker()
	w.OS = "darwin"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AnyOf: []string{"linux"}},
		},
	}
	reason, ok := scheduler.WorkerEligibleWithReason(w, baseJob(), s, nil, nil)
	if ok {
		t.Error("expected not eligible")
	}
	if reason != "attribute requirement not met" {
		t.Errorf("reason: got %q, want %q", reason, "attribute requirement not met")
	}
}

func TestEligibleWithReason_UsagePool(t *testing.T) {
	pools := map[string]store.UsagePool{
		"nuke": {ID: "p1", Name: "nuke", MaxConcurrent: 1},
	}
	active := map[string]int{"nuke": 1}
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		UsagePools: []string{"nuke"},
	}
	reason, ok := scheduler.WorkerEligibleWithReason(baseWorker(), baseJob(), s, pools, active)
	if ok {
		t.Error("expected not eligible")
	}
	if reason != "usage pool at capacity" {
		t.Errorf("reason: got %q, want %q", reason, "usage pool at capacity")
	}
}

// ── Combined requirements ─────────────────────────────────────────────────────

func TestEligible_AllRequirements_Satisfied(t *testing.T) {
	w := baseWorker()
	w.OS = "linux"
	w.CPUCount = 16
	w.RAMMb = 65536
	w.ComputeLocation = "render-farm"
	w.Tags = map[string]string{"renderer": "vray"}

	pools := map[string]store.UsagePool{
		"vray": {ID: "p1", Name: "vray", MaxConcurrent: 10},
	}
	active := map[string]int{"vray": 3}

	s := baseStep()
	s.ComputeLocation = "render-farm"
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.vcpu", Min: new("8")},
			{Name: "amount.worker.memory.mb", Min: new("32768")},
		},
		Attributes: []store.StepAttributeRequirement{
			{Name: "attr.worker.os.family", AnyOf: []string{"linux"}},
			{Name: "attr.worker.tag.renderer", AnyOf: []string{"vray", "arnold"}},
		},
		UsagePools: []string{"vray"},
	}

	if !scheduler.WorkerEligible(w, baseJob(), s, pools, active) {
		t.Error("all requirements satisfied: expected eligible")
	}
}

func TestEligible_FirstFailure_FarmBlocksRest(t *testing.T) {
	// Even when all other requirements are satisfied, a farm mismatch should
	// block the worker.
	w := baseWorker()
	j := baseJob()
	j.FarmID = "wrong-farm"
	s := baseStep()
	s.HostRequirements = &store.StepHostRequirements{
		Amounts: []store.StepAmountRequirement{
			{Name: "amount.worker.vcpu", Min: new("1")},
		},
	}
	if scheduler.WorkerEligible(w, j, s, nil, nil) {
		t.Error("farm mismatch should block even when other requirements are met")
	}
}

// ── Priority ordering (informational) ────────────────────────────────────────

// TestEligible_PriorityOrdering verifies that WorkerEligible is purely a
// boolean predicate and does not itself order workers.  Priority ordering is
// handled upstream by [store.TaskStore.ListReadyTasks], which sorts tasks by
// job priority descending before handing them to the scheduler.  This test
// documents the responsibility boundary.
func TestEligible_PriorityOrdering(t *testing.T) {
	w := baseWorker()
	hiPriJob := baseJob()
	hiPriJob.Priority = 100
	loPriJob := baseJob()
	loPriJob.Priority = 1

	// Both jobs should be independently eligible — priority is not a filter.
	if !scheduler.WorkerEligible(w, hiPriJob, baseStep(), nil, nil) {
		t.Error("high-priority job: expected eligible")
	}
	if !scheduler.WorkerEligible(w, loPriJob, baseStep(), nil, nil) {
		t.Error("low-priority job: expected eligible")
	}
}
