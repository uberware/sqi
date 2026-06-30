// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// pathTranslationJobYAML is an OpenJD template that declares the
// SQI_PATH_TRANSLATION extension with swap_in_place, command_flags, and
// stage_locally deliveries.  It also declares a PATH parameter with dataFlow IN
// so the scheduler builds a non-empty staging manifest.
const pathTranslationJobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Path Translation Integration Test
extensions: [ SQI_PATH_TRANSLATION ]
SQI_PATH_TRANSLATION:
  deliveries:
    - swap_in_place
    - command_flags: { pattern: "--remap {src}={dest}" }
    - stage_locally
parameterDefinitions:
  - name: ShotFile
    type: PATH
    dataFlow: IN
    objectType: FILE
    default: "/projects/shot.ma"
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args:
            - "path translation integration test"
`

// submitJobCustomYAML posts a raw YAML template to POST /api/v1/jobs and
// returns the created job ID.
func submitJobCustomYAML(t *testing.T, ts *testServer, farmID, queueID string, yamlBody string) string {
	t.Helper()
	url := fmt.Sprintf("%s?farm_id=%s&queue_id=%s&owner=test",
		apiURL(ts, "/api/v1/jobs"), farmID, queueID)
	var resp jobResp
	mustDoJSON(t, http.MethodPost, url, []byte(yamlBody), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		t.Fatal("submitJobCustomYAML: server returned empty job ID")
	}
	return resp.ID
}

// TestPathTranslation asserts that the server builds the correct PathDeliveries
// and Staging manifest in the AssignMsg for jobs that declare SQI_PATH_TRANSLATION,
// and uses the implicit default deliveries for jobs that do not.
func TestPathTranslation(t *testing.T) {
	t.Run("WithExtension", func(t *testing.T) {
		ts := startServer(t)
		farmID, queueID := seedFarmAndQueue(t, ts)

		const workerID = "worker-path-trans-001"
		natsURL := "nats://" + ts.NATSAddr
		worker := newMockWorker(t, natsURL, workerID, farmID, queueID)
		worker.register()
		worker.startHeartbeat(5 * time.Second)
		pollWorkerOnline(t, ts, workerID, 10*time.Second)

		jobID := submitJobCustomYAML(t, ts, farmID, queueID, pathTranslationJobYAML)
		t.Logf("submitted path-translation job %s", jobID)

		assign := worker.pullAssignment(15 * time.Second)
		t.Logf("pulled assignment: task=%s attempt=%s", assign.TaskID, assign.AttemptID)

		if assign.JobID != jobID {
			t.Errorf("assignment job ID: got %q, want %q", assign.JobID, jobID)
		}

		// ── Assert PathDeliveries ─────────────────────────────────────────────
		// Expect exactly 3 deliveries: swap_in_place, command_flags, stage_locally.
		if len(assign.PathDeliveries) != 3 {
			t.Fatalf("PathDeliveries: got %d entries, want 3: %+v", len(assign.PathDeliveries), assign.PathDeliveries)
		}
		kindAt := func(i int) string { return assign.PathDeliveries[i].Kind }
		if kindAt(0) != "swap_in_place" {
			t.Errorf("PathDeliveries[0].Kind = %q, want %q", kindAt(0), "swap_in_place")
		}
		if kindAt(1) != "command_flags" {
			t.Errorf("PathDeliveries[1].Kind = %q, want %q", kindAt(1), "command_flags")
		}
		if assign.PathDeliveries[1].Pattern != "--remap {src}={dest}" {
			t.Errorf("PathDeliveries[1].Pattern = %q, want %q",
				assign.PathDeliveries[1].Pattern, "--remap {src}={dest}")
		}
		if kindAt(2) != "stage_locally" {
			t.Errorf("PathDeliveries[2].Kind = %q, want %q", kindAt(2), "stage_locally")
		}

		// ── Assert Staging ────────────────────────────────────────────────────
		// Expect exactly 1 staging entry for the IN PATH parameter default value.
		if len(assign.Staging) != 1 {
			t.Fatalf("Staging: got %d entries, want 1: %+v", len(assign.Staging), assign.Staging)
		}
		if assign.Staging[0].Direction != "IN" {
			t.Errorf("Staging[0].Direction = %q, want %q", assign.Staging[0].Direction, "IN")
		}
		if assign.Staging[0].Path != "/projects/shot.ma" {
			t.Errorf("Staging[0].Path = %q, want %q", assign.Staging[0].Path, "/projects/shot.ma")
		}
	})

	t.Run("DefaultDeliveries", func(t *testing.T) {
		ts := startServer(t)
		farmID, queueID := seedFarmAndQueue(t, ts)

		const workerID = "worker-path-trans-002"
		natsURL := "nats://" + ts.NATSAddr
		worker := newMockWorker(t, natsURL, workerID, farmID, queueID)
		worker.register()
		worker.startHeartbeat(5 * time.Second)
		pollWorkerOnline(t, ts, workerID, 10*time.Second)

		// Submit the minimal job (no SQI_PATH_TRANSLATION extension).
		jobID := submitJob(t, ts, farmID, queueID)
		t.Logf("submitted minimal job %s", jobID)

		assign := worker.pullAssignment(15 * time.Second)
		t.Logf("pulled assignment: task=%s attempt=%s", assign.TaskID, assign.AttemptID)

		if assign.JobID != jobID {
			t.Errorf("assignment job ID: got %q, want %q", assign.JobID, jobID)
		}

		// ── Assert PathDeliveries defaults ────────────────────────────────────
		// Without the extension the implicit defaults are [swap_in_place, translation_file].
		if len(assign.PathDeliveries) != 2 {
			t.Fatalf("PathDeliveries: got %d entries, want 2: %+v", len(assign.PathDeliveries), assign.PathDeliveries)
		}
		if assign.PathDeliveries[0].Kind != "swap_in_place" {
			t.Errorf("PathDeliveries[0].Kind = %q, want %q", assign.PathDeliveries[0].Kind, "swap_in_place")
		}
		if assign.PathDeliveries[1].Kind != "translation_file" {
			t.Errorf("PathDeliveries[1].Kind = %q, want %q", assign.PathDeliveries[1].Kind, "translation_file")
		}

		// ── Assert Staging is absent ──────────────────────────────────────────
		if len(assign.Staging) != 0 {
			t.Errorf("Staging: got %d entries, want 0: %+v", len(assign.Staging), assign.Staging)
		}
	})
}
