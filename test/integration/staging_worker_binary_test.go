// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// staging_worker_binary_test.go — end-to-end test of the stage_locally path
// delivery with a real sqi-worker subprocess.
//
// Unlike the mock-worker tests (which only inspect the server-built AssignMsg)
// and the staging unit tests (which drive Stager in isolation), this test runs
// the full path through a real worker: a job declaring SQI_PATH_TRANSLATION with
// swap_in_place + stage_locally is submitted; the worker copies the IN file into
// scratch, runs the task against the scratch paths, and copies the OUT file back
// to its real location. It is a regression guard for the staging bugs that unit
// tests could not catch because nothing exercised stage-in → execute → stage-out
// as one flow.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWorkerBinaryStaging configures a real worker with a scratch dir and a
// cp-based sync command, submits a stage_locally job with an IN and an OUT PATH
// parameter, and asserts the output is copied back to its real path carrying the
// staged input's content — proving stage-in, scratch redirection, and stage-out
// all ran end-to-end.
func TestWorkerBinaryStaging(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	// Operator-owned worker staging config. `cp` (no -R) handles a single FILE
	// cleanly; the test uses FILE params to avoid cp -R directory-nesting quirks.
	scratchDir := t.TempDir()
	stagingCfg := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	cfgBody := "staging:\n  scratch_dir: " + scratchDir + "\n  sync_command: cp {src} {dest}\n"
	if err := os.WriteFile(stagingCfg, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write staging config: %v", err)
	}

	// Start the real worker with the staging config file.
	startRealWorkerWithOptions(t, ts, farmID, queueID, []string{"--config", stagingCfg}, nil)

	// Input file that must be staged into scratch for the job to read it.
	inFile := filepath.Join(t.TempDir(), "scene.txt")
	if err := os.WriteFile(inFile, []byte("hello-staged"), 0o644); err != nil {
		t.Fatalf("write input file: %v", err)
	}

	// Output path the job writes to; it must be redirected into scratch during the
	// run and copied back here afterward. The directory exists; the file does not.
	outFile := filepath.Join(t.TempDir(), "result.txt")

	// The task copies InFile → OutFile. With swap_in_place both are rewritten to
	// their scratch paths, so the job reads the staged input and writes the staged
	// output; stage-out then copies the staged output back to the real OutFile.
	jobYAML := fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: Staging Integration Test
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

	url := fmt.Sprintf("http://%s/api/v1/jobs?farm_id=%s&queue_id=%s&owner=staging-test",
		ts.HTTPAddr, farmID, queueID)
	var jobResp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost, url, []byte(jobYAML), "application/x-yaml",
		http.StatusCreated, &jobResp)
	if jobResp.ID == "" {
		t.Fatal("server returned empty job ID")
	}
	t.Logf("submitted staging job %s", jobResp.ID)

	final := pollJobStatus(t, ts, jobResp.ID,
		[]string{"completed", "failed", "canceled"}, 60*time.Second)
	if final != "completed" {
		t.Fatalf("job final status = %q, want completed", final)
	}

	// The output must exist at its REAL path (copied back from scratch) and carry
	// the staged input's content. If the input were not staged in, the task's read
	// would fail; if the output were not redirected to scratch, copy-out would fail
	// on a missing scratch file and the job would not have completed.
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output not copied back to %q: %v", outFile, err)
	}
	if string(data) != "hello-staged" {
		t.Errorf("output content = %q, want %q", string(data), "hello-staged")
	}
}
