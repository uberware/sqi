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
func (s *Store) UpdateTaskStatus(_ context.Context, id string, status store.TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}

	task.Status = status
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	return nil
}

// TransitionStepPendingTasks transitions every pending task of the step to `to`,
// updates UpdatedAt, and returns the affected tasks.
func (s *Store) TransitionStepPendingTasks(_ context.Context, stepID string, to store.TaskStatus) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var affected []store.Task
	for id, t := range s.tasks {
		if t.StepID != stepID || t.Status != store.TaskStatusPending {
			continue
		}
		t.Status = to
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
		task.UpdatedAt = now
		s.tasks[id] = task
		count++
	}
	return count, nil
}

// ListReadyTasks returns up to limit tasks in [store.TaskStatusReady] that
// belong to non-paused queues within the given farm, ordered by job priority
// descending then CreatedAt ascending.
func (s *Store) ListReadyTasks(_ context.Context, farmID string, limit int) ([]store.Task, error) {
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
		if isReadyInFarm(t, farmID, s.jobs, queuePaused) {
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
// [store.TaskStatusCanceled] and returns those that were in
// [store.TaskStatusAssigned] or [store.TaskStatusRunning] at call time.
func (s *Store) CancelJobTasks(_ context.Context, jobID string, now time.Time) ([]store.Task, error) {
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
		task.UpdatedAt = now
		s.tasks[id] = task
	}
	return active, nil
}

// CountReadyTasksByQueue returns the number of tasks in [store.TaskStatusReady]
// state for each queue in the given farm, keyed by queue ID.
func (s *Store) CountReadyTasksByQueue(_ context.Context, farmID string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	counts := make(map[string]int)
	for _, t := range s.tasks {
		if t.Status != store.TaskStatusReady {
			continue
		}
		job, ok := s.jobs[t.JobID]
		if !ok || job.FarmID != farmID {
			continue
		}
		counts[job.QueueID]++
	}
	return counts, nil
}

// isReadyInFarm reports whether t is eligible for assignment: status Ready,
// job belongs to farmID (or farmID is "" meaning all farms), and the job's
// queue is not paused.
func isReadyInFarm(t store.Task, farmID string, jobs map[string]store.Job, queuePaused map[string]bool) bool {
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
	return !queuePaused[job.QueueID]
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

// cmpReadyTask orders tasks by job priority descending, then CreatedAt ascending.
func cmpReadyTask(a, b store.Task, jobPriority map[string]int) int {
	if n := cmp.Compare(jobPriority[b.JobID], jobPriority[a.JobID]); n != 0 {
		return n // higher priority first
	}
	return a.CreatedAt.Compare(b.CreatedAt)
}
