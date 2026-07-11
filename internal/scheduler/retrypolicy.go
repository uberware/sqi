// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"time"

	"github.com/uberware/sqi/internal/store"
)

// RetryPolicy is the resolved, effective retry configuration for a task.
type RetryPolicy struct {
	MaxAttempts  int           // total genuine tries before terminal-fail (>= 1)
	RetryDelay   time.Duration // backoff before re-queue (>= 0)
	FailureLimit int           // job-level ceiling; 0 = off
}

// coalesceInt returns the first non-nil pointer's value, else def.
func coalesceInt(def int, vals ...*int) int {
	for _, v := range vals {
		if v != nil {
			return *v
		}
	}
	return def
}

// resolveRetryPolicy computes the effective policy as the first non-nil of
// Job -> Queue -> Farm for each knob, falling back to the server default def.
func resolveRetryPolicy(job store.Job, queue store.Queue, farm store.Farm, def RetryPolicy) RetryPolicy {
	//nolint:gocritic,predeclared // max is a meaningful local variable name here
	max := coalesceInt(def.MaxAttempts, job.MaxAttempts, queue.MaxAttempts, farm.MaxAttempts)
	if max < 1 {
		max = 1
	}
	delaySec := coalesceInt(int(def.RetryDelay.Seconds()), job.RetryDelaySeconds, queue.RetryDelaySeconds, farm.RetryDelaySeconds)
	if delaySec < 0 {
		delaySec = 0
	}
	limit := coalesceInt(def.FailureLimit, job.FailureLimit, queue.FailureLimit, farm.FailureLimit)
	if limit < 0 {
		limit = 0
	}
	return RetryPolicy{
		MaxAttempts:  max,
		RetryDelay:   time.Duration(delaySec) * time.Second,
		FailureLimit: limit,
	}
}

// retryDefaults returns the server-level fallback retry policy from config.
func (s *Scheduler) retryDefaults() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:  s.cfg.DefaultMaxAttempts,
		RetryDelay:   s.cfg.RetryDelay,
		FailureLimit: s.cfg.DefaultFailureLimit,
	}
}
