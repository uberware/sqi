// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// logChecksTemplateJSON declares SQI_LOG_CHECKS and carries a block on its one
// step, so a job built from it needs a worker that enforces the checks.
const logChecksTemplateJSON = `{
  "specificationVersion": "jobtemplate-2023-09",
  "extensions": ["SQI_LOG_CHECKS"],
  "name": "j",
  "steps": [
    {
      "name": "render",
      "SQI_LOG_CHECKS": { "failOn": ["ERROR", "Traceback"], "requireLine": ["SQI_RENDER: OK"] },
      "script": { "actions": { "onRun": { "command": "render" } } }
    }
  ]
}`

// TestProtocolAndRegistryAgreeOnWorkerEnforcedExtensions pins that the name a
// worker advertises (the protocol constant, the only one internal/worker may
// import) is exactly the set the server's registry says a worker must enforce.
// A rename on one side alone would make every worker look unsupporting, or
// every checked job look ungated.
func TestProtocolAndRegistryAgreeOnWorkerEnforcedExtensions(t *testing.T) {
	want := []string{protocol.ExtensionLogChecks}
	if got := openjd.WorkerEnforcedExtensions(); !slices.Equal(got, want) {
		t.Fatalf("openjd.WorkerEnforcedExtensions() = %v, want %v", got, want)
	}
}

func TestWorkerExtensionBlock(t *testing.T) {
	const reason = "worker does not enforce SQI_LOG_CHECKS; upgrade sqi-worker"
	tests := []struct {
		name      string
		worker    []string
		declared  []string
		recorded  bool
		wantBlock string
	}{
		{"declared, worker advertises nothing", nil, []string{"SQI_LOG_CHECKS"}, true, reason},
		{"declared, worker advertises empty", []string{}, []string{"SQI_LOG_CHECKS"}, true, reason},
		{"declared, worker advertises it", []string{"SQI_LOG_CHECKS"}, []string{"SQI_LOG_CHECKS"}, true, ""},
		{"recorded and empty", nil, []string{}, true, ""},
		{"declares only another extension", nil, []string{"EXPR"}, true, ""},
		{"legacy row, nothing recorded", nil, nil, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := store.Worker{Extensions: tc.worker}
			j := store.Job{DeclaredExtensions: tc.declared, ExtensionsRecorded: tc.recorded}
			if got := workerExtensionBlock(w, j); got != tc.wantBlock {
				t.Fatalf("workerExtensionBlock = %q, want %q", got, tc.wantBlock)
			}
		})
	}
}

// seedLogChecksFixture seeds one ready task of a job submitted through the real
// path from logChecksTemplateJSON, plus one online worker advertising ext.
func seedLogChecksFixture(t *testing.T, st store.Store, ext []string) (store.Worker, string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatal(err)
	}
	w, _, err := st.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		CPUCount: 4, LastHeartbeatAt: &now, Tags: map[string]string{}, Extensions: ext,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := openjd.NewSubmitter(st).Submit(ctx, logChecksTemplateJSON, store.TemplateFormatJSON, openjd.SubmitOptions{
		FarmID: "f1", QueueID: "q1", Owner: "alice",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return w, res.Tasks[0].ID
}

func TestLease_WorkerWithoutLogChecksIsNotOfferedCheckedWork(t *testing.T) {
	st := newCheckedFake(t)
	w, taskID := seedLogChecksFixture(t, st, nil)
	s := newMetricsScheduler(st, &recordBus{}, "f1")

	if got := leaseOnce(t, s, w); got != 0 {
		t.Fatalf("leased %d assignments of a SQI_LOG_CHECKS job to a worker that advertises no "+
			"extensions; want 0", got)
	}
	if got := mustTask(t, st, taskID).Status; got != store.TaskStatusReady {
		t.Fatalf("task status = %q, want it left ready for a capable worker", got)
	}
}

func TestLease_WorkerWithLogChecksIsOfferedCheckedWork(t *testing.T) {
	st := newCheckedFake(t)
	w, _ := seedLogChecksFixture(t, st, []string{protocol.ExtensionLogChecks})
	s := newMetricsScheduler(st, &recordBus{}, "f1")

	batch, err := s.selectLeaseBatch(t.Context(), w)
	if err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("leased %d assignments, want 1", len(batch))
	}
	var msg protocol.AssignMsg
	if err := json.Unmarshal(batch[0], &msg); err != nil {
		t.Fatal(err)
	}
	if msg.LogChecks == nil || !slices.Equal(msg.LogChecks.RequireLine, []string{"SQI_RENDER: OK"}) {
		t.Fatalf("assignment LogChecks = %+v, want the step's lists", msg.LogChecks)
	}
}

func TestUnschedulable_NamesTheExtensionNoOnlineWorkerEnforces(t *testing.T) {
	st := newCheckedFake(t)
	w, taskID := seedLogChecksFixture(t, st, nil)
	s := newMetricsScheduler(st, &recordBus{}, "f1")

	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{w})
	got := mustTask(t, st, taskID).UnschedulableReason
	if !strings.Contains(got, "no eligible online worker:") || !strings.Contains(got, "SQI_LOG_CHECKS") {
		t.Fatalf("UnschedulableReason = %q, want it to contain %q and name SQI_LOG_CHECKS",
			got, "no eligible online worker:")
	}

	// A worker that does enforce it makes the task schedulable again.
	capable := w
	capable.Extensions = []string{protocol.ExtensionLogChecks}
	s.reconcileTaskSchedulability(t.Context(), mustTask(t, st, taskID), []store.Worker{capable})
	if got := mustTask(t, st, taskID).UnschedulableReason; got != "" {
		t.Fatalf("UnschedulableReason = %q after a capable worker came online, want it cleared", got)
	}
}

// ── buildAssignPayload carries the block ─────────────────────────────────────

func TestBuildAssignPayload_CarriesLogChecks(t *testing.T) {
	st := newCheckedFake(t)

	task, worker, job, step, queue := buildFixture(t, logChecksTemplateJSON, store.TemplateFormatJSON, "render")
	data, err := buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}
	var msg protocol.AssignMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	want := &protocol.LogChecks{
		FailOn:      []string{"ERROR", "Traceback"},
		RequireLine: []string{"SQI_RENDER: OK"},
	}
	if msg.LogChecks == nil ||
		!slices.Equal(msg.LogChecks.FailOn, want.FailOn) ||
		!slices.Equal(msg.LogChecks.RequireLine, want.RequireLine) {
		t.Fatalf("LogChecks = %+v, want %+v", msg.LogChecks, want)
	}

	// A template without the block yields no key at all, so a base-spec
	// assignment's wire bytes are unchanged.
	task, worker, job, step, queue = buildFixture(t, minimalJobJSON, store.TemplateFormatJSON, "Render")
	data, err = buildAssignPayload(t.Context(), task, worker, job, step, queue, uuid.NewString(), st)
	if err != nil {
		t.Fatalf("buildAssignPayload: %v", err)
	}
	if strings.Contains(string(data), "log_checks") {
		t.Fatalf("wire bytes of an unchecked assignment contain log_checks: %s", data)
	}
	var plain protocol.AssignMsg
	if err := json.Unmarshal(data, &plain); err != nil {
		t.Fatal(err)
	}
	if plain.LogChecks != nil {
		t.Fatalf("LogChecks = %+v for a template with no block, want nil", plain.LogChecks)
	}
}
