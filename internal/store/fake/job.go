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

	job = normalizeDeclaredExtensions(job)
	s.jobs[job.ID] = job
	return job, nil
}

// normalizeDeclaredExtensions mirrors what a round trip through the SQLite
// declared_extensions column does to the pair of fields carrying a job's
// declared OpenJD extensions.
//
// The tri-state must survive identically on both backends, because the
// scheduler branches on all three: NOT RECORDED falls back to scanning the raw
// template, RECORDED-EMPTY does not, and RECORDED-NON-EMPTY answers outright.
// SQLite stores ” for the first and '[]' for the second and reads a recorded
// row back as a non-nil slice; without this, the fake would hand back a nil
// slice where SQLite hands back an empty one, and a nil-vs-empty assumption
// could pass every fake-backed test and fail in production.
func normalizeDeclaredExtensions(job store.Job) store.Job {
	if !job.ExtensionsRecorded {
		job.DeclaredExtensions = nil
		return job
	}
	job.DeclaredExtensions = copySlice(job.DeclaredExtensions)
	if job.DeclaredExtensions == nil {
		job.DeclaredExtensions = []string{}
	}
	return job
}

// CreateJobSubmission implements [store.JobStore]. It validates the whole
// submission before mutating anything, so a rejected submission leaves the
// store untouched — the in-memory equivalent of the SQLite implementation's
// transaction.
func (s *Store) CreateJobSubmission(_ context.Context, sub store.JobSubmission) (store.JobSubmission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validateSubmission(sub); err != nil {
		return store.JobSubmission{}, err
	}

	now := time.Now().UTC()
	out := store.JobSubmission{
		DependsOn: copySlice(sub.DependsOn),
		Steps:     make([]store.Step, 0, len(sub.Steps)),
		Tasks:     make([]store.Task, 0, len(sub.Tasks)),
	}

	job := normalizeDeclaredExtensions(sub.Job)
	job.Parameters = copyMap(job.Parameters)
	job.CreatedAt, job.UpdatedAt = now, now
	s.jobs[job.ID] = job
	out.Job = job

	existing := s.jobDependencies[job.ID]
	for _, up := range sub.DependsOn {
		if slices.Contains(existing, up) {
			continue
		}
		existing = append(existing, up)
	}
	if len(existing) > 0 {
		s.jobDependencies[job.ID] = existing
	}

	// Each step and task is stamped with its own time.Now(), mirroring the
	// SQLite implementation, where tasks within a step sharing one created_at
	// would silently disable the ready-task ordering tiebreaker and destabilize
	// ListTasks paging (see insertTasksTx in sqlite/job.go).
	for _, step := range sub.Steps {
		rowNow := time.Now().UTC()
		step.DependsOn = copySlice(step.DependsOn)
		step.CreatedAt, step.UpdatedAt = rowNow, rowNow
		s.steps[step.ID] = step
		out.Steps = append(out.Steps, step)
	}
	for _, task := range sub.Tasks {
		rowNow := time.Now().UTC()
		task.Parameters = copyMap(task.Parameters)
		task.CreatedAt, task.UpdatedAt = rowNow, rowNow
		s.tasks[task.ID] = task
		out.Tasks = append(out.Tasks, task)
	}

	return out, nil
}

// validateSubmission runs, before any mutation happens, the checks that make
// the fake reject what SQLite's schema would reject. Callers must hold s.mu.
//
// It mirrors exactly three constraints: the jobs primary key, the
// steps_job_name_unique UNIQUE (job_id, name) constraint, and the steps and
// tasks primary keys — each checked both within the submission and against
// what is already stored. Without the primary-key checks the fake does not
// merely accept a duplicate ID, it silently LOSES the row (the map assignment
// overwrites) and still reports success, so a Submit regression that reused an
// ID would be green through every fake-backed test and ErrConflict only in
// production.
//
// It does NOT mirror SQLite's foreign keys: a submission naming a nonexistent
// farm, queue or step is accepted here. That gap is pre-existing in CreateJob,
// CreateStep and CreateTask and is deliberately left alone rather than closed
// only on this one path. (job_dependencies.depends_on_job_id carries no FK at
// all, so accepting an edge to a nonexistent upstream job is correct parity.)
func (s *Store) validateSubmission(sub store.JobSubmission) error {
	if _, exists := s.jobs[sub.Job.ID]; exists {
		return store.ErrConflict
	}
	if err := s.validateSubmissionSteps(sub.Steps); err != nil {
		return err
	}
	return s.validateSubmissionTasks(sub.Tasks)
}

// validateSubmissionTasks rejects a task ID that collides within the
// submission or with a stored task. Callers must hold s.mu.
func (s *Store) validateSubmissionTasks(tasks []store.Task) error {
	ids := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if _, dup := ids[task.ID]; dup {
			return store.ErrConflict
		}
		if _, exists := s.tasks[task.ID]; exists {
			return store.ErrConflict
		}
		ids[task.ID] = struct{}{}
	}
	return nil
}

// validateSubmissionSteps rejects a step ID or a (job_id, name) pair that
// collides within the submission or with a stored step. Callers must hold
// s.mu.
func (s *Store) validateSubmissionSteps(steps []store.Step) error {
	ids := make(map[string]struct{}, len(steps))
	names := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		if _, dup := ids[step.ID]; dup {
			return store.ErrConflict
		}
		if _, exists := s.steps[step.ID]; exists {
			return store.ErrConflict
		}
		ids[step.ID] = struct{}{}

		key := step.JobID + "\x00" + step.Name
		if _, dup := names[key]; dup {
			return store.ErrConflict
		}
		names[key] = struct{}{}
		for _, existing := range s.steps {
			if existing.JobID == step.JobID && existing.Name == step.Name {
				return store.ErrConflict
			}
		}
	}
	return nil
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
	// Same reason: declared_extensions is derived from the template at
	// submission and is not on sqlUpdateJob's SET list.
	job.DeclaredExtensions = copySlice(existing.DeclaredExtensions)
	job.ExtensionsRecorded = existing.ExtensionsRecorded
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
//
// A candidate still referenced as an upstream (depends_on_job_id) by a
// non-terminal dependent is skipped: purging it would leave the dependent's
// reconciler reading a GetJob ErrNotFound for an upstream that actually
// succeeded, which it treats as "missing" and wrongly cancels the dependent.
// Manual DeleteJob intentionally keeps the cancel-dependents behavior — this
// guard only applies to the automatic retention sweep.
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
		if s.neededByNonTerminalDependentLocked(id) {
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

// neededByNonTerminalDependentLocked reports whether candidateID is recorded
// as an upstream (depends_on_job_id) of some dependent job that has not yet
// reached a terminal status. Callers must hold s.mu.
func (s *Store) neededByNonTerminalDependentLocked(candidateID string) bool {
	for dependentID, ups := range s.jobDependencies {
		if !slices.Contains(ups, candidateID) {
			continue
		}
		if dep, ok := s.jobs[dependentID]; ok && !dep.Status.IsTerminal() {
			return true
		}
	}
	return false
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

// jobMatchesSearch reports whether every whitespace-separated term in query
// matches (case-insensitive substring) one of j's name, id, owner, or project
// fields. Uses the shared matcher so it stays in lockstep with the SQLite
// LIKE search.
func jobMatchesSearch(j store.Job, query string) bool {
	return store.MatchesSearch(query, j.Name, j.ID, j.Owner, j.Project)
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
	if opts.Owner != "" && !strings.EqualFold(j.Owner, opts.Owner) {
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
