// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateJob inserts a new job with all fields populated by the caller.
func (s *Store) CreateJob(_ context.Context, job store.Job) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.jobs[job.ID] = job
	return job, nil
}

// GetJob returns the job with the given ID, or [store.ErrNotFound].
func (s *Store) GetJob(_ context.Context, id string) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	return job, nil
}

// ListJobs returns a paginated, filtered, and sorted page of jobs matching opts.
func (s *Store) ListJobs(_ context.Context, opts store.ListJobsOptions) (store.Page[store.Job], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := opts.Pagination.Validate(); err != nil {
		return store.Page[store.Job]{}, err
	}

	jobs := make([]store.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if filterJob(j, opts) {
			jobs = append(jobs, j)
		}
	}

	slices.SortStableFunc(jobs, func(a, b store.Job) int {
		return cmpJob(a, b, opts.SortBy, opts.SortDir)
	})

	return applyPage(jobs, opts.Pagination), nil
}

// UpdateJob replaces only the mutable user-settable fields of an existing job.
// status, started_at, and completed_at are preserved from the stored record
// to match the sqlite implementation which excludes those columns.
func (s *Store) UpdateJob(_ context.Context, job store.Job) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.jobs[job.ID]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}

	// Preserve lifecycle fields — these are owned by UpdateJobStatus /
	// CancelJobStatus, not by UpdateJob.
	job.Status = existing.Status
	job.StartedAt = existing.StartedAt
	job.CompletedAt = existing.CompletedAt
	job.CreatedAt = existing.CreatedAt
	job.UpdatedAt = time.Now()
	s.jobs[job.ID] = job
	return job, nil
}

// UpdateJobStatus transitions a job to a new status and updates UpdatedAt.
// If the new status is [store.JobStatusRunning] and StartedAt is nil, StartedAt
// is set to the current time. Terminal statuses set CompletedAt.
func (s *Store) UpdateJobStatus(_ context.Context, id string, status store.JobStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return store.ErrNotFound
	}

	now := time.Now()
	job.Status = status
	job.UpdatedAt = now

	if status == store.JobStatusRunning && job.StartedAt == nil {
		job.StartedAt = &now
	}

	if status == store.JobStatusCompleted ||
		status == store.JobStatusFailed ||
		status == store.JobStatusCanceled {
		job.CompletedAt = &now
	}

	s.jobs[id] = job
	return nil
}

// CancelJobStatus implements [store.JobStore].
// Transitions the job to canceled unless it is already completed or failed.
func (s *Store) CancelJobStatus(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return store.ErrNotFound
	}

	switch job.Status {
	case store.JobStatusCanceled:
		return nil // idempotent
	case store.JobStatusCompleted, store.JobStatusFailed:
		return store.ErrConflict
	}

	now := time.Now()
	job.Status = store.JobStatusCanceled
	job.CompletedAt = &now
	job.UpdatedAt = now
	s.jobs[id] = job
	return nil
}

// filterJob reports whether j matches all non-zero filter fields in opts.
func filterJob(j store.Job, opts store.ListJobsOptions) bool {
	if opts.FarmID != "" && j.FarmID != opts.FarmID {
		return false
	}
	if opts.QueueID != "" && j.QueueID != opts.QueueID {
		return false
	}
	if opts.Status != "" && j.Status != opts.Status {
		return false
	}
	if opts.Owner != "" && j.Owner != opts.Owner {
		return false
	}
	if opts.Project != "" && j.Project != opts.Project {
		return false
	}
	return true
}

// cmpJob returns a comparison value for two jobs by the given sort field and
// direction. An empty field defaults to [store.JobSortByCreatedAt]; an empty
// direction defaults to ascending.
func cmpJob(a, b store.Job, field store.JobSortField, dir store.SortDir) int {
	var n int
	switch field {
	case store.JobSortByPriority:
		n = cmp.Compare(a.Priority, b.Priority)
	case store.JobSortByStatus:
		n = cmp.Compare(string(a.Status), string(b.Status))
	case store.JobSortByUpdatedAt:
		n = a.UpdatedAt.Compare(b.UpdatedAt)
	case store.JobSortByName:
		n = cmp.Compare(a.Name, b.Name)
	default: // JobSortByCreatedAt
		n = a.CreatedAt.Compare(b.CreatedAt)
	}
	if dir == store.SortDesc {
		return -n
	}
	return n
}
