// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"slices"
	"strings"
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
	job.DependsOn = slices.Clone(s.jobDependencies[id])
	if job.DependsOn == nil {
		job.DependsOn = []string{}
	}
	return job, nil
}

// CreateJobDependencies records that jobID waits on each upstream ID.
// Duplicate edges (already-recorded upstream IDs) are ignored.
func (s *Store) CreateJobDependencies(_ context.Context, jobID string, upstreamIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.jobDependencies[jobID]
	for _, up := range upstreamIDs {
		if slices.Contains(existing, up) {
			continue
		}
		existing = append(existing, up)
	}
	s.jobDependencies[jobID] = existing
	return nil
}

// ListJobDependencyIDs returns the upstream job IDs jobID waits on, ordered
// by upstream job ID (matching the SQLite implementation's ORDER BY).
func (s *Store) ListJobDependencyIDs(_ context.Context, jobID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := slices.Clone(s.jobDependencies[jobID])
	slices.Sort(out)
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// ListDependents returns the IDs of jobs that declared a dependency on
// upstreamJobID, ordered by dependent job ID.
func (s *Store) ListDependents(_ context.Context, upstreamJobID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := []string{}
	for jobID, ups := range s.jobDependencies {
		if slices.Contains(ups, upstreamJobID) {
			out = append(out, jobID)
		}
	}
	slices.Sort(out) // deterministic for tests
	return out, nil
}

// ListBlockedJobs returns every job currently in [store.JobStatusBlocked].
func (s *Store) ListBlockedJobs(_ context.Context) ([]store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []store.Job
	for _, j := range s.jobs {
		if j.Status == store.JobStatusBlocked {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b store.Job) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
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
// status, started_at, completed_at, and parameters are preserved from the stored
// record to match the sqlite implementation which excludes those columns.
// parameters is persisted at submission time and is not mutable via UpdateJob.
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
	job.Parameters = existing.Parameters // persisted at submit; not mutable via UpdateJob (parity with SQLite)
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

// DemoteStalledJobs implements [store.JobStore]. It returns every running job
// with no assigned/running task — but at least one ready/pending task — to
// pending, and returns the demoted job IDs.
func (s *Store) DemoteStalledJobs(_ context.Context, now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Tally each job's task statuses in one pass.
	type counts struct{ active, schedulable int }
	byJob := make(map[string]*counts)
	for _, t := range s.tasks {
		c := byJob[t.JobID]
		if c == nil {
			c = &counts{}
			byJob[t.JobID] = c
		}
		switch t.Status {
		case store.TaskStatusAssigned, store.TaskStatusRunning:
			c.active++
		case store.TaskStatusReady, store.TaskStatusPending:
			c.schedulable++
		}
	}

	var demoted []string
	for id, job := range s.jobs {
		if job.Status != store.JobStatusRunning {
			continue
		}
		c := byJob[id]
		if c == nil || c.active > 0 || c.schedulable == 0 {
			continue
		}
		job.Status = store.JobStatusPending
		job.UpdatedAt = now
		s.jobs[id] = job
		demoted = append(demoted, id)
	}
	return demoted, nil
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

// DeleteTerminalJobsBefore implements [store.JobStore]. completed and canceled
// jobs are always eligible; failed jobs only when includeFailed. Effective
// completion time is CompletedAt, falling back to UpdatedAt when nil.
func (s *Store) DeleteTerminalJobsBefore(
	ctx context.Context, cutoff time.Time, includeFailed bool,
) ([]store.DeletedJob, error) {
	s.mu.Lock()
	var ids []string
	for id, j := range s.jobs {
		if !terminalJobEligible(j.Status, includeFailed) {
			continue
		}
		completed := j.UpdatedAt
		if j.CompletedAt != nil {
			completed = *j.CompletedAt
		}
		if !completed.Before(cutoff) {
			continue
		}
		ids = append(ids, id)
	}
	s.mu.Unlock()

	var deleted []store.DeletedJob
	for _, id := range ids {
		s.mu.Lock()
		j := s.jobs[id]
		s.mu.Unlock()
		if err := s.DeleteJob(ctx, id); err != nil {
			return nil, err
		}
		deleted = append(deleted, store.DeletedJob{
			ID: j.ID, Name: j.Name, FarmID: j.FarmID, QueueID: j.QueueID,
		})
	}
	return deleted, nil
}

// terminalJobEligible reports whether a job in the given status is eligible for
// retention deletion. completed and canceled always qualify; failed only when
// includeFailed is set.
func terminalJobEligible(status store.JobStatus, includeFailed bool) bool {
	switch status {
	case store.JobStatusCompleted, store.JobStatusCanceled:
		return true
	case store.JobStatusFailed:
		return includeFailed
	default:
		return false
	}
}

// DeleteJob implements [store.JobStore]. It removes the job and every in-memory
// row that belongs to it: usage claims (by attempt), task logs (by task), task
// attempts, tasks, and steps, mirroring the SQLite cascade. Returns
// [store.ErrNotFound] when the job is absent.
func (s *Store) DeleteJob(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[id]; !ok {
		return store.ErrNotFound
	}

	// Collect this job's task IDs, then its attempt IDs.
	taskIDs := make(map[string]struct{})
	for tid, t := range s.tasks {
		if t.JobID == id {
			taskIDs[tid] = struct{}{}
		}
	}
	attemptIDs := make(map[string]struct{})
	for aid, a := range s.taskAttempts {
		if _, ok := taskIDs[a.TaskID]; ok {
			attemptIDs[aid] = struct{}{}
		}
	}

	// Usage claims referencing this job's attempts.
	for cid, c := range s.usageClaims {
		if _, ok := attemptIDs[c.TaskAttemptID]; ok {
			delete(s.usageClaims, cid)
		}
	}
	// Task logs for this job's tasks (taskLogs is a slice).
	kept := s.taskLogs[:0:0]
	for _, l := range s.taskLogs {
		if _, ok := taskIDs[l.TaskID]; !ok {
			kept = append(kept, l)
		}
	}
	s.taskLogs = kept
	// Attempts, tasks, steps, job.
	for aid := range attemptIDs {
		delete(s.taskAttempts, aid)
	}
	for tid := range taskIDs {
		delete(s.tasks, tid)
	}
	for sid, st := range s.steps {
		if st.JobID == id {
			delete(s.steps, sid)
		}
	}
	// Outgoing job_dependencies edges only; incoming edges (other jobs
	// depending on id) are deliberately left for the reconciler.
	delete(s.jobDependencies, id)
	delete(s.jobs, id)
	return nil
}

// ParkJob implements [store.JobStore]. It transitions the job to
// [store.JobStatusPaused] and records reason, but only while the job is
// non-terminal; a terminal job (completed/failed/canceled) is a legitimate
// no-op.
func (s *Store) ParkJob(_ context.Context, jobID, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	switch j.Status {
	case store.JobStatusCompleted, store.JobStatusFailed, store.JobStatusCanceled:
		return nil // terminal: no-op
	}
	j.Status = store.JobStatusPaused
	j.ParkReason = reason
	j.UpdatedAt = now
	s.jobs[jobID] = j
	return nil
}

// ResumeJob implements [store.JobStore]. It transitions a paused job back to
// [store.JobStatusPending]; an auto-parked job (non-empty ParkReason) also has
// its ParkReason cleared and FailedAttempts reset. A job that is no longer
// paused is a legitimate no-op.
func (s *Store) ResumeJob(_ context.Context, jobID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	if j.Status != store.JobStatusPaused {
		return nil // no longer paused: no-op
	}
	j.Status = store.JobStatusPending
	if j.ParkReason != "" {
		j.FailedAttempts = 0
	}
	j.ParkReason = ""
	j.UpdatedAt = now
	s.jobs[jobID] = j
	return nil
}

// jobMatchesSearch reports whether j contains the given query string
// (case-insensitive) in any of its name, id, owner, or project fields.
func jobMatchesSearch(j store.Job, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(j.Name), q) ||
		strings.Contains(strings.ToLower(j.ID), q) ||
		strings.Contains(strings.ToLower(j.Owner), q) ||
		strings.Contains(strings.ToLower(j.Project), q)
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
	if opts.Search != "" && !jobMatchesSearch(j, opts.Search) {
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
