// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// lease.go implements worker-requested task leasing: a worker asks for work,
// the scheduler selects a priority-ordered batch of ready tasks the worker is
// eligible for that fits its free CPU cores, atomically leases them, and returns
// the assignment payloads. Replaces the eager dispatch loop.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// leaseRequest is the worker's work-lease request payload.
type leaseRequest struct {
	WorkerID string `json:"worker_id"`
}

// leaseReply is the server's batch response. Assignments holds marshaled
// protocol.AssignMsg payloads (json.RawMessage) so the worker can decode each.
type leaseReply struct {
	Assignments []json.RawMessage `json:"assignments"`
}

// handleLeaseRequest decodes a lease request, leases a fitting batch, and on an
// empty result parks the request in the waiter registry until new work appears
// or leaseHoldTimeout elapses, then replies once more.
//
// workerID is the identity carried by the request's subject; queueID is the
// queue the worker asked about.
func (s *Scheduler) handleLeaseRequest(workerID, queueID string, data []byte) []byte {
	ctx := s.ctx
	var req leaseRequest
	if err := json.Unmarshal(data, &req); err != nil || req.WorkerID == "" {
		// The subject is the only identity left once the body will not decode.
		// Debug, not warn: an unauthenticated broker lets anything publish here,
		// so a warn would be a log-flood vector.
		s.logger.DebugContext(
			ctx, "scheduler: malformed lease request",
			slog.String("subject_worker_id", workerID),
		)
		return marshalLeaseReply(nil)
	}

	// The subject is authoritative. A payload that names a different worker
	// is either a stale client or an attempt to have tasks assigned to
	// another worker while this connection receives the job code.
	if req.WorkerID != "" && req.WorkerID != workerID {
		s.logger.WarnContext(
			ctx, "scheduler: lease request whose payload identity differs from its subject — refusing",
			slog.String("subject_worker_id", workerID),
			slog.String("payload_worker_id", req.WorkerID),
		)
		return marshalLeaseReply(nil)
	}

	worker, err := s.store.GetWorker(ctx, workerID)
	if err != nil {
		return marshalLeaseReply(nil)
	}

	batch, err := s.selectLeaseBatchLocked(ctx, worker)
	if err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: lease selection failed",
			slog.String("worker_id", workerID),
			slog.Any("error", err),
		)
		return marshalLeaseReply(nil)
	}
	if len(batch) > 0 {
		return marshalLeaseReply(batch)
	}

	// Park until work appears or the hold elapses, then try exactly once more.
	// The park happens OUTSIDE the per-worker lock; only the selection below is
	// serialized, so a re-woken request reads the up-to-date committed cores.
	if s.waiters.wait(ctx, queueID, s.leaseHoldTimeout) {
		if w2, err2 := s.store.GetWorker(ctx, workerID); err2 == nil {
			if batch2, err2 := s.selectLeaseBatchLocked(ctx, w2); err2 == nil {
				return marshalLeaseReply(batch2)
			}
		}
	}
	return marshalLeaseReply(nil)
}

// selectLeaseBatchLocked runs selectLeaseBatch while holding the per-worker
// lease lock so concurrent requests for the same worker select one-at-a-time
// (the loser then reads the committed cores the winner already claimed).
// Requests for different workers proceed in parallel.
func (s *Scheduler) selectLeaseBatchLocked(ctx context.Context, worker store.Worker) ([][]byte, error) {
	mu := s.workerLeaseLock(worker.ID)
	mu.Lock()
	defer mu.Unlock()
	return s.selectLeaseBatch(ctx, worker)
}

// workerLeaseLock returns the per-worker mutex for lease selection, creating it
// on first use.
func (s *Scheduler) workerLeaseLock(workerID string) *sync.Mutex {
	mu, _ := s.leaseLocks.LoadOrStore(workerID, &sync.Mutex{})
	return mu.(*sync.Mutex) //nolint:errcheck,forcetypeassert // value type is always *sync.Mutex
}

func marshalLeaseReply(batch [][]byte) []byte {
	reply := leaseReply{Assignments: make([]json.RawMessage, 0, len(batch))}
	for _, b := range batch {
		reply.Assignments = append(reply.Assignments, json.RawMessage(b))
	}
	out, _ := json.Marshal(reply) //nolint:errcheck // leaseReply always marshals
	return out
}

// leaseGateData holds the records fetched by leaseGatesPass for use by the
// caller after all gates have passed.
type leaseGateData struct {
	job          store.Job
	step         store.Step
	queue        store.Queue
	pools        map[string]store.UsagePool
	activeCounts map[string]int
}

// selectLeaseBatch leases as many ready tasks to worker as fit its free cores,
// in the store's priority order (first-fit, skip-and-continue). Each leased task
// is transitioned ready->assigned, given an open attempt, and has its usage-pool
// claims held; the returned slice holds the marshaled AssignMsg payloads.
func (s *Scheduler) selectLeaseBatch(ctx context.Context, worker store.Worker) ([][]byte, error) {
	full := worker.CPUCount
	if full <= 0 {
		full = 1 // a worker advertising no cores can still run one undeclared task
	}
	committed, err := s.store.CommittedCores(ctx, worker.ID, full)
	if err != nil {
		return nil, fmt.Errorf("lease: committed cores for %s: %w", worker.ID, err)
	}
	free := full - committed
	if free <= 0 {
		return nil, nil
	}

	candidates, err := s.store.ListReadyTasks(ctx, s.cfg.FarmID, time.Now().UTC(), s.cfg.AssignBatchSize)
	if err != nil {
		return nil, fmt.Errorf("lease: list ready tasks: %w", err)
	}

	// Hoisted out of the candidate loop: this worker's EXPR shortfall depends
	// only on the worker and this server's configuration, so it is constant for
	// the whole batch, and on a misconfigured farm computing it per candidate
	// would rebuild the same four-sentence reason 50 times.
	exprShortfall := s.workerExprShortfall(worker)

	var batch [][]byte
	for _, task := range candidates {
		if free <= 0 {
			break
		}
		payload, cost, ok, err := s.tryLeaseTask(ctx, task, worker, free, exprShortfall)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // ineligible, didn't fit, lost the race, or policy/usage blocked
		}
		batch = append(batch, payload)
		free -= cost
	}
	return batch, nil
}

// leaseGatesPass performs capability, policy, and usage-pool gate checks for
// a candidate task/worker pair. Returns (data, true, nil) when all gates pass;
// (data, false, nil) when any gate blocks (skip silently); (data, false, err)
// on unexpected store failure.
func (s *Scheduler) leaseGatesPass(
	ctx context.Context,
	task store.Task,
	worker store.Worker,
	exprShortfall string,
) (leaseGateData, bool, error) {
	var d leaseGateData

	job, err := s.store.GetJob(ctx, task.JobID)
	if err != nil {
		return d, false, fmt.Errorf("lease: get job %s: %w", task.JobID, err)
	}
	d.job = job

	if job.Status == store.JobStatusPaused || job.Status.IsTerminal() {
		return d, false, nil // paused/terminal job: skip (defends the ready-list→lease window)
	}

	// Cross-binary EXPR limits: never hand an EXPR job to a worker whose
	// advertised caps are tighter than the ones this template was accepted
	// under. Skipping leaves the task ready for a capable worker; if none
	// exists, the unschedulable sweep writes the same reason onto the task --
	// but only while that sweep is enabled (Config.UnschedulableGrace > 0, the
	// default; <= 0 is a legitimate "off" setting, and with it off such a task
	// waits with nothing written on it). See exprcaps.go for why this is a skip
	// and not a submit-time rejection.
	if exprCapsBlock(exprShortfall, job) != "" {
		return d, false, nil
	}

	step, err := s.store.GetStep(ctx, task.StepID)
	if err != nil {
		return d, false, fmt.Errorf("lease: get step %s: %w", task.StepID, err)
	}
	d.step = step

	queue, err := s.store.GetQueue(ctx, job.QueueID)
	if err != nil {
		return d, false, fmt.Errorf("lease: get queue %s: %w", job.QueueID, err)
	}
	d.queue = queue

	farm, err := s.store.GetFarm(ctx, job.FarmID)
	if err != nil {
		return d, false, fmt.Errorf("lease: get farm %s: %w", job.FarmID, err)
	}

	policyErr := policyGate(ctx, s.store, job, queue, farm)
	if policyErr != nil {
		if errors.Is(policyErr, errPolicyBlocked) {
			return d, false, nil // errPolicyBlocked is a skip signal, not a caller error
		}
		return d, false, policyErr // genuine store error → propagate
	}

	pools, activeCounts, err := s.buildUsageContext(ctx, step)
	if err != nil {
		return d, false, fmt.Errorf("lease: usage context: %w", err)
	}
	d.pools = pools
	d.activeCounts = activeCounts

	if !WorkerEligible(worker, job, step, pools, activeCounts) {
		return d, false, nil
	}
	return d, true, nil
}

// tryLeaseTask attempts to lease one task to worker if it is eligible and fits
// free cores. Returns (payload, coreCost, true, nil) on success; (nil, 0, false,
// nil) when skipped; a non-nil error only on an unexpected store failure.
func (s *Scheduler) tryLeaseTask(
	ctx context.Context,
	task store.Task,
	worker store.Worker,
	free int,
	exprShortfall string,
) (payload []byte, cost int, ok bool, err error) {
	// Log once when a task's declared core requirement exceeds the worker's total
	// capacity — it can never run here regardless of current load. Best-effort
	// observability only; does not change scheduling behavior.
	if task.RequiredCores != nil && *task.RequiredCores > worker.CPUCount {
		s.logger.InfoContext(ctx, "scheduler: task may be unschedulable on this worker",
			slog.String("task_id", task.ID),
			slog.Int("required_cores", *task.RequiredCores),
			slog.Int("worker_cores", worker.CPUCount))
	}
	cost = fullMachineCost(task, worker)
	if cost > free {
		return nil, 0, false, nil
	}

	gd, pass, err := s.leaseGatesPass(ctx, task, worker, exprShortfall)
	if err != nil {
		return nil, 0, false, err
	}
	if !pass {
		return nil, 0, false, nil
	}

	// Win the race for this still-ready task.
	now := time.Now().UTC()
	leased, err := s.store.LeaseReadyTask(ctx, task.ID, worker.ID, now)
	if err != nil {
		return nil, 0, false, fmt.Errorf("lease: lease task %s: %w", task.ID, err)
	}
	if !leased {
		return nil, 0, false, nil // another worker got it
	}

	// Attempt + usage claim (reverts task to ready on failure internally).
	attempt, claimErr := s.createAttemptAndClaimUsage(ctx, task, worker, gd.step, gd.pools, now)
	if claimErr != nil {
		if !errors.Is(claimErr, errNoWorkerAvailable) {
			s.logger.WarnContext(
				ctx, "lease: createAttemptAndClaimUsage failed — task reverted to ready",
				slog.String("task_id", task.ID),
				slog.Any("error", claimErr),
			)
		}
		return nil, 0, false, nil // task already reverted internally; skip, do not propagate
	}

	payload, err = buildAssignPayload(ctx, task, worker, gd.job, gd.step, gd.queue, attempt.ID, s.store)
	if err != nil {
		return nil, 0, false, fmt.Errorf("lease: build payload for %s: %w", task.ID, err)
	}
	return payload, cost, true, nil
}

// fullMachineCost returns a task's effective CPU cost for worker: its declared
// required_cores, or the worker's full CPUCount when undeclared.
func fullMachineCost(task store.Task, worker store.Worker) int {
	if task.RequiredCores != nil {
		if *task.RequiredCores < 1 {
			return 1
		}
		return *task.RequiredCores
	}
	if worker.CPUCount > 0 {
		return worker.CPUCount
	}
	return 1
}
