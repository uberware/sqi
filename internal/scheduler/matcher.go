// SPDX-License-Identifier: AGPL-3.0-only

package scheduler

// matcher.go implements worker-to-task eligibility checking (task 50).
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
//	amount.worker.licensepool.<n> → license pool named n must have capacity
//
// Note: gpu.memory.mb matches against the per-device VRAM of a worker's
// homogeneous GPU pool (see [store.GPUInfo]).  A worker with mixed GPU models
// should advertise the lowest per-device VRAM to avoid over-scheduling.
// Per-device heterogeneous matching is deferred to a future phase.
//
// Attribute requirements (categorical, compared as strings):
//
//	attr.worker.os.family         → strings.ToLower(worker.OS)
//	attr.worker.os.version        → worker.OSVersion
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
//  6. License pool availability: all required pools have remaining capacity

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
	rejectLicensePool                         // required license pool is at capacity
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
	case rejectLicensePool:
		return "license pool at capacity"
	default:
		return "eligible"
	}
}

// WorkerEligible reports whether worker w can run a task that belongs to step s
// and job j, given the current license pool state.
//
// pools maps pool name → [store.LicensePool].
// activeCounts maps pool name → current active checkout count.
//
// Both maps may be nil or empty when no license pools exist.
func WorkerEligible(
	worker store.Worker,
	job store.Job,
	step store.Step,
	pools map[string]store.LicensePool,
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
	pools map[string]store.LicensePool,
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
	pools map[string]store.LicensePool,
	activeCounts map[string]int,
) (matchRejection, bool) {
	// ── 1. Farm membership ────────────────────────────────────────────────────
	if worker.FarmID != job.FarmID {
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
		if r, ok := checkLicensePools(step.HostRequirements.LicensePools, pools, activeCounts); !ok {
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
// License pool requirements (prefixed "amount.worker.licensepool.") are handled
// separately by [checkLicensePools] and skipped here.
func checkAmounts(
	amts []store.StepAmountRequirement,
	worker store.Worker,
	_ map[string]store.LicensePool,
	_ map[string]int,
) (matchRejection, bool) {
	for _, req := range amts {
		// License pool amounts are handled by checkLicensePools.
		if strings.HasPrefix(req.Name, "amount.worker.licensepool.") {
			continue
		}

		fn, known := workerAmountValue[req.Name]
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
func workerAttributeValue(worker store.Worker, name string) string {
	switch name {
	case "attr.worker.os.family":
		return strings.ToLower(worker.OS)
	case "attr.worker.os.version":
		return worker.OSVersion
	case "attr.worker.computelocation":
		return worker.ComputeLocation
	default:
		if key, ok := strings.CutPrefix(name, "attr.worker.tag."); ok {
			return worker.Tags[key]
		}
		return ""
	}
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

// ── License pool requirements ─────────────────────────────────────────────────

// checkLicensePools verifies that every named license pool has at least one
// free slot (active checkouts < MaxConcurrent). A pool not found in pools is
// treated as having zero capacity, blocking assignment.
func checkLicensePools(
	poolNames []string,
	pools map[string]store.LicensePool,
	activeCounts map[string]int,
) (matchRejection, bool) {
	for _, name := range poolNames {
		pool, ok := pools[name]
		if !ok {
			// Pool not configured — treat as at capacity to prevent over-subscription.
			return rejectLicensePool, false
		}
		if pool.MaxConcurrent <= 0 {
			// MaxConcurrent == 0 means unlimited; any positive active count is fine.
			continue
		}
		active := activeCounts[name]
		if active >= pool.MaxConcurrent {
			return rejectLicensePool, false
		}
	}
	return rejectNone, true
}
