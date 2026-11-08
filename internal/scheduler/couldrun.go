// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// couldrun.go answers a capability question the lease path does not: could a
// given machine ever run a step, whatever it is doing now. The lease path asks
// the load-dependent version (a worker that is online, enabled, within its
// policy caps and has free cores) and is not changed by anything here.

import (
	"github.com/uberware/sqi/internal/store"
)

// couldRun reports whether w could ever run a task of step and job needing
// requiredCores, ignoring everything that varies with load: whether w is
// disabled or online, the queue, farm and pool caps, and how many cores are
// free. It is the lease path's static checks and nothing else.
//
// A usage pool is checked for existence only: a pool at its cap is still
// runnable, a pool that does not exist is not (matching the lease path, which
// treats a missing pool as at capacity). requiredCores is checked against
// w.CPUCount only when both are positive.
func (s *Scheduler) couldRun(
	w store.Worker,
	job store.Job,
	step store.Step,
	requiredCores int,
	pools map[string]store.UsagePool,
) bool {
	// Nil active counts read as zero for every pool, so no pool is ever full.
	if _, ok := workerEligible(w, job, step, pools, nil); !ok {
		return false
	}
	if workerCompatBlock(s.workerExprShortfall(w), w, job) != "" {
		return false
	}
	return requiredCores <= 0 || w.CPUCount <= 0 || requiredCores <= w.CPUCount
}
