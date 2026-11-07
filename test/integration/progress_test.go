// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// progress_test.go — task progress from a real worker's output, through a
// server restart, to the REST API.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// progressJobYAML is a one-task job that reports a status and a progress
// value once a second for nine seconds, then keeps running for twenty more,
// so a server restart lands while the task is still running.
const progressJobYAML = `specificationVersion: jobtemplate-2023-09
name: progress-e2e
steps:
  - name: work
    script:
      actions:
        onRun:
          command: /bin/sh
          args:
            - "-c"
            - "for n in 1 2 3 4 5 6 7 8 9; do echo \"openjd_status: step $n\"; echo \"openjd_progress: $n.5\"; sleep 1; done; sleep 20"
`

// progressTaskResp is the slice of GET /api/v1/tasks/{id} this test reads.
type progressTaskResp struct {
	Status     string     `json:"status"`
	Progress   *int       `json:"progress"`
	StatusText string     `json:"status_text"`
	ProgressAt *time.Time `json:"progress_at"`
}

// getProgressTask fetches the task's progress fields.
func getProgressTask(t *testing.T, ts *testServer, taskID string) progressTaskResp {
	t.Helper()
	var resp progressTaskResp
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID), nil, "", http.StatusOK, &resp)
	return resp
}

// waitProgress polls the task until ok accepts it, failing after timeout.
func waitProgress(t *testing.T, ts *testServer, taskID string, timeout time.Duration, ok func(progressTaskResp) bool) progressTaskResp {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := getProgressTask(t, ts, taskID)
		if ok(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s after %s: status %q progress %v status_text %q", taskID, timeout, got.Status, deref(got.Progress), got.StatusText)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// deref renders an optional progress for a message; -1 means absent.
func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// TestTaskProgressEndToEnd runs a job whose command prints openjd_progress and
// openjd_status lines on a real worker, and asserts the snapshots reach the
// task and job REST resources, survive a server restart while the task runs,
// and remain on the task once it has succeeded.
func TestTaskProgressEndToEnd(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRealWorker(t, ts, farmID, queueID)

	jobID := submitJobCustomYAML(t, ts, farmID, queueID, progressJobYAML)
	taskID := firstTaskID(t, ts, jobID)

	before := waitProgress(t, ts, taskID, 60*time.Second, func(r progressTaskResp) bool {
		return r.Progress != nil && *r.Progress >= 1 && strings.HasPrefix(r.StatusText, "step")
	})
	if before.Status != "running" {
		t.Fatalf("task status with progress %d: got %q, want running", *before.Progress, before.Status)
	}
	if before.ProgressAt == nil {
		t.Errorf("task reports progress %d with no progress_at", *before.Progress)
	}

	var job struct {
		ProgressPercent *float64 `json:"progress_percent"`
	}
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &job)
	if job.ProgressPercent == nil || *job.ProgressPercent <= 0 || *job.ProgressPercent >= 100 {
		t.Fatalf("job progress_percent with the task at %d: got %v, want strictly between 0 and 100", *before.Progress, job.ProgressPercent)
	}
	t.Logf("before restart: task progress %d %q, job %.1f%%", *before.Progress, before.StatusText, *job.ProgressPercent)

	ts.Stop(t)
	ts = restartServer(t, ts)

	after := getProgressTask(t, ts, taskID)
	if after.Progress == nil || *after.Progress < *before.Progress {
		t.Fatalf("task progress after restart: got %v, want at least %d", deref(after.Progress), *before.Progress)
	}
	if after.StatusText == "" {
		t.Fatalf("task status_text after restart is empty (progress %d)", *after.Progress)
	}
	t.Logf("after restart: task progress %d %q, status %q", *after.Progress, after.StatusText, after.Status)

	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 90*time.Second); final != "completed" {
		t.Fatalf("job final status: got %q, want completed", final)
	}
	done := getProgressTask(t, ts, taskID)
	if done.Status != "succeeded" {
		t.Fatalf("task status: got %q, want succeeded", done.Status)
	}
	if done.Progress == nil || *done.Progress != 9 {
		t.Fatalf("succeeded task progress: got %v, want 9", deref(done.Progress))
	}
}
