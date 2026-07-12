// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateTask inserts a new task.
func (s *Store) CreateTask(_ context.Context, task store.Task) (store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task.Parameters = copyMap(task.Parameters)
	s.tasks[task.ID] = task
	return task, nil
}

// GetTask returns the task with the given ID, or [store.ErrNotFound].
func (s *Store) GetTask(_ context.Context, id string) (store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return store.Task{}, store.ErrNotFound
	}

	task.Parameters = copyMap(task.Parameters)
	return task, nil
}

// ListTasks returns a paginated, filtered, and sorted page of tasks matching opts.
func (s *Store) ListTasks(_ context.Context, opts store.ListTasksOptions) (store.Page[store.Task], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := opts.Pagination.Validate(); err != nil {
		return store.Page[store.Task]{}, err
	}

	tasks := make([]store.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if filterTask(t, opts) {
			t.Parameters = copyMap(t.Parameters)
			tasks = append(tasks, t)
		}
	}

	slices.SortStableFunc(tasks, func(a, b store.Task) int {
		return cmpTask(a, b, opts.SortBy, opts.SortDir)
	})

	return applyPage(tasks, opts.Pagination), nil
}

// UpdateTaskStatus transitions a task to a new status and updates UpdatedAt.
// unschedulable_reason is only meaningful while a task is ready (set by the
// scheduler sweep), so it is cleared here regardless of the destination
// status — harmless when it was already empty.
func (s *Store) UpdateTaskStatus(_ context.Context, id string, status store.TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}

	task.Status = status
	task.UnschedulableReason = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	return nil
}

// SetTaskUnschedulableReason implements [store.TaskStore].
func (s *Store) SetTaskUnschedulableReason(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	task.UnschedulableReason = reason
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	return nil
}

// SetTaskFailureReason implements [store.TaskStore].
func (s *Store) SetTaskFailureReason(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	task.FailureReason = reason
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	return nil
}

// SetTaskFailureReasonIfEmpty implements [store.TaskStore]. It writes the reason
// only when the task currently has none; an unknown task or one that already
// carries a reason is a legitimate no-op, not an error.
func (s *Store) SetTaskFailureReasonIfEmpty(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok || task.FailureReason != "" {
		return nil
	}
	task.FailureReason = reason
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	return nil
}

// TransitionStepPendingTasks transitions every pending task of the step to `to`,
// updates UpdatedAt, stamps a non-empty failureReason on tasks that carry none,
// and returns the affected tasks.
func (s *Store) TransitionStepPendingTasks(_ context.Context, stepID string, to store.TaskStatus, failureReason string) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var affected []store.Task
	for id, t := range s.tasks {
		if t.StepID != stepID || t.Status != store.TaskStatusPending {
			continue
		}
		t.Status = to
		t.UnschedulableReason = ""
		if failureReason != "" && t.FailureReason == "" {
			t.FailureReason = failureReason
		}
		t.UpdatedAt = time.Now()
		s.tasks[id] = t

		out := t
		out.Parameters = copyMap(t.Parameters)
		affected = append(affected, out)
	}
	return affected, nil
}

// AssignTask atomically sets AssignedWorkerID, AssignedAt, and Status to
// [store.TaskStatusAssigned] for the given task.
func (s *Store) AssignTask(_ context.Context, id, workerID string, assignedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}

	task.AssignedWorkerID = workerID
	task.AssignedAt = &assignedAt
	task.Status = store.TaskStatusAssigned
	task.UnschedulableReason = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	return nil
}

// ReclaimWorkerTasks resets assigned/running tasks for the given worker back
// to [store.TaskStatusReady] and returns the count of tasks reclaimed.
func (s *Store) ReclaimWorkerTasks(_ context.Context, workerID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	var count int
	for id, task := range s.tasks {
		if task.AssignedWorkerID != workerID {
			continue
		}
		if task.Status != store.TaskStatusAssigned && task.Status != store.TaskStatusRunning {
			continue
		}
		task.Status = store.TaskStatusReady
		task.AssignedWorkerID = ""
		task.AssignedAt = nil
		task.UnschedulableReason = ""
		task.UpdatedAt = now
		s.tasks[id] = task
		count++
	}
	return count, nil
}

// ReclaimStaleAssignedTasks resets tasks stuck in [store.TaskStatusAssigned]
// with an AssignedAt older than cutoff back to [store.TaskStatusReady] and
// returns the reclaimed tasks (carrying their pre-reset assigned_worker_id).
func (s *Store) ReclaimStaleAssignedTasks(_ context.Context, cutoff time.Time) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	var reclaimed []store.Task
	for id, task := range s.tasks {
		if task.Status != store.TaskStatusAssigned {
			continue
		}
		if task.AssignedAt == nil || !task.AssignedAt.Before(cutoff) {
			continue
		}
		reclaimed = append(reclaimed, task) // snapshot with assigned_worker_id intact
		task.Status = store.TaskStatusReady
		task.AssignedWorkerID = ""
		task.AssignedAt = nil
		task.UnschedulableReason = ""
		task.UpdatedAt = now
		s.tasks[id] = task
	}
	return reclaimed, nil
}

// ListReadyTasks returns up to limit tasks in [store.TaskStatusReady] that
// belong to non-paused queues within the given farm, excluding tasks still
// backing off (RetryAfter after now) and tasks whose job is paused or
// terminal, ordered by job priority descending then CreatedAt ascending.
func (s *Store) ListReadyTasks(_ context.Context, farmID string, now time.Time, limit int) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build lookups: queue paused state and job priority.
	// farmID = "" means "all farms".
	queuePaused := make(map[string]bool)
	for _, q := range s.queues {
		if farmID == "" || q.FarmID == farmID {
			queuePaused[q.ID] = q.Paused
		}
	}

	jobPriority := make(map[string]int)
	for _, j := range s.jobs {
		if farmID == "" || j.FarmID == farmID {
			jobPriority[j.ID] = j.Priority
		}
	}

	var readyTasks []store.Task
	for _, t := range s.tasks {
		if isReadyInFarm(t, farmID, now, s.jobs, queuePaused) {
			t.Parameters = copyMap(t.Parameters)
			readyTasks = append(readyTasks, t)
		}
	}

	slices.SortStableFunc(readyTasks, func(a, b store.Task) int {
		return cmpReadyTask(a, b, jobPriority)
	})

	if len(readyTasks) > limit {
		readyTasks = readyTasks[:limit]
	}

	return readyTasks, nil
}

// filterTask reports whether t matches all non-zero filter fields in opts.
func filterTask(t store.Task, opts store.ListTasksOptions) bool {
	if opts.JobID != "" && t.JobID != opts.JobID {
		return false
	}
	if opts.StepID != "" && t.StepID != opts.StepID {
		return false
	}
	if len(opts.Statuses) > 0 {
		matched := slices.Contains(opts.Statuses, t.Status)
		if !matched {
			return false
		}
	} else if opts.Status != "" && t.Status != opts.Status {
		return false
	}
	if opts.WorkerID != "" && t.AssignedWorkerID != opts.WorkerID {
		return false
	}
	return true
}

// cmpTask returns a comparison value for two tasks by the given sort field and
// direction. An empty field defaults to [store.TaskSortByCreatedAt]; an empty
// direction defaults to ascending.
func cmpTask(a, b store.Task, field store.TaskSortField, dir store.SortDir) int {
	var n int
	switch field {
	case store.TaskSortByStatus:
		n = cmp.Compare(string(a.Status), string(b.Status))
	case store.TaskSortByUpdatedAt:
		n = a.UpdatedAt.Compare(b.UpdatedAt)
	case store.TaskSortByName:
		n = cmp.Compare(a.Name, b.Name)
	default: // TaskSortByCreatedAt
		n = a.CreatedAt.Compare(b.CreatedAt)
	}
	if dir == store.SortDesc {
		return -n
	}
	return n
}

// CountActiveTasksInQueue returns the number of tasks in 'assigned' or
// 'running' state whose job belongs to queueID.
func (s *Store) CountActiveTasksInQueue(_ context.Context, queueID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, t := range s.tasks {
		if t.Status != store.TaskStatusAssigned && t.Status != store.TaskStatusRunning {
			continue
		}
		job, ok := s.jobs[t.JobID]
		if ok && job.QueueID == queueID {
			n++
		}
	}
	return n, nil
}

// CountActiveTasksInFarm returns the number of tasks in 'assigned' or
// 'running' state whose job belongs to farmID.
func (s *Store) CountActiveTasksInFarm(_ context.Context, farmID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, t := range s.tasks {
		if t.Status != store.TaskStatusAssigned && t.Status != store.TaskStatusRunning {
			continue
		}
		job, ok := s.jobs[t.JobID]
		if ok && job.FarmID == farmID {
			n++
		}
	}
	return n, nil
}

// CancelJobTasks transitions all non-terminal tasks for the given job to
// [store.TaskStatusCanceled], stamping a non-empty reason on tasks that carry
// no failure reason yet, and returns those that were in
// [store.TaskStatusAssigned] or [store.TaskStatusRunning] at call time.
func (s *Store) CancelJobTasks(_ context.Context, jobID string, now time.Time, reason string) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var active []store.Task
	for id, task := range s.tasks {
		if task.JobID != jobID {
			continue
		}
		switch task.Status {
		case store.TaskStatusSucceeded, store.TaskStatusFailed, store.TaskStatusCanceled:
			// Already terminal; leave unchanged.
			continue
		}
		// Capture assigned/running tasks before clearing the worker reference.
		if task.Status == store.TaskStatusAssigned || task.Status == store.TaskStatusRunning {
			active = append(active, task)
		}
		task.Status = store.TaskStatusCanceled
		task.AssignedWorkerID = ""
		task.AssignedAt = nil
		task.UnschedulableReason = ""
		if reason != "" && task.FailureReason == "" {
			task.FailureReason = reason
		}
		task.UpdatedAt = now
		s.tasks[id] = task
	}
	return active, nil
}

// RetryTasks reverts failed/canceled tasks (and their terminal steps and the
// terminal job) to pending. See [store.TaskStore.RetryTasks].
func (s *Store) RetryTasks(_ context.Context, jobID string, taskIDs []string, now time.Time) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var only map[string]struct{}
	if taskIDs != nil {
		only = make(map[string]struct{}, len(taskIDs))
		for _, id := range taskIDs {
			only[id] = struct{}{}
		}
	}

	var revived []store.Task
	affectedSteps := make(map[string]struct{})
	for id, t := range s.tasks {
		if t.JobID != jobID {
			continue
		}
		if t.Status != store.TaskStatusFailed && t.Status != store.TaskStatusCanceled {
			continue
		}
		if only != nil {
			if _, ok := only[id]; !ok {
				continue
			}
		}
		t.Status = store.TaskStatusPending
		t.UnschedulableReason = ""
		t.FailedAttempts = 0
		t.RetryAfter = nil
		t.FailureReason = ""
		t.UpdatedAt = now
		s.tasks[id] = t
		affectedSteps[t.StepID] = struct{}{}

		out := t
		out.Parameters = copyMap(t.Parameters)
		revived = append(revived, out)
	}

	if len(revived) == 0 {
		return nil, nil
	}

	// Reset any terminal steps that owned a revived task.
	for stepID := range affectedSteps {
		st, ok := s.steps[stepID]
		if !ok {
			continue
		}
		switch st.Status {
		case store.StepStatusFailed, store.StepStatusCanceled:
			st.Status = store.StepStatusPending
			st.UpdatedAt = now
			s.steps[stepID] = st
		}
	}

	s.retryResetJobLocked(jobID, now)

	return revived, nil
}

// retryResetJobLocked resets the job to pending when it is currently terminal
// or auto-parked (paused with a park reason), clearing its failure counter and
// park reason. A manually paused job (empty park reason) stays paused —
// retrying its tasks must not override the operator's pause. Caller must hold
// s.mu.
func (s *Store) retryResetJobLocked(jobID string, now time.Time) {
	job, ok := s.jobs[jobID]
	if !ok {
		return
	}
	terminal := job.Status == store.JobStatusFailed || job.Status == store.JobStatusCanceled
	autoParked := job.Status == store.JobStatusPaused && job.ParkReason != ""
	if !terminal && !autoParked {
		return
	}
	job.Status = store.JobStatusPending
	job.CompletedAt = nil
	job.FailedAttempts = 0
	job.ParkReason = ""
	job.UpdatedAt = now
	s.jobs[jobID] = job
}

// CountReadyTasksByQueue returns the number of leasable ready tasks for each
// queue in the given farm ("" = all farms), keyed by queue ID — the same
// eligibility predicate as ListReadyTasks (see isReadyInFarm).
func (s *Store) CountReadyTasksByQueue(_ context.Context, farmID string, now time.Time) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queuePaused := make(map[string]bool)
	for _, q := range s.queues {
		if farmID == "" || q.FarmID == farmID {
			queuePaused[q.ID] = q.Paused
		}
	}

	counts := make(map[string]int)
	for _, t := range s.tasks {
		if isReadyInFarm(t, farmID, now, s.jobs, queuePaused) {
			counts[s.jobs[t.JobID].QueueID]++
		}
	}
	return counts, nil
}

// isReadyInFarm reports whether t is eligible for assignment: status Ready,
// job belongs to farmID (or farmID is "" meaning all farms), the job's queue
// is not paused, the job itself is not paused or terminal, and t is not
// still backing off (RetryAfter, if set, must not be after now).
func isReadyInFarm(t store.Task, farmID string, now time.Time, jobs map[string]store.Job, queuePaused map[string]bool) bool {
	if t.Status != store.TaskStatusReady {
		return false
	}
	job, ok := jobs[t.JobID]
	if !ok {
		return false
	}
	if farmID != "" && job.FarmID != farmID {
		return false
	}
	if queuePaused[job.QueueID] {
		return false
	}
	if job.Status == store.JobStatusPaused || job.Status.IsTerminal() {
		return false
	}
	if t.RetryAfter != nil && t.RetryAfter.After(now) {
		return false
	}
	return true
}

// CountTasksByJob returns the number of tasks for the given job keyed by status.
// Statuses with zero tasks are omitted from the returned map.
func (s *Store) CountTasksByJob(_ context.Context, jobID string) (map[store.TaskStatus]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	counts := make(map[store.TaskStatus]int)
	for _, t := range s.tasks {
		if t.JobID == jobID {
			counts[t.Status]++
		}
	}
	return counts, nil
}

// CountUnschedulableTasksByJob returns the number of ready tasks for the
// given job that carry a non-empty unschedulable reason.
func (s *Store) CountUnschedulableTasksByJob(_ context.Context, jobID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, t := range s.tasks {
		if t.JobID == jobID && t.Status == store.TaskStatusReady && t.UnschedulableReason != "" {
			n++
		}
	}
	return n, nil
}

// FailureReasonSummary implements [store.TaskStore]. It mirrors the sqlite
// group-by-then-order-by(n DESC, reason ASC) semantics: the dominant reason
// is the most frequent one, ties broken by reason string ascending.
func (s *Store) FailureReasonSummary(_ context.Context, jobID string) (store.FailureSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	counts := make(map[string]int)
	for _, t := range s.tasks {
		if t.JobID == jobID && t.Status == store.TaskStatusFailed && t.FailureReason != "" {
			counts[t.FailureReason]++
		}
	}
	if len(counts) == 0 {
		return store.FailureSummary{}, nil
	}

	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	slices.SortFunc(reasons, func(a, b string) int {
		if n := counts[b] - counts[a]; n != 0 {
			return n // descending by count
		}
		return cmp.Compare(a, b) // ascending by reason
	})

	var sum store.FailureSummary
	sum.DominantReason = reasons[0]
	sum.DistinctReasons = len(reasons)
	for _, reason := range reasons {
		sum.FailedCount += counts[reason]
	}
	return sum, nil
}

// CommittedCores implements [store.TaskStore].
func (s *Store) CommittedCores(_ context.Context, workerID string, fullMachineCost int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	for _, t := range s.tasks {
		if t.AssignedWorkerID != workerID {
			continue
		}
		if t.Status != store.TaskStatusAssigned && t.Status != store.TaskStatusRunning {
			continue
		}
		if t.RequiredCores != nil {
			total += *t.RequiredCores
		} else {
			total += fullMachineCost
		}
	}
	return total, nil
}

// LeaseReadyTask implements [store.TaskStore].
func (s *Store) LeaseReadyTask(_ context.Context, taskID, workerID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[taskID]
	if !ok {
		return false, store.ErrNotFound
	}
	if t.Status != store.TaskStatusReady {
		return false, nil
	}
	t.Status = store.TaskStatusAssigned
	t.AssignedWorkerID = workerID
	at := now
	t.AssignedAt = &at
	t.UnschedulableReason = ""
	t.UpdatedAt = now
	s.tasks[taskID] = t
	return true, nil
}

// RecordTaskFailure implements [store.TaskStore]. Under the store lock it
// closes the attempt as failed and increments the task's and job's
// FailedAttempts counters ONLY when the attempt is still running (the first
// delivery of the failure). A redelivery — whose attempt is already terminal,
// or whose attempt is unknown — does not re-count; it returns the current
// counters so the caller's retry/park decision is stable across redeliveries.
func (s *Store) RecordTaskFailure(
	_ context.Context,
	attemptID, taskID string,
	exitCode *int,
	sessionID, message string,
	now time.Time,
) (taskFailed, jobFailed int, firstClose bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[taskID]
	if !ok {
		return 0, 0, false, store.ErrNotFound
	}
	j, ok := s.jobs[t.JobID]
	if !ok {
		return 0, 0, false, store.ErrNotFound
	}

	// Gate the increment on the attempt's running→failed transition. Only the
	// first close of a still-running attempt counts; redeliveries no-op.
	attempt, ok := s.taskAttempts[attemptID]
	if ok && attempt.Status == store.AttemptStatusRunning {
		firstClose = true
		attempt.Status = store.AttemptStatusFailed
		endedAt := now
		attempt.EndedAt = &endedAt
		if exitCode != nil {
			code := *exitCode
			attempt.ExitCode = &code
		}
		if sessionID != "" {
			attempt.SessionID = sessionID
		}
		if message != "" {
			attempt.Message = message
		}
		s.taskAttempts[attemptID] = attempt

		t.FailedAttempts++
		t.UpdatedAt = now
		s.tasks[taskID] = t

		j.FailedAttempts++
		j.UpdatedAt = now
		s.jobs[t.JobID] = j
	}

	return t.FailedAttempts, j.FailedAttempts, firstClose, nil
}

// RequeueTaskForRetry implements [store.TaskStore]. It returns the task to
// [store.TaskStatusReady], clears its worker assignment, and stamps RetryAfter.
// Guarded to assigned/running; anything else (including a missing task) is a
// legitimate no-op reported as false.
func (s *Store) RequeueTaskForRetry(_ context.Context, taskID string, retryAfter, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[taskID]
	if !ok || (t.Status != store.TaskStatusAssigned && t.Status != store.TaskStatusRunning) {
		return false, nil
	}
	t.Status = store.TaskStatusReady
	t.AssignedWorkerID = ""
	t.AssignedAt = nil
	ra := retryAfter
	t.RetryAfter = &ra
	t.FailureReason = ""
	t.UpdatedAt = now
	s.tasks[taskID] = t
	return true, nil
}

// cmpReadyTask orders tasks by job priority descending, then CreatedAt ascending.
func cmpReadyTask(a, b store.Task, jobPriority map[string]int) int {
	if n := cmp.Compare(jobPriority[b.JobID], jobPriority[a.JobID]); n != 0 {
		return n // higher priority first
	}
	return a.CreatedAt.Compare(b.CreatedAt)
}
