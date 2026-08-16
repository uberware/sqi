// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// matcher.go implements worker-to-task eligibility checking.
//
// # Capability name conventions
//
// The OpenJD hostRequirements block declares requirements using dot-separated
// capability names.  sqi defines the following well-known names; all others are
// treated as custom worker tag lookups via the "attr.worker.tag.<key>" prefix.
//
// Amount requirements (quantifiable, compared as float64):
//
//	amount.worker.vcpu            → worker.CPUCount
//	amount.worker.memory.mb       → worker.RAMMb
//	amount.worker.gpu.count       → worker.GPUInfo.Count
//	amount.worker.gpu.memory.mb   → worker.GPUInfo.VRAMMb (per-device; all GPUs assumed identical)
//	amount.worker.usagepool.<n>   → usage pool named n must have capacity
//
// Note: gpu.memory.mb matches against the per-device VRAM of a worker's
// homogeneous GPU pool (see [store.GPUInfo]).  A worker with mixed GPU models
// should advertise the lowest per-device VRAM to avoid over-scheduling.
// Per-device heterogeneous matching is deferred to a future phase.
//
// Attribute requirements (categorical, compared as strings):
//
//	attr.worker.os.family         → osFamily(worker.OS)  ("darwin" → "macos")
//	attr.worker.os.version        → worker.OSVersion
//	attr.worker.cpu.arch          → cpuArch(worker.Arch)  ("amd64" → "x86_64")
//	attr.worker.computelocation   → worker.ComputeLocation
//	attr.worker.tag.<key>         → worker.Tags[key]  (empty if absent)
//
// # Matching rules
//
// A worker is eligible for a task when ALL of the following hold:
//  1. Farm membership: worker.FarmID == job.FarmID
//  2. Queue affinity: worker.QueueID == "" (any queue) or worker.QueueID == job.QueueID
//  3. Compute-location affinity: step.ComputeLocation == "" (any) or matches worker.ComputeLocation
//  4. Amount requirements: all are satisfied by the worker's hardware values
//  5. Attribute requirements: all AnyOf/AllOf constraints are satisfied
//  6. Usage pool availability: all required pools have remaining capacity

import (
	"strconv"
	"strings"

	"github.com/uberware/sqi/internal/store"
)

// matchRejection describes why a worker failed eligibility.
// Used for debug logging; not exposed to callers.
type matchRejection int

const (
	rejectNone                 matchRejection = iota
	rejectFarm                                // worker not in the job's farm
	rejectQueue                               // worker queue affinity excludes this job's queue
	rejectComputeLocation                     // worker not in the required compute location
	rejectAmountRequirement                   // worker lacks required hardware capacity
	rejectAttributeRequirement                // worker missing required capability attribute
	rejectUsagePool                           // required usage pool is at capacity
)

func (r matchRejection) String() string {
	switch r {
	case rejectFarm:
		return "farm mismatch"
	case rejectQueue:
		return "queue affinity"
	case rejectComputeLocation:
		return "compute location mismatch"
	case rejectAmountRequirement:
		return "amount requirement not met"
	case rejectAttributeRequirement:
		return "attribute requirement not met"
	case rejectUsagePool:
		return "usage pool at capacity"
	default:
		return "eligible"
	}
}

// WorkerEligible reports whether worker w can run a task that belongs to step s
// and job j, given the current usage pool state.
//
// pools maps pool name → [store.UsagePool].
// activeCounts maps pool name → current active claim count.
//
// Both maps may be nil or empty when no usage pools exist.
func WorkerEligible(
	worker store.Worker,
	job store.Job,
	step store.Step,
	pools map[string]store.UsagePool,
	activeCounts map[string]int,
) bool {
	reason, ok := workerEligible(worker, job, step, pools, activeCounts)
	_ = reason // available for debug logging in the caller
	return ok
}

// WorkerEligibleWithReason is the same as [WorkerEligible] but also returns a
// human-readable rejection reason for diagnostic logging.
func WorkerEligibleWithReason(
	worker store.Worker,
	job store.Job,
	step store.Step,
	pools map[string]store.UsagePool,
	activeCounts map[string]int,
) (reason string, eligible bool) {
	r, ok := workerEligible(worker, job, step, pools, activeCounts)
	return r.String(), ok
}

// workerEligible is the internal implementation shared by both exported wrappers.
func workerEligible(
	worker store.Worker,
	job store.Job,
	step store.Step,
	pools map[string]store.UsagePool,
	activeCounts map[string]int,
) (matchRejection, bool) {
	// ── 1. Farm membership ────────────────────────────────────────────────────
	// An empty worker FarmID means the worker is unaffiliated and accepts
	// tasks from any farm (analogous to how an empty scheduler FarmID means
	// "manage all farms"). A non-empty worker FarmID must match the job's farm.
	if worker.FarmID != "" && worker.FarmID != job.FarmID {
		return rejectFarm, false
	}

	// ── 2. Queue affinity ─────────────────────────────────────────────────────
	// A worker with an empty QueueID accepts tasks from any queue in its farm.
	// A worker with a non-empty QueueID is dedicated to that queue only.
	if worker.QueueID != "" && worker.QueueID != job.QueueID {
		return rejectQueue, false
	}

	// ── 3. Compute-location affinity ─────────────────────────────────────────
	if step.ComputeLocation != "" && step.ComputeLocation != worker.ComputeLocation {
		return rejectComputeLocation, false
	}

	// ── 4–6. Host requirements ────────────────────────────────────────────────
	if step.HostRequirements != nil {
		if r, ok := checkAmounts(step.HostRequirements.Amounts, worker, pools, activeCounts); !ok {
			return r, false
		}
		if r, ok := checkAttributes(step.HostRequirements.Attributes, worker); !ok {
			return r, false
		}
		if r, ok := checkUsagePools(step.HostRequirements.UsagePools, pools, activeCounts); !ok {
			return r, false
		}
	}

	return rejectNone, true
}

// ── Amount requirements ───────────────────────────────────────────────────────

// workerAmountValue maps well-known amount capability names to functions that
// read the corresponding integer value from a [store.Worker].
// Names not present in this map resolve to zero (treated as unsatisfied if a
// non-zero minimum is required).
var workerAmountValue = map[string]func(store.Worker) int{
	"amount.worker.vcpu":          func(w store.Worker) int { return w.CPUCount },
	"amount.worker.memory.mb":     func(w store.Worker) int { return w.RAMMb },
	"amount.worker.gpu.count":     func(w store.Worker) int { return w.GPUInfo.Count },
	"amount.worker.gpu.memory.mb": func(w store.Worker) int { return w.GPUInfo.VRAMMb },
}

// checkAmounts evaluates all amount requirements against the worker's hardware
// values, returning the first rejection reason encountered.
// Usage pool requirements (prefixed "amount.worker.usagepool.") are handled
// separately by [checkUsagePools] and skipped here.
func checkAmounts(
	amts []store.StepAmountRequirement,
	worker store.Worker,
	_ map[string]store.UsagePool,
	_ map[string]int,
) (matchRejection, bool) {
	for _, req := range amts {
		// Usage pool amounts are handled by checkUsagePools. Capability names are
		// case-insensitive (OpenJD jobtemplate-2023-09), so match the prefix and
		// look up the well-known name case-insensitively.
		if hasPrefixFold(req.Name, "amount.worker.usagepool.") {
			continue
		}

		fn, known := workerAmountValue[strings.ToLower(req.Name)]
		var val int
		if known {
			val = fn(worker)
		}
		// Unknown amount names are treated as zero — if the requirement has a
		// min > 0, the worker fails.

		if !satisfiesAmount(req, val) {
			return rejectAmountRequirement, false
		}
	}
	return rejectNone, true
}

// satisfiesAmount returns true when workerVal is within the [Min, Max] range
// declared by req. Nil bounds are treated as unbounded.
func satisfiesAmount(req store.StepAmountRequirement, workerVal int) bool {
	fval := float64(workerVal)

	if req.Min != nil {
		minVal, err := strconv.ParseFloat(*req.Min, 64)
		if err != nil || fval < minVal {
			return false
		}
	}
	if req.Max != nil {
		maxVal, err := strconv.ParseFloat(*req.Max, 64)
		if err != nil || fval > maxVal {
			return false
		}
	}
	return true
}

// ── Attribute requirements ────────────────────────────────────────────────────

// workerAttributeValue returns the value of the named attribute on worker.
// Handles well-known names and falls back to the worker's Tags map for custom
// attributes declared via the "attr.worker.tag.<key>" convention.
// Capability names are case-insensitive in full (OpenJD jobtemplate-2023-09), so
// the well-known names, the tag namespace prefix, AND the tag key are all matched
// case-insensitively.
func workerAttributeValue(worker store.Worker, name string) string {
	switch strings.ToLower(name) {
	case "attr.worker.os.family":
		return osFamily(worker.OS)
	case "attr.worker.os.version":
		return worker.OSVersion
	case "attr.worker.cpu.arch":
		return cpuArch(worker.Arch)
	case "attr.worker.computelocation":
		return worker.ComputeLocation
	default:
		if key, ok := cutPrefixFold(name, "attr.worker.tag."); ok {
			return tagValueFold(worker.Tags, key)
		}
		return ""
	}
}

// osFamily translates a worker's self-reported OS into the token OpenJD's
// reserved attr.worker.os.family attribute is spelled with.
//
// The worker reports runtime.GOOS (internal/worker/capabilities, probe.OS), and
// GOOS agrees with the specification on "linux" and "windows" but not on macOS,
// where GOOS says "darwin" and the specification says "macos". The template
// validator enforces the specification's set — internal/openjd's
// reservedAttributeAllowed accepts only {linux, windows, macos} — so "macos" is
// the ONLY spelling a template can legally carry for a Mac, and comparing it
// against a raw "darwin" made every such requirement unsatisfiable: the job
// validated, was accepted, and its tasks then waited for a worker that could
// not exist. Found 2026-08-15; nothing had caught it because the matcher's
// tests exercised only linux and windows.
//
// The mapping is deliberately ONE-WAY. This attribute carries the
// specification's family, not the Go runtime's, so "darwin" must not match
// either — accepting both would leave the same two spellings disagreeing, just
// in the other direction. A template that genuinely wants the GOOS spelling has
// attr.worker.tag.os, which capabilities.Detect populates verbatim.
//
// Normalizing HERE, and not at registration, is also deliberate: store.Worker.OS
// is surfaced raw in the REST API, the web UI and `sqi-worker capabilities`, and
// is copied into the "os" capability tag. Rewriting it at the source would
// change all four, and would leave rows written by earlier workers still saying
// "darwin". Matching is the only place the specification's vocabulary applies.
func osFamily(workerOS string) string {
	family := strings.ToLower(workerOS)
	if family == "darwin" {
		return "macos"
	}
	return family
}

// cpuArch translates a worker's self-reported CPU architecture into the token
// OpenJD's reserved attr.worker.cpu.arch attribute is spelled with.
//
// Exactly the shape of [osFamily], and it was broken worse. The worker reports
// runtime.GOARCH, which says "amd64" where the specification says "x86_64" and
// agrees on "arm64". But until this landed the switch above had NO case for
// attr.worker.cpu.arch at all: the name fell through to the tag lookup,
// resolved to "", and could never match on any platform — and the worker did
// not report its architecture in the first place, so there was nothing to
// compare even if it had. internal/openjd validates the attribute (its enum is
// {x86_64, arm64}), so a template gating on it was accepted and then waited for
// a worker that could not exist. Found 2026-08-15 while fixing the os.family
// version of the same mistake.
//
// An UNREPORTED architecture stays "" and matches nothing. That is the safe
// direction: the alternative — treating unknown as universally acceptable —
// would dispatch x86_64 work to an arm64 host. A worker upgraded past this
// change reports its architecture as soon as it restarts and re-registers.
//
// One-way, for [osFamily]'s reason: this attribute carries the specification's
// vocabulary, so Go's "amd64" must not match it either.
func cpuArch(workerArch string) string {
	arch := strings.ToLower(workerArch)
	if arch == "amd64" {
		return "x86_64"
	}
	return arch
}

// tagValueFold returns the value of the worker tag whose key matches key
// case-insensitively, or "" if none. An exact match is preferred; otherwise keys
// are compared with EqualFold.
func tagValueFold(tags map[string]string, key string) string {
	if v, ok := tags[key]; ok {
		return v
	}
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// hasPrefixFold reports whether s begins with prefix, compared case-insensitively.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// cutPrefixFold removes a case-insensitive prefix from s, returning the remainder
// (with its original case preserved) and whether the prefix was present.
func cutPrefixFold(s, prefix string) (string, bool) {
	if hasPrefixFold(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// checkAttributes evaluates all attribute requirements against the worker,
// returning the first rejection reason encountered.
func checkAttributes(attrs []store.StepAttributeRequirement, worker store.Worker) (matchRejection, bool) {
	for _, req := range attrs {
		val := workerAttributeValue(worker, req.Name)

		if len(req.AnyOf) > 0 {
			matched := false
			for _, accepted := range req.AnyOf {
				if strings.EqualFold(val, accepted) {
					matched = true
					break
				}
			}
			if !matched {
				return rejectAttributeRequirement, false
			}
		}

		for _, required := range req.AllOf {
			if !strings.EqualFold(val, required) {
				return rejectAttributeRequirement, false
			}
		}
	}
	return rejectNone, true
}

// ── Usage pool requirements ───────────────────────────────────────────────────

// checkUsagePools verifies that every named usage pool has at least one
// free slot (active claims < MaxConcurrent). A pool not found in pools is
// treated as having zero capacity, blocking assignment.
func checkUsagePools(
	poolNames []string,
	pools map[string]store.UsagePool,
	activeCounts map[string]int,
) (matchRejection, bool) {
	for _, name := range poolNames {
		// pools and activeCounts are keyed by lowercased pool name (see
		// buildUsageContext); pool names are case-insensitive per OpenJD.
		lk := strings.ToLower(name)
		pool, ok := pools[lk]
		if !ok {
			// Pool not configured — treat as at capacity to prevent over-subscription.
			return rejectUsagePool, false
		}
		if pool.MaxConcurrent <= 0 {
			// MaxConcurrent == 0 means unlimited; any positive active count is fine.
			continue
		}
		active := activeCounts[lk]
		if active >= pool.MaxConcurrent {
			return rejectUsagePool, false
		}
	}
	return rejectNone, true
}
