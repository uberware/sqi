// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// failure_reason_realworker_test.go — the origin-bug regression guard for the
// failure-reason-visibility feature. Unlike TestTaskFailureReason_VisibleEndToEnd
// (which uses the mock worker and fabricates the failure Message, proving only
// the server→store→REST transport), this test runs a REAL sqi-worker subprocess
// with NO staging configured and submits a job that requests the stage_locally
// path delivery. The worker's real failPreExec path (internal/worker/executor/
// run.go buildEffectiveLookup → "worker not configured for staging …") is the
// exact condition that originally failed with no visible reason. This test
// asserts that reason now surfaces all the way through to the REST API.

import (
	"strings"
	"testing"
	"time"
)

// TestWorkerBinaryStagingFailureReason starts a real worker with no staging
// config, submits a stage_locally job with max_attempts=1 (so the single
// pre-exec failure is immediately terminal), and asserts:
//
//   - the task ends "failed" with failure_reason mentioning "staging" (the
//     worker's real failPreExec message), and
//   - the job detail's failure_summary reflects it as the dominant reason with
//     failed_count >= 1.
func TestWorkerBinaryStagingFailureReason(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// Start the real worker WITHOUT any staging config — stage_locally must fail
	// pre-exec with an explicit reason.
	startRealWorker(t, ts, farmID, queueID)

	// A job requesting the stage_locally delivery. The command is never reached:
	// the worker fails during pre-exec because staging is unconfigured. A PATH
	// parameter is present so the SQI_PATH_TRANSLATION block is well-formed.
	const jobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Staging Failure Reason Test
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries:
    - stage_locally
parameterDefinitions:
  - name: InFile
    type: PATH
    objectType: FILE
    dataFlow: IN
    default: "/tmp/sqi-nonexistent-input.txt"
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: bash
          args:
            - "-c"
            - 'cat "{{Param.InFile}}"'
`

	jobID := submitJobWithParams(t, ts, farmID, queueID, jobYAML, map[string]string{
		"max_attempts":        "1",
		"retry_delay_seconds": "0",
	})
	t.Logf("submitted staging-failure job %s", jobID)

	final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled", "paused"}, 60*time.Second)
	if final != "failed" {
		t.Fatalf("job final status = %q, want failed", final)
	}

	// Find the (single) task and assert its surfaced failure reason.
	taskID := firstTaskID(t, ts, jobID)
	task := getTaskFailure(t, ts, taskID)
	if task.Status != "failed" {
		t.Errorf("task status = %q, want failed", task.Status)
	}
	if !strings.Contains(task.FailureReason, "staging") {
		t.Errorf("task failure_reason = %q, want it to mention %q", task.FailureReason, "staging")
	}

	job := getJobFailureDetail(t, ts, jobID)
	if job.FailureSummary == nil {
		t.Fatal("job failure_summary: got nil, want non-nil")
	}
	if job.FailureSummary.FailedCount < 1 {
		t.Errorf("job failure_summary.failed_count = %d, want >= 1", job.FailureSummary.FailedCount)
	}
	if !strings.Contains(job.FailureSummary.DominantReason, "staging") {
		t.Errorf("job failure_summary.dominant_reason = %q, want it to mention %q",
			job.FailureSummary.DominantReason, "staging")
	}

	// The attempt-history endpoint must independently surface the same
	// staging failure: at least one recorded attempt is "failed" with a
	// message mentioning "staging". This exercises GET
	// /api/v1/tasks/{id}/attempts end-to-end against a real worker's
	// failPreExec path, not just the fake-worker/API-level coverage in
	// internal/api/tasks_test.go.
	attempts := getTaskAttempts(t, ts, taskID)
	if len(attempts.Items) < 1 {
		t.Fatalf("task attempts: got %d items, want >= 1", len(attempts.Items))
	}
	found := false
	for _, a := range attempts.Items {
		if a.Status == "failed" && strings.Contains(a.Message, "staging") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("task attempts: no failed attempt with a message mentioning %q, got %+v", "staging", attempts.Items)
	}
}
