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

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// leaseRequest is the worker's work-lease request payload.
type leaseRequest struct {
	WorkerID string `json:"worker_id"`
	// InstanceID is the requesting worker process's instance ID, the one it
	// sends in its registration (H4a2 §4.5). Empty from a worker that sends
	// none. See [Scheduler.leaseFromUnregisteredInstance].
	InstanceID string `json:"instance_id,omitempty"`
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
		// Two cases, both refused the same way: a body this server cannot
		// decode at all, and one that decodes but carries no identity to
		// check against the subject. In neither is there anything to
		// authorize, so the subject ID is all that is left to log.
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
	// another worker while this connection receives the job code. No
	// req.WorkerID == "" guard here: that case already returned above.
	if req.WorkerID != workerID {
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
	// Both refusals are held, not answered at once: see refuseLeaseAfterDelay.
	// The order of the two checks only decides whether the instance-mismatch
	// Debug log fires: a disabled worker is refused without it.
	if workerDisabled(worker) || s.leaseFromUnregisteredInstance(ctx, worker, req.InstanceID) {
		return s.refuseLeaseAfterDelay(ctx)
	}

	batch, err := s.selectLeaseBatchLocked(ctx, worker)
	if err != nil {
		s.logLeaseSelectionFailure(ctx, workerID, len(batch), err)
	}
	// A store error part way through a batch still delivers what was leased
	// before it: those tasks are committed as assigned, and dropping them would
	// strand them with nobody running them until the assigned-task timeout.
	if len(batch) > 0 {
		return marshalLeaseReply(batch)
	}
	if err != nil {
		return marshalLeaseReply(nil)
	}

	// Park until work appears or the hold elapses, then try exactly once more.
	// The park happens OUTSIDE the per-worker lock; only the selection below is
	// serialized, so a re-woken request reads the up-to-date committed cores.
	if s.waiters.wait(ctx, queueID, s.leaseHoldTimeout) {
		return marshalLeaseReply(s.leaseAfterPark(ctx, workerID, req.InstanceID))
	}
	return marshalLeaseReply(nil)
}

// refuseLeaseAfterDelay answers a lease request that is refused outright, from a
// worker process whose registration has not landed or from a disabled worker: an
// empty batch, held for leaseRefusalDelay (or until ctx ends). The worker
// re-requests as soon as a reply arrives, so an immediate answer would spin its
// lease loop for as long as the registration takes, or, for a disabled worker
// (which can stay disabled for days), for as long as the operator leaves it
// disabled. It holds no lock and touches nothing; each request runs on its own
// goroutine and a worker keeps one request outstanding per queue, so a process
// holds at most one of these per queue.
func (s *Scheduler) refuseLeaseAfterDelay(ctx context.Context) []byte {
	t := time.NewTimer(s.leaseRefusalDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	return marshalLeaseReply(nil)
}

// workerDisabled reports whether an operator has disabled the worker. Disabled
// drains: the worker finishes the tasks it holds and is leased nothing new
// (docs/api.md). It is checked on the lease request, where the refusal is held
// for leaseRefusalDelay so the worker's loop cannot spin, and again after a
// park, where it is answered at once because the park already waited. A worker
// can be disabled while its request is parked. A disable that lands between the
// check and the lease can still let one batch through, which is the documented
// drain.
func workerDisabled(w store.Worker) bool {
	return w.Status == store.WorkerStatusDisabled
}

// leaseAfterPark is the one retry a parked lease request makes once woken: it
// re-reads the worker and, unless the worker was disabled, or re-registered from
// another process, while the request was parked, selects a batch. A failure to
// read the worker is an empty batch; a failure part way through selection
// returns the tasks already leased, which must reach the worker (see
// [Scheduler.selectLeaseBatch]).
func (s *Scheduler) leaseAfterPark(ctx context.Context, workerID, instanceID string) [][]byte {
	w, err := s.store.GetWorker(ctx, workerID)
	if err != nil || workerDisabled(w) || s.leaseFromUnregisteredInstance(ctx, w, instanceID) {
		return nil
	}
	batch, err := s.selectLeaseBatchLocked(ctx, w)
	if err != nil {
		s.logLeaseSelectionFailure(ctx, workerID, len(batch), err)
	}
	return batch
}

// logLeaseSelectionFailure records a store error during lease selection and how
// many tasks leased before it are still delivered.
func (s *Scheduler) logLeaseSelectionFailure(ctx context.Context, workerID string, delivered int, err error) {
	s.logger.WarnContext(
		ctx, "scheduler: lease selection failed",
		slog.String("worker_id", workerID),
		slog.Int("delivered", delivered),
		slog.Any("error", err),
	)
}

// leaseFromUnregisteredInstance reports whether a lease request comes from a
// worker process other than the one whose registration the store last applied:
// both instance IDs are known and they differ. Such a request gets an empty
// batch, without parking and without touching a task. The common case is a
// restarted worker that asks for work before the server has consumed its new
// registration (the register message goes through JetStream asynchronously,
// the lease is core-NATS request/reply); a task leased to it then would match
// the registration's restart reclaim, which goes by worker ID, and be handed
// back to ready while this process runs it. Its next request, after the
// registration lands, is served. The refusal at the front of the handler is
// held for leaseRefusalDelay (see [Scheduler.refuseLeaseAfterDelay]); the one
// after a park is not, since the park already waited. An empty ID on either
// side (a worker that sends none, or a row no ID-sending process has
// registered yet) proves nothing, and the request is served as before.
func (s *Scheduler) leaseFromUnregisteredInstance(ctx context.Context, w store.Worker, instanceID string) bool {
	if w.InstanceID == "" || instanceID == "" || w.InstanceID == instanceID {
		return false
	}
	// Debug: a restarted worker asks once per queue until its registration
	// lands, and an unauthenticated broker lets anything publish here.
	s.logger.DebugContext(
		ctx, "scheduler: lease request from a worker process whose registration has not landed — no work",
		slog.String("worker_id", w.ID),
		slog.String("registered_instance_id", w.InstanceID),
		slog.String("request_instance_id", instanceID),
	)
	return true
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
//
// On a store error it returns the assignments already leased together with the
// error: those tasks are committed as assigned, so they must reach the worker.
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
			return batch, err
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
// nil) when skipped, including when the lease transaction refuses it because a
// parallel lease changed the picture after the gates passed; a non-nil error only
// on an unexpected store failure.
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

	// The gates above are cheap early filters over values read a moment ago.
	// This call is the decision: one transaction leases the task, re-checks the
	// queue, farm and pool caps against their current values, and writes the
	// attempt and its claims (invariants I3 and I5). Every outcome other than
	// Leased writes nothing, so there is nothing to revert and the task simply
	// stays ready (or is no longer ours) for the next lease request.
	res, err := s.store.LeaseTask(ctx, store.LeaseRequest{
		TaskID:    task.ID,
		WorkerID:  worker.ID,
		AttemptID: uuid.NewString(),
		Now:       time.Now().UTC(),
		Claims:    buildUsageClaims(gd.step, gd.pools),
	})
	if err != nil {
		return nil, 0, false, fmt.Errorf("lease: lease task %s: %w", task.ID, err)
	}
	if res.Outcome != store.LeaseLeased {
		s.logLeaseRefused(ctx, task.ID, res)
		return nil, 0, false, nil // skip, do not propagate: a lost race is not an error
	}
	attempt := res.Attempt
	// The scheduler already knows both fields the log-ingest path needs, so
	// populate the cache now rather than waiting for the first log chunk to
	// pay for a store read.
	s.attemptCache.put(attempt.ID, attempt.WorkerID, attempt.TaskID)

	payload, err = buildAssignPayload(ctx, task, worker, gd.job, gd.step, gd.queue, attempt.ID, s.store)
	if err != nil {
		return nil, 0, false, fmt.Errorf("lease: build payload for %s: %w", task.ID, err)
	}
	return payload, cost, true, nil
}

// logLeaseRefused records why [store.TaskStore.LeaseTask] did not lease a task.
// Every refusal is a skip, never an error: another lease took the task or it
// stopped being leasable (LeaseLost), or a cap filled between the gates and the
// lease (LeaseQueueFull, LeaseFarmFull, LeasePoolFull). Only a full usage pool is
// logged, because the policy gate and the lost-race skip have always been silent
// and a pool name is the one thing an operator can act on.
func (s *Scheduler) logLeaseRefused(ctx context.Context, taskID string, res store.LeaseResult) {
	if res.Outcome != store.LeasePoolFull {
		return
	}
	s.logger.DebugContext(
		ctx, "scheduler: usage pool at capacity — deferring assignment",
		slog.String("task_id", taskID),
		slog.String("pool", res.FullPool),
	)
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
