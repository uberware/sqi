// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/eta"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// timingFailStore fails the timing reads it is told to and counts the calls.
type timingFailStore struct {
	store.Store

	failJobs, failQueues bool
	jobCalls, queueCalls int
	queueIDs             []string
	jobIDs               []string
}

func (s *timingFailStore) JobTimings(ctx context.Context, ids []string) (map[string]store.JobTiming, error) {
	s.jobCalls++
	s.jobIDs = append(s.jobIDs, ids...)
	if s.failJobs {
		return nil, errors.New("timing boom")
	}
	return s.Store.JobTimings(ctx, ids)
}

func (s *timingFailStore) QueueJobTimings(ctx context.Context, ids []string) ([]store.JobTiming, error) {
	s.queueCalls++
	s.queueIDs = append(s.queueIDs, ids...)
	if s.failQueues {
		return nil, errors.New("timing boom")
	}
	return s.Store.QueueJobTimings(ctx, ids)
}

func withEstimate(t *testing.T, st store.Store, j store.Job, est int) {
	t.Helper()
	j.EstimatedSeconds = &est
	if _, err := st.UpdateJob(t.Context(), j); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
}

func getJSON(t *testing.T, h http.Handler, target string) map[string]json.RawMessage {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newUnscopedJobsReq(t, target))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", target, rr.Code, rr.Body)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func itemsByID(t *testing.T, m map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(m["items"], &items); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, it := range items {
		var id string
		_ = json.Unmarshal(it["id"], &id) //nolint:errcheck // test decode
		out[id] = it
	}
	return out
}

func rawInt(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("not an int: %s", raw)
	}
	return n
}

func assertAsOf(t *testing.T, it map[string]json.RawMessage) {
	t.Helper()
	var s string
	if err := json.Unmarshal(it["as_of"], &s); err != nil {
		t.Fatalf("as_of missing or not a string: %s", it["as_of"])
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		t.Fatalf("as_of %q: %v", s, err)
	}
}

func TestListJobs_RemainingSeconds(t *testing.T) {
	st := fake.New()
	est, _ := seedJobTasks(t, st, store.JobStatusPending, store.Task{Name: "a", Status: store.TaskStatusReady})
	withEstimate(t, st, est, 600)
	unknown := seedJob(t, st, store.JobStatusPending)
	done := seedJob(t, st, store.JobStatusCompleted)

	items := itemsByID(t, getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs"))

	if got := rawInt(t, items[est.ID]["remaining_seconds"]); got != 600 {
		t.Errorf("estimated job remaining_seconds = %d, want 600", got)
	}
	if got := string(items[est.ID]["remaining_basis"]); got != `"estimate"` {
		t.Errorf("estimated job remaining_basis = %s", got)
	}
	if got := string(items[unknown.ID]["remaining_seconds"]); got != "null" {
		t.Errorf("unknown job remaining_seconds = %s, want null", got)
	}
	if _, ok := items[unknown.ID]["remaining_basis"]; ok {
		t.Errorf("unknown job has remaining_basis %s", items[unknown.ID]["remaining_basis"])
	}
	if got := rawInt(t, items[done.ID]["remaining_seconds"]); got != 0 {
		t.Errorf("completed job remaining_seconds = %d, want 0", got)
	}
	if got := string(items[done.ID]["remaining_basis"]); got != `"finished"` {
		t.Errorf("completed job remaining_basis = %s", got)
	}
	for id, it := range items {
		assertAsOf(t, it)
		if _, ok := it["remaining_seconds"]; !ok {
			t.Errorf("job %s lacks remaining_seconds", id)
		}
	}
}

func TestListJobs_TimingsReadOncePerPage(t *testing.T) {
	base := fake.New()
	st := &timingFailStore{Store: base}
	for range 3 {
		seedJob(t, base, store.JobStatusPending)
	}
	getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs")
	if st.jobCalls != 1 {
		t.Fatalf("JobTimings calls = %d, want 1", st.jobCalls)
	}
}

func TestListJobs_TerminalOnlyPageSkipsTimingRead(t *testing.T) {
	base := fake.New()
	st := &timingFailStore{Store: base, failJobs: true}
	done := seedJob(t, base, store.JobStatusCompleted)
	failed := seedJob(t, base, store.JobStatusFailed)

	items := itemsByID(t, getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs"))
	if st.jobCalls != 0 {
		t.Fatalf("JobTimings calls = %d, want 0 for a terminal-only page", st.jobCalls)
	}
	for _, id := range []string{done.ID, failed.ID} {
		if got := rawInt(t, items[id]["remaining_seconds"]); got != 0 {
			t.Errorf("job %s remaining_seconds = %d, want 0", id, got)
		}
		if got := string(items[id]["remaining_basis"]); got != `"finished"` {
			t.Errorf("job %s remaining_basis = %s", id, got)
		}
	}
}

func TestListJobs_MixedPageReadsOnlyNonTerminal(t *testing.T) {
	base := fake.New()
	st := &timingFailStore{Store: base}
	live := seedJob(t, base, store.JobStatusPending)
	seedJob(t, base, store.JobStatusCompleted)

	getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs")
	if st.jobCalls != 1 || len(st.jobIDs) != 1 || st.jobIDs[0] != live.ID {
		t.Fatalf("JobTimings calls = %d ids = %v, want one call for [%s]", st.jobCalls, st.jobIDs, live.ID)
	}
}

func TestGetJob_TerminalSkipsTimingRead(t *testing.T) {
	base := fake.New()
	st := &timingFailStore{Store: base, failJobs: true}
	done := seedJob(t, base, store.JobStatusCompleted)

	m := getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs/"+done.ID)
	if st.jobCalls != 0 {
		t.Fatalf("JobTimings calls = %d, want 0", st.jobCalls)
	}
	if got := rawInt(t, m["remaining_seconds"]); got != 0 {
		t.Errorf("remaining_seconds = %d, want 0", got)
	}
	if got := string(m["remaining_basis"]); got != `"finished"` {
		t.Errorf("remaining_basis = %s", got)
	}
}

func TestGetJob_RemainingSeconds_Measured(t *testing.T) {
	st := fake.New()
	job, tasks := seedJobTasks(t, st, store.JobStatusRunning,
		store.Task{Name: "a", Status: store.TaskStatusSucceeded},
		store.Task{Name: "b", Status: store.TaskStatusReady},
		store.Task{Name: "c", Status: store.TaskStatusReady})
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(60 * time.Second)
	storetest.InjectAttempt(t, st, store.TaskAttempt{
		TaskID: tasks[0].ID, AttemptNumber: 1, Status: store.AttemptStatusSucceeded, StartedAt: start, EndedAt: &end,
	})

	m := getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs/"+job.ID)
	if got := rawInt(t, m["remaining_seconds"]); math.Abs(float64(got-120)) > 2 {
		t.Errorf("remaining_seconds = %d, want ~120", got)
	}
	if got := string(m["remaining_basis"]); got != `"measured"` {
		t.Errorf("remaining_basis = %s", got)
	}
	assertAsOf(t, m)
}

func TestGetJob_RemainingSeconds_TimingFailure(t *testing.T) {
	base := fake.New()
	job := seedJob(t, base, store.JobStatusPending)
	withEstimate(t, base, job, 600)
	st := &timingFailStore{Store: base, failJobs: true}

	m := getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs/"+job.ID)
	if got := string(m["remaining_seconds"]); got != "null" {
		t.Errorf("remaining_seconds = %s, want null", got)
	}
	if _, ok := m["remaining_basis"]; ok {
		t.Errorf("remaining_basis present on timing failure")
	}
}

func TestListJobs_RemainingSeconds_TimingFailure(t *testing.T) {
	base := fake.New()
	seedJob(t, base, store.JobStatusPending)
	st := &timingFailStore{Store: base, failJobs: true}
	items := itemsByID(t, getJSON(t, newJobRouter(st, &fakeScheduler{}), "/api/v1/jobs"))
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	for _, it := range items {
		if string(it["remaining_seconds"]) != "null" {
			t.Errorf("remaining_seconds = %s", it["remaining_seconds"])
		}
	}
}

// seedQueueTiming seeds queue A (an estimated pending job, an unknown pending
// job, a running job with one running task) and an empty queue B.
func seedQueueTiming(t *testing.T, st *fake.Store) (a, b store.Queue, runningJob store.Job) {
	t.Helper()
	_, a = seedFarmQueue(t, st)
	b, err := st.CreateQueue(t.Context(), store.Queue{ID: "queue-b", FarmID: a.FarmID, Name: "qb"})
	if err != nil {
		t.Fatal(err)
	}
	estJob, _ := seedJobTasks(t, st, store.JobStatusPending, store.Task{Name: "e", Status: store.TaskStatusReady})
	withEstimate(t, st, estJob, 600)
	seedJob(t, st, store.JobStatusPending)
	runningJob, _ = seedJobTasks(t, st, store.JobStatusRunning, store.Task{Name: "r", Status: store.TaskStatusRunning})
	withEstimate(t, st, runningJob, 1000)
	return a, b, runningJob
}

func wantRunningJobFigure(t *testing.T, st *fake.Store, j store.Job) int {
	t.Helper()
	timings, err := st.JobTimings(t.Context(), []string{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	r := eta.Job(timings[j.ID], time.Now().UTC())
	if r.Seconds == nil {
		t.Fatal("running job has no figure")
	}
	return int(*r.Seconds)
}

func assertQueueA(t *testing.T, it map[string]json.RawMessage, runningFigure int) {
	t.Helper()
	if got := rawInt(t, it["unfinished_jobs"]); got != 3 {
		t.Errorf("unfinished_jobs = %d, want 3", got)
	}
	if got := rawInt(t, it["jobs_without_estimate"]); got != 1 {
		t.Errorf("jobs_without_estimate = %d, want 1", got)
	}
	if got := rawInt(t, it["running_tasks"]); got != 1 {
		t.Errorf("running_tasks = %d, want 1", got)
	}
	if got := rawInt(t, it["work_remaining_seconds"]); math.Abs(float64(got-(600+runningFigure))) > 2 {
		t.Errorf("work_remaining_seconds = %d, want ~%d", got, 600+runningFigure)
	}
	assertAsOf(t, it)
}

func TestListQueues_Timing(t *testing.T) {
	base := fake.New()
	a, b, rj := seedQueueTiming(t, base)
	st := &timingFailStore{Store: base}
	items := itemsByID(t, getJSON(t, newQueueRouter(st), "/api/v1/queues"))

	assertQueueA(t, items[a.ID], wantRunningJobFigure(t, base, rj))
	for _, k := range []string{"unfinished_jobs", "work_remaining_seconds", "jobs_without_estimate", "running_tasks"} {
		if got := rawInt(t, items[b.ID][k]); got != 0 {
			t.Errorf("queue B %s = %d, want 0", k, got)
		}
	}
	assertAsOf(t, items[b.ID])
	if st.queueCalls != 1 {
		t.Errorf("QueueJobTimings calls = %d, want exactly 1", st.queueCalls)
	}
	if len(st.queueIDs) != 2 {
		t.Errorf("QueueJobTimings queue IDs = %v, want both queues", st.queueIDs)
	}
}

func TestGetQueue_Timing(t *testing.T) {
	base := fake.New()
	a, _, rj := seedQueueTiming(t, base)
	m := getJSON(t, newQueueRouter(base), "/api/v1/queues/"+a.ID)
	assertQueueA(t, m, wantRunningJobFigure(t, base, rj))
}

func TestListQueues_TimingFailure(t *testing.T) {
	base := fake.New()
	a, _, _ := seedQueueTiming(t, base)
	st := &timingFailStore{Store: base, failQueues: true}
	items := itemsByID(t, getJSON(t, newQueueRouter(st), "/api/v1/queues"))
	if _, ok := items[a.ID]; !ok || len(items) != 2 {
		t.Fatalf("items = %d, want both queues", len(items))
	}
	for id, it := range items {
		for _, k := range []string{"unfinished_jobs", "work_remaining_seconds", "jobs_without_estimate", "running_tasks", "as_of"} {
			if _, ok := it[k]; ok {
				t.Errorf("queue %s has %s on timing failure", id, k)
			}
		}
	}
}

func TestGetQueue_TimingFailure(t *testing.T) {
	base := fake.New()
	a, _, _ := seedQueueTiming(t, base)
	st := &timingFailStore{Store: base, failQueues: true}
	m := getJSON(t, newQueueRouter(st), "/api/v1/queues/"+a.ID)
	if _, ok := m["unfinished_jobs"]; ok {
		t.Error("unfinished_jobs present on timing failure")
	}
}

func TestCreateQueue_NoTiming(t *testing.T) {
	st := fake.New()
	f, _ := seedFarmAndQueue(t, st)
	rr := httptest.NewRecorder()
	body := `{"farm_id":"` + f.ID + `","name":"fresh"}`
	newQueueRouter(st).ServeHTTP(rr, newReq(t, http.MethodPost, "/api/v1/queues", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	for _, k := range []string{"work_remaining_seconds", "unfinished_jobs", "as_of"} {
		if strings.Contains(rr.Body.String(), k) {
			t.Errorf("create response contains %s: %s", k, rr.Body)
		}
	}
}
