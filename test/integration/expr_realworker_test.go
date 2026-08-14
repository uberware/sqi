// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// expr_realworker_test.go — the first EXPR job ever executed by the real
// binaries.
//
// Until EXPR sub-project H2 flipped the extension to StatusSupported, no
// template declaring extensions: [EXPR] could be submitted at all: the HTTP
// gate rejected it at /extensions/0, so every EXPR test in the repo ran
// in-process against the packages directly. This test closes that: one job,
// through a real sqi-server and a real sqi-worker subprocess, asserting on the
// VALUE an expression resolved to rather than merely on the job succeeding.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// exprWorkerJobYAML is an EXPR job whose onRun args cannot be resolved before
// the worker runs them, so a passing assertion is proof that phase-3 evaluation
// happened in the worker process against the assignment the server sent.
//
// Each of the three args is chosen for a different reason:
//
//   - "expr-phase3" is a plain literal, present so a log match cannot come from
//     an unrelated line.
//
//   - "{{ upper(Param.Shot) + '.' + zfill(Task.Param.Frame * 3, 4) }}" resolves
//     to "SQ010.0021". Task.Param.Frame is the one symbol the SERVER cannot
//     substitute: the assignment carries the task's parameter values and types
//     (protocol.AssignMsg.Parameters/ParameterTypes) and the raw, unresolved
//     action, and internal/worker/fmtres builds the concrete symbol table from
//     them. It also exercises two library functions and both string and integer
//     arithmetic, so the resolved text pins real evaluation rather than a
//     lookup.
//
//   - "{{ ['workdir-absolute', string(is_absolute(Session.WorkingDirectory))] }}"
//     is a LONE reference producing a list[string], which section 1.3.2's
//     list-item rule flattens into TWO command-line arguments — behavior that
//     exists only in the worker's renderer (internal/worker/fmtres's
//     TargetArgItem), since a type check discards the value. Session.WorkingDirectory
//     is a host fact that does not exist at submission time in any form, and
//     is_absolute() forces it through the path engine on the worker's own
//     native flavor. echo joins the two flattened args with a space, so the
//     expected text is "workdir-absolute true".
//
// The full expected line is therefore:
//
//	expr-phase3 SQ010.0021 workdir-absolute true
const exprWorkerJobYAML = `specificationVersion: "jobtemplate-2023-09"
extensions:
  - EXPR
name: EXPR Real Worker Integration Test
parameterDefinitions:
  - name: Shot
    type: STRING
    default: "sq010"
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "7"
    script:
      actions:
        onRun:
          command: echo
          args:
            - "expr-phase3"
            - "{{ upper(Param.Shot) + '.' + zfill(Task.Param.Frame * 3, 4) }}"
            - "{{ ['workdir-absolute', string(is_absolute(Session.WorkingDirectory))] }}"
`

// exprWorkerWantOutput is the exact text the resolved command line must echo.
// Asserting on the RESOLVED VALUE — not on the job reaching "completed" — is
// what makes this test meaningful: a template whose expressions were somehow
// dropped, or resolved to the wrong thing, would still complete.
const exprWorkerWantOutput = "expr-phase3 SQ010.0021 workdir-absolute true"

// TestEXPRJobEndToEnd submits an extensions: [EXPR] job to a real sqi-server,
// lets a real sqi-worker subprocess execute it, and asserts the worker resolved
// the step's expressions at phase 3 to the expected concrete text.
func TestEXPRJobEndToEnd(t *testing.T) {
	ts := startServer(t)

	farmID, queueID := seedFarmAndQueue(t, ts)
	workerID := startRealWorker(t, ts, farmID, queueID)

	url := fmt.Sprintf("http://%s/api/v1/jobs?farm_id=%s&queue_id=%s&owner=expr-test",
		ts.HTTPAddr, farmID, queueID)
	var jobResp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost, url,
		[]byte(exprWorkerJobYAML), "application/x-yaml",
		http.StatusCreated, &jobResp)
	if jobResp.ID == "" {
		t.Fatal("server returned empty job ID")
	}
	t.Logf("submitted EXPR job %s to farm %s queue %s", jobResp.ID, farmID, queueID)

	finalStatus := pollJobStatus(t, ts, jobResp.ID,
		[]string{"completed", "failed", "canceled"}, 60*time.Second)
	if finalStatus != "completed" {
		t.Errorf("job final status: got %q, want %q", finalStatus, "completed")
	}

	taskID := firstTaskID(t, ts, jobResp.ID)
	logs := pollTaskLogs(t, ts, taskID, exprWorkerWantOutput, 15*time.Second)
	if !strings.Contains(logs, exprWorkerWantOutput) {
		t.Errorf("phase-3 resolution: expected log output to contain %q; got: %q",
			exprWorkerWantOutput, logs)
	}

	var taskDetail struct {
		Status           string `json:"status"`
		AssignedWorkerID string `json:"assigned_worker_id"`
	}
	mustDoJSON(t, http.MethodGet,
		"http://"+ts.HTTPAddr+"/api/v1/tasks/"+taskID, nil, "",
		http.StatusOK, &taskDetail)

	if taskDetail.AssignedWorkerID != workerID {
		t.Errorf("task.assigned_worker_id: got %q, want %q",
			taskDetail.AssignedWorkerID, workerID)
	}
	if taskDetail.Status != "succeeded" {
		t.Errorf("task.status: got %q, want %q", taskDetail.Status, "succeeded")
	}
}
