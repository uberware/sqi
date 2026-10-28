// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// extcaps.go keeps a job that declares a worker-enforced extension off a worker
// that does not enforce it.
//
// Some extensions change what a task's success means and are therefore decided
// on the worker (SQI_LOG_CHECKS fails a task on a log line or a missing success
// line; the server never sees the process's output as a stream it could judge).
// A worker built before the extension existed would run the task and report its
// exit status alone, so a render that printed its failure and exited 0 would
// succeed -- the very outcome the author declared the extension to prevent. The
// worker therefore advertises the worker-enforced extensions it supports in its
// registration ([protocol.RegisterMsg.Extensions]), the server persists them
// ([store.Worker.Extensions]), and the scheduler withholds a job declaring one
// from a worker that does not list it.
//
// Same shape as exprcaps.go, and both halves matter for the same reason: the
// lease gate is the bound, and the unschedulable sweep is what the operator
// reads -- a bound with no explanation is a silent stall.
//
// A job row that records no declared-extension list (written before migration
// 00027) is NOT gated: SQI_LOG_CHECKS did not exist then, so no such row can
// declare it, and unlike EXPR there is nothing to fall back to.

import (
	"slices"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

// workerExtensionBlock returns "" when worker may be given job, or the
// operator-facing reason it may not: job declares a worker-enforced extension
// that worker did not advertise.
//
// Both callers matter and neither may be dropped: [Scheduler.leaseGatesPass] is
// the bound and [Scheduler.evaluateSchedulability] is the explanation the sweep
// writes onto the task.
func workerExtensionBlock(worker store.Worker, job store.Job) string {
	for _, name := range openjd.WorkerEnforcedExtensions() {
		declared, recorded := job.DeclaresExtension(name)
		if !recorded || !declared {
			continue
		}
		if !slices.Contains(worker.Extensions, name) {
			return "worker does not enforce " + name + "; upgrade sqi-worker"
		}
	}
	return ""
}
