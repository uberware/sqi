// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// remaining_test.go — a job's remaining time and a queue's remaining work,
// from a real worker's progress and finished tasks, through the REST API.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// remainingGateYAML is a one-task job that prints nothing for six seconds,
// reports 50% progress, then keeps running for thirty. The linear estimate
// needs thirty seconds of run time, so for that long the job's remaining time
// comes from its estimate.
const remainingGateYAML = `specificationVersion: jobtemplate-2023-09
name: remaining-gate
steps:
  - name: work
    script:
      actions:
        onRun:
          command: /bin/sh
          args:
            - "-c"
            - "sleep 6; echo 'openjd_progress: 50'; sleep 30"
`

// remainingQuickYAML is a one-task job that finishes at once.
const remainingQuickYAML = `specificationVersion: jobtemplate-2023-09
name: remaining-quick
steps:
  - name: work
    script:
      actions:
        onRun:
          command: /bin/sh
          args: ["-c", "true"]
`

// remainingMeasuredYAML is one step of three tasks, each sleeping Frame x 5
// seconds and printing no progress.
const remainingMeasuredYAML = `specificationVersion: jobtemplate-2023-09
name: remaining-measured
steps:
  - name: work
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-3"
    script:
      actions:
        onRun:
          command: /bin/sh
          args:
            - "-c"
            - "sleep $(( {{Task.Param.Frame}} * 5 ))"
`

// remainingJobResp is the slice of GET /api/v1/jobs/{id} this test reads.
type remainingJobResp struct {
	Status           string   `json:"status"`
	RemainingSeconds *int64   `json:"remaining_seconds"`
	RemainingBasis   string   `json:"remaining_basis"`
	ProductName      string   `json:"product_name"`
	ProductVersion   string   `json:"product_version"`
	EstimatedSeconds *int     `json:"estimated_seconds"`
	ProgressPercent  *float64 `json:"progress_percent"`
}

// remainingQueueResp is the slice of GET /api/v1/queues/{id} this test reads.
type remainingQueueResp struct {
	UnfinishedJobs       int   `json:"unfinished_jobs"`
	WorkRemainingSeconds int64 `json:"work_remaining_seconds"`
	JobsWithoutEstimate  int   `json:"jobs_without_estimate"`
	RunningTasks         int   `json:"running_tasks"`
}

func getRemainingJob(t *testing.T, ts *testServer, jobID string) remainingJobResp {
	t.Helper()
	var r remainingJobResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &r)
	return r
}

func getRemainingQueue(t *testing.T, ts *testServer, queueID string) remainingQueueResp {
	t.Helper()
	var r remainingQueueResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/queues/"+queueID+"?include=timing"), nil, "", http.StatusOK, &r)
	return r
}

// waitRemaining polls the job and the queue until ok accepts them, failing
// after timeout with the last values seen.
func waitRemaining(t *testing.T, ts *testServer, jobID, queueID string, timeout time.Duration, what string, ok func(remainingJobResp, remainingQueueResp) bool) (remainingJobResp, remainingQueueResp) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j := getRemainingJob(t, ts, jobID)
		q := getRemainingQueue(t, ts, queueID)
		if ok(j, q) {
			return j, q
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s, after %s: job %+v (remaining %v), queue %+v", what, timeout, j, derefI64(j.RemainingSeconds), q)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// derefI64 renders an optional remaining time for a message; -1 means null.
func derefI64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// submitEstimated submits a raw template with an estimate and optional
// dependencies.
func submitEstimated(t *testing.T, ts *testServer, farmID, queueID, yaml string, estimate int, dependsOn ...string) string {
	t.Helper()
	q := url.Values{}
	q.Set("farm_id", farmID)
	q.Set("queue_id", queueID)
	q.Set("owner", "test")
	q.Set("estimated_seconds", strconv.Itoa(estimate))
	for _, d := range dependsOn {
		q.Add("depends_on", d)
	}
	target := apiURL(ts, "/api/v1/jobs") + "?" + q.Encode()
	var resp jobResp
	mustDoJSON(t, http.MethodPost, target, []byte(yaml), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("submitEstimated: server returned empty job ID")
	}
	return resp.ID
}

// TestRemainingTimeEndToEnd drives a real worker and asserts the remaining
// time of jobs and of a queue: from the submitted estimate, then from a
// measured per-step rate, then zero once finished; and that a product
// submission records which product it came from.
func TestRemainingTimeEndToEnd(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorker(t, ts, farmID, queueID)

	// A queue of three: the first runs, the other two wait on it.
	job1 := submitEstimated(t, ts, farmID, queueID, remainingGateYAML, 600)
	job2 := submitEstimated(t, ts, farmID, queueID, remainingQuickYAML, 600, job1)
	job3 := submitEstimated(t, ts, farmID, queueID, remainingQuickYAML, 600, job1)

	// Running, no progress yet: each job's whole estimate remains.
	j, q := waitRemaining(t, ts, job1, queueID, 60*time.Second, "job 1 running", func(_ remainingJobResp, q remainingQueueResp) bool {
		return q.RunningTasks == 1
	})
	if j.ProgressPercent != nil && *j.ProgressPercent > 0 {
		t.Fatalf("job 1 already reports progress %v before the gate; widen the sleep", *j.ProgressPercent)
	}
	if q.UnfinishedJobs != 3 || q.JobsWithoutEstimate != 0 || q.WorkRemainingSeconds != 1800 || q.RunningTasks != 1 {
		t.Fatalf("queue before progress: got %+v, want unfinished 3, without estimate 0, remaining 1800, running 1", q)
	}

	// Progress 50 and under thirty seconds of run time: the estimate, halved.
	j, q = waitRemaining(t, ts, job1, queueID, 30*time.Second, "job 1 at progress 50", func(j remainingJobResp, _ remainingQueueResp) bool {
		return j.ProgressPercent != nil && *j.ProgressPercent >= 50
	})
	if j.RemainingSeconds == nil || *j.RemainingSeconds != 300 || j.RemainingBasis != "estimate" {
		t.Fatalf("job 1 at progress 50: got remaining %v basis %q, want 300 estimate", derefI64(j.RemainingSeconds), j.RemainingBasis)
	}
	if q.WorkRemainingSeconds != 1500 {
		t.Fatalf("queue at progress 50: got remaining %d, want 1500 (%+v)", q.WorkRemainingSeconds, q)
	}

	// Let the queue drain so the measured-rate job has the worker to itself.
	for _, id := range []string{job1, job2, job3} {
		if final := pollJobStatus(t, ts, id, []string{"completed", "failed", "canceled"}, 120*time.Second); final != "completed" {
			t.Fatalf("job %s final status: got %q, want completed", id, final)
		}
	}
	done := getRemainingJob(t, ts, job1)
	if done.RemainingSeconds == nil || *done.RemainingSeconds != 0 || done.RemainingBasis != "finished" {
		t.Fatalf("finished job 1: got remaining %v basis %q, want 0 finished", derefI64(done.RemainingSeconds), done.RemainingBasis)
	}
	if done.ProductName != "" || done.ProductVersion != "" {
		t.Errorf("raw-template job records product %q %q, want none", done.ProductName, done.ProductVersion)
	}

	// Measured rate: no estimate, three tasks of five, ten and fifteen seconds.
	measured := submitJobCustomYAML(t, ts, farmID, queueID, remainingMeasuredYAML)
	if m := getRemainingJob(t, ts, measured); m.RemainingSeconds != nil {
		t.Fatalf("measured job before any task finishes: got remaining %d, want null", *m.RemainingSeconds)
	}
	m, _ := waitRemaining(t, ts, measured, queueID, 60*time.Second, "measured job's first success", func(m remainingJobResp, _ remainingQueueResp) bool {
		return m.RemainingSeconds != nil
	})
	if m.RemainingBasis != "measured" {
		t.Fatalf("measured job once a task succeeded: got basis %q (remaining %d), want measured", m.RemainingBasis, *m.RemainingSeconds)
	}
	if final := pollJobStatus(t, ts, measured, []string{"completed", "failed", "canceled"}, 120*time.Second); final != "completed" {
		t.Fatalf("measured job final status: got %q, want completed", final)
	}
	if m = getRemainingJob(t, ts, measured); m.RemainingSeconds == nil || *m.RemainingSeconds != 0 || m.RemainingBasis != "finished" {
		t.Fatalf("finished measured job: got remaining %v basis %q, want 0 finished", derefI64(m.RemainingSeconds), m.RemainingBasis)
	}

	// The totals are opt-in: a plain queue read carries none of them.
	var plain map[string]json.RawMessage
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/queues/"+queueID), nil, "", http.StatusOK, &plain)
	for _, k := range []string{"unfinished_jobs", "work_remaining_seconds", "jobs_without_estimate", "running_tasks", "as_of"} {
		if _, ok := plain[k]; ok {
			t.Errorf("plain queue read carries %q without include=timing", k)
		}
	}

	// A product submission records its product.
	var prod remainingJobResp
	var created struct {
		ID string `json:"id"`
	}
	body := []byte(`{"farm_id":"` + farmID + `","queue_id":"` + queueID + `","parameters":{"Command":"true"}}`)
	mustDoJSON(t, http.MethodPost, apiURL(ts, "/api/v1/products/script/jobs"), body, "application/json", http.StatusCreated, &created)
	prod = getRemainingJob(t, ts, created.ID)
	if prod.ProductName != "script" || prod.ProductVersion == "" {
		t.Fatalf("product job: got product %q version %q, want script and a version", prod.ProductName, prod.ProductVersion)
	}
}
