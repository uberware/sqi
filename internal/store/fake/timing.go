// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/uberware/sqi/internal/store"
)

// JobTimings implements [store.TimingStore].
func (s *Store) JobTimings(_ context.Context, jobIDs []string) (map[string]store.JobTiming, error) {
	out := make(map[string]store.JobTiming, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []store.Job
	for _, id := range jobIDs {
		if j, ok := s.jobs[id]; ok {
			jobs = append(jobs, j)
		}
	}
	for _, jt := range s.jobTimingsLocked(jobs) {
		out[jt.JobID] = jt
	}
	return out, nil
}

// QueueJobTimings implements [store.TimingStore]. Jobs are ordered by ID, as
// on the SQL stores.
func (s *Store) QueueJobTimings(_ context.Context, queueIDs []string) ([]store.JobTiming, error) {
	if len(queueIDs) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []store.Job
	for _, j := range s.jobs {
		if slices.Contains(queueIDs, j.QueueID) && !j.Status.IsTerminal() {
			jobs = append(jobs, j)
		}
	}
	return s.jobTimingsLocked(jobs), nil
}

// runningEntry is a running task's timing with the task ID it is ordered by
// after StartedAt, as the SQL stores order it.
type runningEntry struct {
	store.RunningTiming

	taskID string
}

// jobTimingsLocked builds the timing inputs of jobs, ordered by job ID.
// Caller holds s.mu.
func (s *Store) jobTimingsLocked(jobs []store.Job) []store.JobTiming {
	slices.SortFunc(jobs, func(a, b store.Job) int { return strings.Compare(a.ID, b.ID) })
	out := make([]store.JobTiming, len(jobs))
	index := make(map[string]int, len(jobs))
	steps := make([]map[string]*store.StepTiming, len(jobs))
	running := make([][]runningEntry, len(jobs))
	for i, j := range jobs {
		out[i] = store.JobTiming{
			JobID: j.ID, QueueID: j.QueueID, Status: j.Status, EstimatedSeconds: copyIntPtr(j.EstimatedSeconds),
		}
		index[j.ID] = i
		steps[i] = map[string]*store.StepTiming{}
	}
	for _, t := range s.tasks {
		i, ok := index[t.JobID]
		if !ok {
			continue
		}
		st := steps[i][t.StepID]
		if st == nil {
			st = &store.StepTiming{StepID: t.StepID}
			steps[i][t.StepID] = st
		}
		s.countTaskTimingLocked(st, t)
		if t.Status != store.TaskStatusRunning {
			continue
		}
		if a, ok := s.timingAttemptLocked(t.ID); ok {
			running[i] = append(running[i], runningEntry{taskID: t.ID, RunningTiming: store.RunningTiming{
				StepID: t.StepID, StartedAt: a.StartedAt, Progress: copyIntPtr(a.Progress),
			}})
		}
	}
	for i := range out {
		for _, st := range steps[i] {
			out[i].Steps = append(out[i].Steps, *st)
		}
		slices.SortFunc(out[i].Steps, func(a, b store.StepTiming) int { return strings.Compare(a.StepID, b.StepID) })
		slices.SortFunc(running[i], func(a, b runningEntry) int {
			return cmp.Or(a.StartedAt.Compare(b.StartedAt), strings.Compare(a.taskID, b.taskID))
		})
		for _, r := range running[i] {
			out[i].Running = append(out[i].Running, r.RunningTiming)
		}
	}
	return out
}

// countTaskTimingLocked counts t into st. A succeeded task is measured when its
// latest attempt succeeded with an EndedAt. Caller holds s.mu.
func (s *Store) countTaskTimingLocked(st *store.StepTiming, t store.Task) {
	switch t.Status {
	case store.TaskStatusPending, store.TaskStatusReady, store.TaskStatusAssigned:
		st.NotStarted++
	case store.TaskStatusRunning:
		st.Running++
	case store.TaskStatusSucceeded:
		st.Succeeded++
		if a, ok := s.timingAttemptLocked(t.ID); ok && a.Status == store.AttemptStatusSucceeded && a.EndedAt != nil {
			st.Measured++
			st.MeasuredSeconds += a.EndedAt.Sub(a.StartedAt).Seconds()
		}
	case store.TaskStatusFailed:
		st.Failed++
	case store.TaskStatusCanceled:
		st.Canceled++
	}
}

// timingAttemptLocked returns taskID's latest attempt as the SQL stores pick it
// for timing: the highest AttemptNumber, and of two sharing it the greater ID.
// Caller holds s.mu.
func (s *Store) timingAttemptLocked(taskID string) (store.TaskAttempt, bool) {
	var found store.TaskAttempt
	var exists bool
	for _, a := range s.taskAttempts {
		if a.TaskID != taskID {
			continue
		}
		if !exists || cmp.Or(cmp.Compare(a.AttemptNumber, found.AttemptNumber), strings.Compare(a.ID, found.ID)) > 0 {
			found, exists = a, true
		}
	}
	return found, exists
}
