// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// failure_reason_realworker_test.go: task failure-reason visibility with a real
// worker. TestTaskFailureReason_VisibleEndToEnd uses the mock worker and
// supplies the failure message itself, so it covers only the
// server→store→REST path. These tests run a real sqi-worker subprocess with no
// staging configured and submit a job that requests the stage_locally path
// delivery. The worker's failPreExec path (internal/worker/executor/run.go
// buildEffectiveLookup → "worker not configured for staging …") fails the
// task before it runs, and that reason must reach the REST API.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorkerBinaryStagingFailureReason starts a real worker with no staging
// config, submits a stage_locally job with max_attempts=1 (so the one
// pre-exec failure is terminal), and asserts:
//
//   - the task ends "failed" with failure_reason mentioning "staging" (the
//     worker's real failPreExec message), and
//   - the job detail's failure_summary reflects it as the dominant reason with
//     failed_count >= 1.
func TestWorkerBinaryStagingFailureReason(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// staging.defaults is off: with it on, an unconfigured worker stages with
	// the built-in copy and a TEMP scratch directory, and never reaches the
	// failPreExec path.
	stagingCfg := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(stagingCfg, []byte("staging:\n  defaults: false\n"), 0o600); err != nil {
		t.Fatalf("write staging config: %v", err)
	}
	startRealWorkerWithOptions(t, ts, farmID, queueID, []string{"--config", stagingCfg}, nil)

	// A job requesting the stage_locally delivery. The command never runs: the
	// worker fails the task before it runs because staging is unconfigured. A
	// PATH parameter is present so the SQI_PATH_TRANSLATION block is
	// well-formed.
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

	// GET /api/v1/tasks/{id}/attempts must surface the same staging failure:
	// at least one recorded attempt is "failed" with a message mentioning
	// "staging". internal/api/tasks_test.go covers this endpoint against the
	// fake store only.
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

// TestWorkerBinaryStagingFailureReason_MultipleAttempts is the multi-attempt
// sibling of TestWorkerBinaryStagingFailureReason: same real worker with no
// staging configured and the same stage_locally job, but with max_attempts=2
// and retry_delay_seconds=0, so the task's first failure is retried
// immediately (RequeueTaskForRetry; see TestAutoRetry_RetryThenSucceed),
// fails a second time, and goes terminal "failed" once max_attempts is
// exhausted. The attempt-history endpoint must then list two attempts in
// order.
func TestWorkerBinaryStagingFailureReason_MultipleAttempts(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// staging.defaults is off; see TestWorkerBinaryStagingFailureReason.
	stagingCfg := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(stagingCfg, []byte("staging:\n  defaults: false\n"), 0o600); err != nil {
		t.Fatalf("write staging config: %v", err)
	}
	startRealWorkerWithOptions(t, ts, farmID, queueID, []string{"--config", stagingCfg}, nil)

	// Same stage_locally job as TestWorkerBinaryStagingFailureReason; the
	// command never runs.
	const jobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Staging Failure Reason Test (multi-attempt)
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
		"max_attempts":        "2",
		"retry_delay_seconds": "0",
	})
	t.Logf("submitted multi-attempt staging-failure job %s", jobID)

	// A terminal job status means both attempts have finished: max_attempts=2
	// keeps the task non-terminal after the first failure.
	final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled", "paused"}, 60*time.Second)
	if final != "failed" {
		t.Fatalf("job final status = %q, want failed", final)
	}

	taskID := firstTaskID(t, ts, jobID)
	task := getTaskFailure(t, ts, taskID)
	if task.Status != "failed" {
		t.Errorf("task status = %q, want failed", task.Status)
	}

	attempts := getTaskAttempts(t, ts, taskID)
	if len(attempts.Items) < 2 {
		t.Fatalf("task attempts: got %d items, want >= 2", len(attempts.Items))
	}
	for i, a := range attempts.Items {
		if a.Status != "failed" {
			t.Errorf("task attempts[%d]: status = %q, want failed (item: %+v)", i, a.Status, a)
		}
		if !strings.Contains(a.Message, "staging") {
			t.Errorf("task attempts[%d]: message = %q, want it to mention %q (item: %+v)", i, a.Message, "staging", a)
		}
	}
	for i := 1; i < len(attempts.Items); i++ {
		if attempts.Items[i-1].AttemptNumber >= attempts.Items[i].AttemptNumber {
			t.Errorf("task attempts: attempt_number not strictly increasing at index %d: %d >= %d (items: %+v)",
				i, attempts.Items[i-1].AttemptNumber, attempts.Items[i].AttemptNumber, attempts.Items)
		}
	}
}

// TestWorkerBinaryStagingDefault_Succeeds is the positive counterpart to
// TestWorkerBinaryStagingFailureReason: it starts a real worker with no
// staging config at all (staging.defaults defaults to true), submits the
// same stage_locally shape of job as TestWorkerBinaryStaging (an IN and an
// OUT PATH parameter), and asserts the task succeeds with the OUT file
// copied back to its real path, using the built-in copy and a TEMP scratch
// directory.
func TestWorkerBinaryStagingDefault_Succeeds(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	startRealWorker(t, ts, farmID, queueID)

	// Input file that must be staged into scratch for the job to read it.
	inFile := filepath.Join(t.TempDir(), "scene.txt")
	if err := os.WriteFile(inFile, []byte("hello-default-staged"), 0o644); err != nil {
		t.Fatalf("write input file: %v", err)
	}

	// Output path the job writes to; it must be redirected into scratch during
	// the run and copied back here afterward. The directory exists; the file
	// does not.
	outFile := filepath.Join(t.TempDir(), "result.txt")

	// The task copies InFile → OutFile. With swap_in_place both are rewritten
	// to their scratch paths, so the job reads the staged input and writes the
	// staged output; stage-out then copies the staged output back to the real
	// OutFile.
	jobYAML := fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: Staging Default Success Test
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries:
    - swap_in_place
    - stage_locally
parameterDefinitions:
  - name: InFile
    type: PATH
    objectType: FILE
    dataFlow: IN
    default: %q
  - name: OutFile
    type: PATH
    objectType: FILE
    dataFlow: OUT
    default: %q
steps:
  - name: Copy
    script:
      actions:
        onRun:
          command: bash
          args:
            - "-c"
            - 'cat "{{Param.InFile}}" > "{{Param.OutFile}}"'
`, inFile, outFile)

	jobID := submitJobWithParams(t, ts, farmID, queueID, jobYAML, map[string]string{
		"max_attempts":        "1",
		"retry_delay_seconds": "0",
	})
	t.Logf("submitted default-staging-success job %s", jobID)

	final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled", "paused"}, 60*time.Second)
	if final != "completed" {
		t.Fatalf("job final status = %q, want completed", final)
	}

	taskID := firstTaskID(t, ts, jobID)
	task := getTaskFailure(t, ts, taskID)
	if task.Status != "succeeded" {
		t.Errorf("task status = %q, want succeeded", task.Status)
	}

	// The output must exist at its real path, copied back from scratch, and
	// carry the staged input's content.
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output not copied back to %q: %v", outFile, err)
	}
	if string(data) != "hello-default-staged" {
		t.Errorf("output content = %q, want %q", string(data), "hello-default-staged")
	}
}
